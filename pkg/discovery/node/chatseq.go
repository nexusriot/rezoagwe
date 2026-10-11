package node

import (
	"sort"
	"sync"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// A chat broadcast is one datagram per peer and nothing re-sends it. Anti-
// entropy repairs the KV store because every entry carries a version a digest
// can compare; a chat line carried nothing, so a dropped one was not merely
// unrecoverable — it was undetectable. The sender's log showed it sent, every
// receiver's log simply never had it, and no counter anywhere moved.
//
// A per-author sequence is the smallest thing that changes that. It makes a
// gap nameable, which in turn makes it askable-for, and makes a re-delivery
// recognisable instead of a second copy of the same line.

const (
	// maxTrackedGaps bounds the per-author gap set. A peer that is badly
	// unreachable must not let the bookkeeping grow without limit; the oldest
	// gaps are the least likely to still be in the author's ring anyway.
	maxTrackedGaps = 512
	// maxPullAttempts is how many times a gap is asked for before it is
	// declared lost. An author that answers says so explicitly; this is for
	// the one that never answers at all.
	maxPullAttempts = 5
	// maxTrackedAuthors bounds how many authors are followed at once. Every
	// heartbeat from every peer names an author, and a node that restarts
	// without a state file comes back with a *new* id, so the set grows with
	// cluster churn and nothing in the protocol ever shrinks it. The gaps
	// inside each author were bounded from the start; the authors were not,
	// which made the one unbounded thing a long-running node accumulates.
	maxTrackedAuthors = 64
)

// chatOutcome is what a receiver should do with a message it just decoded.
type chatOutcome int

const (
	// chatAccept — a line not seen before. Append it.
	chatAccept chatOutcome = iota
	// chatFilledGap — a line that had been missing. Append it; the gap closes.
	chatFilledGap
	// chatDuplicate — already held. Dropping it is the point: without a
	// sequence, a re-delivery (the repair path's whole job) would append a
	// second copy of a line already on screen.
	chatDuplicate
)

// chatSender is what is known about one author's stream.
type chatSender struct {
	// addr is where this author was last heard from, which is where a pull
	// for its backlog has to go — only the author can say what its own seq 7
	// was.
	addr string
	// last is the highest sequence seen.
	last uint64
	// missing maps a gap to how many times it has been asked for.
	missing map[uint64]int
	// lost counts gaps given up on: the author said it no longer holds them,
	// or it never answered. These are the ones that will not arrive.
	lost uint64
	// recovered counts gaps that a pull actually filled.
	recovered uint64
	// touched orders authors by how recently anything was heard about them,
	// so the set can be trimmed to the ones that still matter.
	touched uint64
}

// chatTracker follows every author's sequence, independently of the engine, so
// the rules here are testable without a network.
//
// Authors are keyed by **node id**, not address: the id is persistent and the
// address is not, so a node that moves ports stays one author — and a node
// that restarts with no state file gets a fresh id and is correctly treated as
// a new one, rather than appearing to rewind its whole conversation.
type chatTracker struct {
	mu      sync.Mutex
	senders map[string]*chatSender
	// clock orders activity without depending on wall time, which a peer can
	// be wrong about.
	clock uint64
}

func newChatTracker() *chatTracker {
	return &chatTracker{senders: make(map[string]*chatSender)}
}

// authorOf keys a message. Falling back to the address keeps a peer that
// predates the field tracked as *something* rather than untracked.
func authorOf(m pb.ChatMessage) string {
	if m.Node != "" {
		return m.Node
	}
	return m.Sender
}

// Observe records one received broadcast and says what to do with it.
//
// An unsequenced message (Seq 0 — a direct message, or a peer older than the
// field) is always accepted and never gap-checked: claiming a gap against a
// sender that does not number its lines would invent losses on every hop.
func (t *chatTracker) Observe(m pb.ChatMessage) chatOutcome {
	if m.Seq == 0 {
		return chatAccept
	}
	author := authorOf(m)
	if author == "" {
		return chatAccept
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	st, known := t.senders[author]
	if !known {
		// First line ever heard from this author. Whatever came before it
		// belongs to a conversation this node was not in, so it is not a gap:
		// a joiner that reported every line sent before it arrived as lost
		// would cry wolf on every join.
		st := &chatSender{addr: m.Sender, last: m.Seq, missing: map[uint64]int{}}
		t.senders[author] = st
		t.touchLocked(st)
		return chatAccept
	}
	if m.Sender != "" {
		st.addr = m.Sender
	}
	t.touchLocked(st)

	switch {
	case m.Seq == st.last+1:
		st.last = m.Seq
		return chatAccept
	case m.Seq > st.last+1:
		for s := st.last + 1; s < m.Seq; s++ {
			st.missing[s] = 0
		}
		st.last = m.Seq
		st.trimLocked()
		return chatAccept
	}

	// At or below the high-water mark: either the gap we asked for, or a line
	// already held.
	if _, waiting := st.missing[m.Seq]; waiting {
		delete(st.missing, m.Seq)
		st.recovered++
		return chatFilledGap
	}
	return chatDuplicate
}

// touchLocked marks an author as recently active and trims the set if it has
// outgrown the bound. The least recently active goes: an author nothing has
// been heard about is the one whose bookkeeping is least likely to be needed,
// and dropping it is safe — if they speak again they are simply treated as an
// author newly met, which claims nothing.
func (t *chatTracker) touchLocked(st *chatSender) {
	t.clock++
	st.touched = t.clock
	if len(t.senders) <= maxTrackedAuthors {
		return
	}
	var oldest string
	var oldestAt uint64
	for id, s := range t.senders {
		if oldest == "" || s.touched < oldestAt {
			oldest, oldestAt = id, s.touched
		}
	}
	delete(t.senders, oldest)
}

// trimLocked keeps the gap set bounded by dropping the oldest, which are the
// least likely to still be in the author's ring. They are counted as lost
// rather than forgotten silently.
func (st *chatSender) trimLocked() {
	if len(st.missing) <= maxTrackedGaps {
		return
	}
	seqs := make([]uint64, 0, len(st.missing))
	for s := range st.missing {
		seqs = append(seqs, s)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for _, s := range seqs[:len(st.missing)-maxTrackedGaps] {
		delete(st.missing, s)
		st.lost++
	}
}

// Seed records what a bulk delivery of history already contains, without
// claiming anything is missing.
//
// Two paths put lines into the log without them arriving one at a time: the
// state file at startup, and a state-sync snapshot on join. Both used to go
// straight past the tracker, and the startup case is the one that mattered —
// a node holding alice's lines 1..5 restarts, alice says 6 while it is down,
// and the fresh tracker treats 7 as the first line it has ever seen from her.
// Line 6 is then lost in silence, in exactly the situation where a line is
// most likely to have been missed: the node was not running.
//
// It only ever closes gaps and raises the high-water mark. It never invents
// one: a snapshot is a bounded slice of someone else's ring, so the absence of
// a sequence from it is not evidence that the line was lost — and a false gap
// would send this node asking for something nobody ever said.
func (t *chatTracker) Seed(entries []pb.ChatEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range entries {
		if e.Seq == 0 {
			continue
		}
		author := e.Node
		if author == "" {
			author = e.Sender
		}
		if author == "" {
			continue
		}
		st, known := t.senders[author]
		if !known {
			st := &chatSender{addr: e.Sender, last: e.Seq, missing: map[uint64]int{}}
			t.senders[author] = st
			t.touchLocked(st)
			continue
		}
		if e.Sender != "" && st.addr == "" {
			st.addr = e.Sender
		}
		if _, waiting := st.missing[e.Seq]; waiting {
			delete(st.missing, e.Seq)
			st.recovered++
		}
		if e.Seq > st.last {
			st.last = e.Seq
		}
	}
}

// Announce records how far an author says its own numbering has got, from a
// heartbeat rather than from a line it spoke.
//
// Seeding a baseline only from the first line an author is *heard* to say
// conflates two different things: a node that was not there when the earlier
// lines were said, which has nothing to report, and a node that was there the
// whole time and simply did not hear them, which has lost them. Measured on a
// real cluster: a peer that had never spoken said three lines while one node
// was off the air, and that node reported nothing — the first line it heard
// afterwards became the baseline and the three were gone in silence.
//
// So the first announcement only sets the mark, and later ones name everything
// between. A node that has just met a peer still claims nothing about the
// conversation it was never part of.
func (t *chatTracker) Announce(author, addr string, seq uint64) {
	if author == "" {
		return
	}
	// Seq 0 is not "no information": it is a peer saying it has said nothing
	// yet, and it is the most useful baseline there is — met before it ever
	// spoke, so everything it says from now on is accounted for. A peer too
	// old to send the field reports 0 too, which costs nothing: its lines
	// carry no sequence either, so they are never gap-checked.

	t.mu.Lock()
	defer t.mu.Unlock()
	st, known := t.senders[author]
	if !known {
		st := &chatSender{addr: addr, last: seq, missing: map[uint64]int{}}
		t.senders[author] = st
		t.touchLocked(st)
		return
	}
	if addr != "" {
		st.addr = addr
	}
	t.touchLocked(st)
	if seq <= st.last {
		return
	}
	for s := st.last + 1; s <= seq; s++ {
		st.missing[s] = 0
	}
	st.last = seq
	st.trimLocked()
}

// chatPullPlan is one request to make: which author, where it lives, and which
// of its lines to ask for.
type chatPullPlan struct {
	Addr string
	Seqs []uint64
}

// PullPlans lists the requests worth making now, charging an attempt to each
// gap it names. A gap that has been asked for maxPullAttempts times without
// ever being answered is given up on here — an author that is simply gone
// would otherwise be asked for the same line on every tick forever.
func (t *chatTracker) PullPlans() []chatPullPlan {
	t.mu.Lock()
	defer t.mu.Unlock()

	var plans []chatPullPlan
	for _, st := range t.senders {
		if st.addr == "" || len(st.missing) == 0 {
			continue
		}
		seqs := make([]uint64, 0, len(st.missing))
		for s, attempts := range st.missing {
			if attempts >= maxPullAttempts {
				delete(st.missing, s)
				st.lost++
				continue
			}
			seqs = append(seqs, s)
		}
		if len(seqs) == 0 {
			continue
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		if len(seqs) > pb.MaxChatPull {
			seqs = seqs[:pb.MaxChatPull]
		}
		for _, s := range seqs {
			st.missing[s]++
		}
		plans = append(plans, chatPullPlan{Addr: st.addr, Seqs: seqs})
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].Addr < plans[j].Addr })
	return plans
}

// Expecting reports whether this node is waiting for exactly this line from
// exactly this peer.
//
// It is the gate on the repair channel. A backlog arrives because we asked for
// something, so anything else in one is not a repair: a reply that introduced
// a line nobody asked for would let one datagram raise an author's high-water
// mark arbitrarily, after which every genuine line that author sends sorts
// below it and is dropped as a duplicate — silencing them, permanently and
// silently, through the mechanism built to stop chat going missing.
//
// The address has to match too. Only the author can be sure what its own Seq 7
// was; a copy from anyone else is hearsay, which the serving side already
// refuses to hand out and the receiving side used not to refuse to take.
func (t *chatTracker) Expecting(author, addr string, seq uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.senders[author]
	if !ok || st.addr != addr {
		return false
	}
	_, waiting := st.missing[seq]
	return waiting
}

// MarkUnavailable records lines the author says it no longer holds. This is
// what turns "still waiting" into "gone": without it a receiver asks for the
// same rotated-out line until it gives up on a timer, and reports a gap that
// could still heal when it cannot.
func (t *chatTracker) MarkUnavailable(addr string, seqs []uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, st := range t.senders {
		if st.addr != addr {
			continue
		}
		for _, s := range seqs {
			if _, waiting := st.missing[s]; waiting {
				delete(st.missing, s)
				st.lost++
			}
		}
	}
}

// ChatGapStat is one author's delivery record, for the diagnostics.
type ChatGapStat struct {
	Addr string `json:"addr"`
	Nick string `json:"nick,omitempty"`
	// Outstanding are gaps still being asked for — they may yet arrive.
	Outstanding int `json:"outstanding"`
	// Lost are gaps that will not: the author no longer holds them, or never
	// answered.
	Lost uint64 `json:"lost"`
	// Recovered are gaps a pull actually filled, which is the evidence that
	// the repair path works at all.
	Recovered uint64 `json:"recovered"`
}

// Stats summarises every author, newest-sorted by address so a report is
// stable.
func (t *chatTracker) Stats() []ChatGapStat {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ChatGapStat, 0, len(t.senders))
	for _, st := range t.senders {
		if len(st.missing) == 0 && st.lost == 0 && st.recovered == 0 {
			continue
		}
		out = append(out, ChatGapStat{
			Addr:        st.addr,
			Outstanding: len(st.missing),
			Lost:        st.lost,
			Recovered:   st.recovered,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// outstanding counts every gap still being waited on, across all authors.
// Sampling it either side of an Observe is how the engine tells "this message
// revealed a gap" from "this message was simply the next one".
func (t *chatTracker) outstanding() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	total := 0
	for _, st := range t.senders {
		total += len(st.missing)
	}
	return total
}
