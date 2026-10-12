package com.nexusriot.rezoagwe.core

import com.nexusriot.rezoagwe.proto.ChatEntry
import com.nexusriot.rezoagwe.proto.ChatMessage
import com.nexusriot.rezoagwe.proto.MAX_CHAT_PULL

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
const val MAX_TRACKED_GAPS = 512

/** How many times a gap is asked for before it is declared lost. */
const val MAX_PULL_ATTEMPTS = 5

/**
 * Bounds how many authors are followed at once. Every heartbeat from every
 * peer names an author, and a node that restarts without a state file comes
 * back with a *new* id, so the set grows with cluster churn and nothing in the
 * protocol ever shrinks it. The gaps inside each author were bounded from the
 * start; the authors were not.
 */
const val MAX_TRACKED_AUTHORS = 64

/** What a receiver should do with a message it just decoded. */
enum class ChatOutcome {
    /** Not seen before. Append it. */
    ACCEPT,

    /** Had been missing. Append it; the gap closes. */
    FILLED_GAP,

    /**
     * Already held. Dropping it is the point: without a sequence a
     * re-delivery — the repair path's whole job — would append a second copy
     * of a line already on screen.
     */
    DUPLICATE,
}

/** One author's delivery record, for the diagnostics. */
data class ChatGapStat(
    val addr: String,
    val nick: String = "",
    /** Gaps still being asked for — they may yet arrive. */
    val outstanding: Int = 0,
    /** Gaps that will not: the author no longer holds them, or never answered. */
    val lost: Long = 0,
    /** Gaps a pull actually filled, which is the evidence the repair works. */
    val recovered: Long = 0,
)

/** One request to make: which author, where it lives, and which lines to ask for. */
data class ChatPullPlan(val addr: String, val seqs: List<Long>)

/**
 * Whether a logged line is one this node wrote, and may therefore serve from a
 * pull.
 *
 * It is the node id that decides, not the address. The id is what the sequence
 * numbering belongs to and it outlives a restart and a change of address; the
 * address is recomputed from whatever interface the device has right now. A
 * phone that loses Wi-Fi falls back to advertising loopback, and anything it
 * says there is stamped with that address — so matching on the address meant
 * the node disowned those lines the moment Wi-Fi came back, and answered the
 * pull for its own seq 7 with "gone for good" while that line was on its own
 * screen. The asker then counts it lost, which is the one outcome this whole
 * mechanism exists to prevent.
 *
 * Address is the fallback for entries written before a chat entry carried an
 * id, and only then: an entry that names a *different* author is hearsay
 * whatever address it arrived under.
 */
fun ownsChatEntry(entryNode: String, entrySender: String, selfId: String, selfAddr: String): Boolean =
    if (entryNode.isNotEmpty()) entryNode == selfId else entrySender.isNotEmpty() && entrySender == selfAddr

private class ChatSenderState(
    var addr: String,
    var last: Long,
    val missing: MutableMap<Long, Int> = mutableMapOf(),
    var lost: Long = 0,
    var recovered: Long = 0,
    /** Orders authors by how recently anything was heard about them. */
    var touched: Long = 0,
)

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
    private val lock = Any()
    private val senders = mutableMapOf<String, ChatSenderState>()

    /** Orders activity without depending on wall time, which a peer can be wrong about. */
    private var clock = 0L

    /**
     * Marks an author as recently active and trims the set if it has outgrown
     * the bound. The least recently active goes: dropping it is safe, because
     * an author heard from again is simply treated as one newly met, which
     * claims nothing.
     */
    private fun touch(st: ChatSenderState) {
        st.touched = ++clock
        if (senders.size <= MAX_TRACKED_AUTHORS) return
        val oldest = senders.minByOrNull { it.value.touched }?.key ?: return
        senders.remove(oldest)
    }

    /**
     * Records one received broadcast and says what to do with it.
     *
     * An unsequenced message (seq 0 — a direct message, or a peer older than
     * the field) is always accepted and never gap-checked: claiming a gap
     * against a sender that does not number its lines would invent losses on
     * every hop.
     */
    fun observe(m: ChatMessage): ChatOutcome {
        if (m.seq == 0L) return ChatOutcome.ACCEPT
        val author = m.node.ifEmpty { m.sender }
        if (author.isEmpty()) return ChatOutcome.ACCEPT

        synchronized(lock) {
            val st = senders[author]
            if (st == null) {
                // First line ever heard from this author. Whatever came before
                // belongs to a conversation this node was not in, so it is not
                // a gap: a joiner that reported every earlier line as lost
                // would cry wolf on every join.
                senders[author] = ChatSenderState(addr = m.sender, last = m.seq).also { touch(it) }
                return ChatOutcome.ACCEPT
            }
            if (m.sender.isNotEmpty()) st.addr = m.sender
            touch(st)

            when {
                m.seq == st.last + 1 -> {
                    st.last = m.seq
                    return ChatOutcome.ACCEPT
                }
                m.seq > st.last + 1 -> {
                    for (s in (st.last + 1) until m.seq) st.missing[s] = 0
                    st.last = m.seq
                    trim(st)
                    return ChatOutcome.ACCEPT
                }
            }

            // At or below the high-water mark: either the gap we asked for, or
            // a line already held.
            if (st.missing.remove(m.seq) != null) {
                st.recovered++
                return ChatOutcome.FILLED_GAP
            }
            return ChatOutcome.DUPLICATE
        }
    }

    /**
     * Keeps the gap set bounded by dropping the oldest, which are the least
     * likely to still be in the author's ring. They are counted as lost rather
     * than forgotten silently.
     */
    private fun trim(st: ChatSenderState) {
        if (st.missing.size <= MAX_TRACKED_GAPS) return
        val drop = st.missing.keys.sorted().take(st.missing.size - MAX_TRACKED_GAPS)
        for (s in drop) {
            st.missing.remove(s)
            st.lost++
        }
    }

    /**
     * Records what a bulk delivery of history already contains, without
     * claiming anything is missing.
     *
     * Two paths put lines into the log without them arriving one at a time:
     * the state file at startup, and a state-sync snapshot on join. The
     * startup case is the one that matters — a node holding alice's lines 1..5
     * restarts, alice says 6 while it is down, and a fresh tracker treats 7 as
     * the first line it has ever seen from her. Line 6 is then lost in
     * silence, in exactly the situation where a line is most likely to have
     * been missed: the node was not running.
     *
     * It only closes gaps and raises the high-water mark, never invents one: a
     * snapshot is a bounded slice of someone else's ring, so what it leaves
     * out is not evidence of a loss.
     */
    fun seed(entries: List<ChatEntry>) = synchronized(lock) {
        for (e in entries) {
            if (e.seq == 0L) continue
            val author = e.node.ifEmpty { e.sender }
            if (author.isEmpty()) continue
            val st = senders[author]
            if (st == null) {
                senders[author] = ChatSenderState(addr = e.sender, last = e.seq).also { touch(it) }
                continue
            }
            if (e.sender.isNotEmpty() && st.addr.isEmpty()) st.addr = e.sender
            if (st.missing.remove(e.seq) != null) st.recovered++
            if (e.seq > st.last) st.last = e.seq
        }
    }

    /**
     * Records how far an author says its own numbering has got, from a
     * heartbeat rather than from a line it spoke.
     *
     * Seeding only from the first line an author is *heard* to say conflates a
     * node that was not there when the earlier lines were said, which has
     * nothing to report, with one that was there the whole time and simply did
     * not hear them, which has lost them. Measured on a real cluster: a peer
     * that had never spoken said three lines while one node was off the air,
     * and that node reported nothing.
     *
     * The first announcement only sets the mark — seq 0 included, which is a
     * peer saying it has said nothing yet and is the most useful baseline
     * there is. Later ones name everything in between.
     */
    fun announce(author: String, addr: String, seq: Long) = synchronized(lock) {
        if (author.isEmpty()) return@synchronized
        val st = senders[author]
        if (st == null) {
            senders[author] = ChatSenderState(addr = addr, last = seq).also { touch(it) }
            return@synchronized
        }
        if (addr.isNotEmpty()) st.addr = addr
        touch(st)
        if (seq <= st.last) return@synchronized
        for (s in (st.last + 1)..seq) st.missing[s] = 0
        st.last = seq
        trim(st)
    }

    /**
     * Lists the requests worth making now, charging an attempt to each gap it
     * names. A gap asked for [MAX_PULL_ATTEMPTS] times without ever being
     * answered is given up on here — an author that is simply gone would
     * otherwise be asked for the same line on every tick forever.
     */
    fun pullPlans(): List<ChatPullPlan> = synchronized(lock) {
        val plans = mutableListOf<ChatPullPlan>()
        for (st in senders.values) {
            if (st.addr.isEmpty() || st.missing.isEmpty()) continue
            val seqs = mutableListOf<Long>()
            for ((s, attempts) in st.missing.entries.toList()) {
                if (attempts >= MAX_PULL_ATTEMPTS) {
                    st.missing.remove(s)
                    st.lost++
                } else {
                    seqs += s
                }
            }
            if (seqs.isEmpty()) continue
            seqs.sort()
            val asking = seqs.take(MAX_CHAT_PULL)
            for (s in asking) st.missing[s] = (st.missing[s] ?: 0) + 1
            plans += ChatPullPlan(st.addr, asking)
        }
        plans.sortedBy { it.addr }
    }

    /**
     * Whether this node is waiting for exactly this line from exactly this
     * peer.
     *
     * The gate on the repair channel. A backlog arrives because we asked for
     * something, so anything else in one is not a repair: a reply that
     * introduced a line nobody asked for would let one datagram raise an
     * author's high-water mark arbitrarily, after which every genuine line
     * that author sends sorts below it and is dropped as a duplicate —
     * silencing them, permanently and silently, through the mechanism built to
     * stop chat going missing.
     *
     * The address has to match too. Only the author can be sure what its own
     * seq 7 was; a copy from anyone else is hearsay, which the serving side
     * already refuses to hand out.
     */
    fun expecting(author: String, addr: String, seq: Long): Boolean = synchronized(lock) {
        val st = senders[author] ?: return false
        if (st.addr != addr) return false
        return st.missing.containsKey(seq)
    }

    /**
     * Records lines the author says it no longer holds. This is what turns
     * "still waiting" into "gone": without it a receiver asks for the same
     * rotated-out line until it gives up on a counter, and reports a gap that
     * could still heal when it cannot.
     */
    fun markUnavailable(addr: String, seqs: List<Long>) = synchronized(lock) {
        for (st in senders.values) {
            if (st.addr != addr) continue
            for (s in seqs) {
                if (st.missing.remove(s) != null) st.lost++
            }
        }
    }

    /** Every gap still waited on, across all authors. */
    fun outstanding(): Int = synchronized(lock) { senders.values.sumOf { it.missing.size } }

    /** Summarises every author with something to report, stable by address. */
    fun stats(): List<ChatGapStat> = synchronized(lock) {
        senders.values
            .filter { it.missing.isNotEmpty() || it.lost > 0 || it.recovered > 0 }
            .map {
                ChatGapStat(
                    addr = it.addr,
                    outstanding = it.missing.size,
                    lost = it.lost,
                    recovered = it.recovered,
                )
            }
            .sortedBy { it.addr }
    }

    /** Drops an author's bookkeeping, for a peer this node no longer tracks. */
    fun forget(addr: String) = synchronized(lock) {
        senders.entries.removeAll { it.value.addr == addr }
        Unit
    }

    /** Clears everything, for a node that is stopping. */
    fun reset() = synchronized(lock) { senders.clear() }
}
