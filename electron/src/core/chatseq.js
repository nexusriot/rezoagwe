'use strict';

const { MAX_CHAT_PULL } = require('../proto/wire');

/**
 * A chat broadcast is one datagram per peer and nothing re-sends it.
 * Anti-entropy repairs the KV store because every entry carries a version a
 * digest can compare; a chat line carried nothing, so a dropped one was not
 * merely unrecoverable — it was undetectable. The sender's log showed it sent,
 * every receiver's log simply never had it, and no counter anywhere moved.
 *
 * A per-author sequence is the smallest thing that changes that: it makes a
 * gap nameable, which makes it askable-for, and makes a re-delivery
 * recognisable instead of a second copy of the same line.
 *
 * A port of the Go `chatTracker`; the rules are protocol and must match.
 */

/** Bounds the per-author gap set against a badly unreachable peer. */
const MAX_TRACKED_GAPS = 512;

/** How many times a gap is asked for before it is declared lost. */
const MAX_PULL_ATTEMPTS = 5;

/**
 * Bounds how many authors are followed at once. Every heartbeat from every
 * peer names an author, and a node that restarts without a state file comes
 * back with a *new* id, so the set grows with cluster churn and nothing in the
 * protocol ever shrinks it. The gaps inside each author were bounded from the
 * start; the authors were not.
 */
const MAX_TRACKED_AUTHORS = 64;

/** What a receiver should do with a message it just decoded. */
const ChatOutcome = Object.freeze({
  /** Not seen before. Append it. */
  ACCEPT: 'accept',
  /** Had been missing. Append it; the gap closes. */
  FILLED_GAP: 'filled_gap',
  /**
   * Already held. Dropping it is the point: without a sequence a re-delivery —
   * the repair path's whole job — would append a second copy of a line already
   * on screen.
   */
  DUPLICATE: 'duplicate',
});

/**
 * Follows every author's sequence, independently of the engine, so the rules
 * are testable without a network.
 *
 * Authors are keyed by **node id**, not address: the id is persistent and the
 * address is not, so a node that moves ports stays one author — and a node
 * that restarts with no state file gets a fresh id and is correctly treated as
 * a new one, rather than appearing to rewind its whole conversation.
 */
class ChatTracker {
  constructor() {
    /** @type {Map<string, {addr: string, last: number, missing: Map<number, number>, lost: number, recovered: number, touched: number}>} */
    this.senders = new Map();
    // Orders activity without depending on wall time, which a peer can be
    // wrong about.
    this.clock = 0;
  }

  /**
   * Marks an author as recently active and trims the set if it has outgrown
   * the bound. The least recently active goes: dropping it is safe, because an
   * author heard from again is simply treated as one newly met, which claims
   * nothing.
   */
  #touch(st) {
    st.touched = ++this.clock;
    if (this.senders.size <= MAX_TRACKED_AUTHORS) return;
    let oldest = null;
    let oldestAt = Infinity;
    for (const [id, s] of this.senders) {
      if (s.touched < oldestAt) { oldest = id; oldestAt = s.touched; }
    }
    if (oldest !== null) this.senders.delete(oldest);
  }

  /**
   * Records one received broadcast and says what to do with it.
   *
   * An unsequenced message (seq 0 — a direct message, or a peer older than the
   * field) is always accepted and never gap-checked: claiming a gap against a
   * sender that does not number its lines would invent losses on every hop.
   */
  observe(m) {
    if (!m.seq) return ChatOutcome.ACCEPT;
    const author = m.node || m.sender;
    if (!author) return ChatOutcome.ACCEPT;

    const st = this.senders.get(author);
    if (!st) {
      // First line ever heard from this author. Whatever came before belongs
      // to a conversation this node was not in, so it is not a gap: a joiner
      // that reported every earlier line as lost would cry wolf on every join.
      const fresh = { addr: m.sender, last: m.seq, missing: new Map(), lost: 0, recovered: 0, touched: 0 };
      this.senders.set(author, fresh);
      this.#touch(fresh);
      return ChatOutcome.ACCEPT;
    }
    if (m.sender) st.addr = m.sender;
    this.#touch(st);

    if (m.seq === st.last + 1) {
      st.last = m.seq;
      return ChatOutcome.ACCEPT;
    }
    if (m.seq > st.last + 1) {
      for (let s = st.last + 1; s < m.seq; s++) st.missing.set(s, 0);
      st.last = m.seq;
      this.#trim(st);
      return ChatOutcome.ACCEPT;
    }

    // At or below the high-water mark: either the gap we asked for, or a line
    // already held.
    if (st.missing.has(m.seq)) {
      st.missing.delete(m.seq);
      st.recovered++;
      return ChatOutcome.FILLED_GAP;
    }
    return ChatOutcome.DUPLICATE;
  }

  /**
   * Keeps the gap set bounded by dropping the oldest, which are the least
   * likely to still be in the author's ring. They are counted as lost rather
   * than forgotten silently.
   */
  #trim(st) {
    if (st.missing.size <= MAX_TRACKED_GAPS) return;
    const drop = [...st.missing.keys()].sort((a, b) => a - b).slice(0, st.missing.size - MAX_TRACKED_GAPS);
    for (const s of drop) {
      st.missing.delete(s);
      st.lost++;
    }
  }

  /**
   * Records what a bulk delivery of history already contains, without claiming
   * anything is missing.
   *
   * Two paths put lines into the log without them arriving one at a time: the
   * state file at startup, and a state-sync snapshot on join. The startup case
   * is the one that matters — a node holding alice's lines 1..5 restarts,
   * alice says 6 while it is down, and a fresh tracker treats 7 as the first
   * line it has ever seen from her. Line 6 is then lost in silence, in exactly
   * the situation where a line is most likely to have been missed: the node
   * was not running.
   *
   * It only closes gaps and raises the high-water mark, never invents one: a
   * snapshot is a bounded slice of someone else's ring, so what it leaves out
   * is not evidence of a loss.
   */
  seed(entries) {
    for (const e of entries || []) {
      if (!e.seq) continue;
      const author = e.node || e.sender;
      if (!author) continue;
      const st = this.senders.get(author);
      if (!st) {
        const fresh = { addr: e.sender, last: e.seq, missing: new Map(), lost: 0, recovered: 0, touched: 0 };
        this.senders.set(author, fresh);
        this.#touch(fresh);
        continue;
      }
      if (e.sender && !st.addr) st.addr = e.sender;
      if (st.missing.has(e.seq)) {
        st.missing.delete(e.seq);
        st.recovered++;
      }
      if (e.seq > st.last) st.last = e.seq;
    }
  }

  /**
   * Records how far an author says its own numbering has got, from a heartbeat
   * rather than from a line it spoke.
   *
   * Seeding only from the first line an author is *heard* to say conflates a
   * node that was not there when the earlier lines were said, which has
   * nothing to report, with one that was there the whole time and simply did
   * not hear them, which has lost them. Measured on a real cluster: a peer
   * that had never spoken said three lines while one node was off the air, and
   * that node reported nothing.
   *
   * The first announcement only sets the mark — seq 0 included, which is a
   * peer saying it has said nothing yet and is the most useful baseline there
   * is. Later ones name everything in between.
   */
  announce(author, addr, seq) {
    if (!author) return;
    const st = this.senders.get(author);
    if (!st) {
      const fresh = { addr, last: seq || 0, missing: new Map(), lost: 0, recovered: 0, touched: 0 };
      this.senders.set(author, fresh);
      this.#touch(fresh);
      return;
    }
    if (addr) st.addr = addr;
    this.#touch(st);
    if (!seq || seq <= st.last) return;
    for (let s = st.last + 1; s <= seq; s++) st.missing.set(s, 0);
    st.last = seq;
    this.#trim(st);
  }

  /**
   * Lists the requests worth making now, charging an attempt to each gap it
   * names. A gap asked for MAX_PULL_ATTEMPTS times without ever being answered
   * is given up on here — an author that is simply gone would otherwise be
   * asked for the same line on every tick forever.
   */
  pullPlans() {
    const plans = [];
    for (const st of this.senders.values()) {
      if (!st.addr || st.missing.size === 0) continue;
      const seqs = [];
      for (const [s, attempts] of [...st.missing.entries()]) {
        if (attempts >= MAX_PULL_ATTEMPTS) {
          st.missing.delete(s);
          st.lost++;
        } else {
          seqs.push(s);
        }
      }
      if (seqs.length === 0) continue;
      seqs.sort((a, b) => a - b);
      const asking = seqs.slice(0, MAX_CHAT_PULL);
      for (const s of asking) st.missing.set(s, (st.missing.get(s) || 0) + 1);
      plans.push({ addr: st.addr, seqs: asking });
    }
    return plans.sort((a, b) => (a.addr < b.addr ? -1 : a.addr > b.addr ? 1 : 0));
  }

  /**
   * Whether this node is waiting for exactly this line from exactly this peer.
   *
   * The gate on the repair channel. A backlog arrives because we asked for
   * something, so anything else in one is not a repair: a reply that
   * introduced a line nobody asked for would let one datagram raise an
   * author's high-water mark arbitrarily, after which every genuine line that
   * author sends sorts below it and is dropped as a duplicate — silencing
   * them, permanently and silently, through the mechanism built to stop chat
   * going missing.
   *
   * The address has to match too. Only the author can be sure what its own
   * seq 7 was; a copy from anyone else is hearsay, which the serving side
   * already refuses to hand out.
   */
  expecting(author, addr, seq) {
    const st = this.senders.get(author);
    if (!st || st.addr !== addr) return false;
    return st.missing.has(seq);
  }

  /**
   * Records lines the author says it no longer holds. This is what turns
   * "still waiting" into "gone": without it a receiver asks for the same
   * rotated-out line until it gives up on a counter, and reports a gap that
   * could still heal when it cannot.
   */
  markUnavailable(addr, seqs) {
    for (const st of this.senders.values()) {
      if (st.addr !== addr) continue;
      for (const s of seqs) {
        if (st.missing.has(s)) {
          st.missing.delete(s);
          st.lost++;
        }
      }
    }
  }

  /** Every gap still waited on, across all authors. */
  outstanding() {
    let total = 0;
    for (const st of this.senders.values()) total += st.missing.size;
    return total;
  }

  /** Summarises every author with something to report, stable by address. */
  stats() {
    const out = [];
    for (const st of this.senders.values()) {
      if (st.missing.size === 0 && st.lost === 0 && st.recovered === 0) continue;
      out.push({
        addr: st.addr, nick: '', outstanding: st.missing.size, lost: st.lost, recovered: st.recovered,
      });
    }
    return out.sort((a, b) => (a.addr < b.addr ? -1 : a.addr > b.addr ? 1 : 0));
  }

  /** Drops an author's bookkeeping, for a peer this node no longer tracks. */
  forget(addr) {
    for (const [id, st] of [...this.senders.entries()]) {
      if (st.addr === addr) this.senders.delete(id);
    }
  }

  /** Clears everything, for a node that is stopping. */
  reset() {
    this.senders.clear();
  }
}

module.exports = { ChatTracker, ChatOutcome, MAX_TRACKED_GAPS, MAX_PULL_ATTEMPTS, MAX_TRACKED_AUTHORS };
