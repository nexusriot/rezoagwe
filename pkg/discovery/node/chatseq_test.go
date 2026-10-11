package node

import (
	"fmt"
	"testing"
	"time"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// waitForChat waits until a node's log holds a line with this text.
func waitForChat(t *testing.T, n *Node, text string) {
	t.Helper()
	waitFor(t, "chat line "+text, func() bool {
		for _, e := range n.Model.ChatLog() {
			if e.Text == text {
				return true
			}
		}
		return false
	})
}

func msg(node string, seq uint64, text string) pb.ChatMessage {
	return pb.ChatMessage{Sender: "10.0.0.2:3137", Node: node, Text: text, Seq: seq}
}

// The baseline case: numbering in order is accepted and claims nothing.
func TestChatSequenceInOrderIsAllAccepted(t *testing.T) {
	tr := newChatTracker()
	for s := uint64(1); s <= 5; s++ {
		if got := tr.Observe(msg("a", s, "x")); got != chatAccept {
			t.Fatalf("seq %d = %v, want accept", s, got)
		}
	}
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("outstanding = %d, want 0", n)
	}
	if stats := tr.Stats(); len(stats) != 0 {
		t.Fatalf("a clean stream produced findings: %+v", stats)
	}
}

// The case the whole feature exists for: a line that never arrives used to be
// indistinguishable from one that was never sent.
func TestChatSequenceNamesTheMissingLines(t *testing.T) {
	tr := newChatTracker()
	tr.Observe(msg("a", 1, "one"))
	tr.Observe(msg("a", 4, "four")) // 2 and 3 never arrived

	if n := tr.outstanding(); n != 2 {
		t.Fatalf("outstanding = %d, want 2", n)
	}
	plans := tr.PullPlans()
	if len(plans) != 1 {
		t.Fatalf("plans = %+v, want one", plans)
	}
	if plans[0].Addr != "10.0.0.2:3137" {
		t.Fatalf("pull addressed to %q, want the author", plans[0].Addr)
	}
	if len(plans[0].Seqs) != 2 || plans[0].Seqs[0] != 2 || plans[0].Seqs[1] != 3 {
		t.Fatalf("asked for %v, want [2 3]", plans[0].Seqs)
	}
}

// A late arrival closes its own gap, and is still appended — it is a line the
// user has not seen.
func TestChatSequenceAReorderedLineClosesItsGap(t *testing.T) {
	tr := newChatTracker()
	tr.Observe(msg("a", 1, "one"))
	tr.Observe(msg("a", 3, "three"))

	if got := tr.Observe(msg("a", 2, "two")); got != chatFilledGap {
		t.Fatalf("the late line = %v, want filledGap", got)
	}
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("outstanding = %d, want 0 — the gap did not close", n)
	}
	if stats := tr.Stats(); len(stats) != 1 || stats[0].Recovered != 1 {
		t.Fatalf("stats = %+v, want one recovered", stats)
	}
}

// Re-delivery is what the repair path does, so recognising it is not optional:
// without this the cure puts a second copy of every recovered line on screen.
func TestChatSequenceDropsADuplicate(t *testing.T) {
	tr := newChatTracker()
	tr.Observe(msg("a", 1, "one"))
	tr.Observe(msg("a", 2, "two"))

	if got := tr.Observe(msg("a", 2, "two")); got != chatDuplicate {
		t.Fatalf("re-delivery = %v, want duplicate", got)
	}
	if got := tr.Observe(msg("a", 1, "one")); got != chatDuplicate {
		t.Fatalf("an older re-delivery = %v, want duplicate", got)
	}
}

// Joining mid-conversation is not a loss. A node that reported every line sent
// before it arrived would cry wolf on every join.
func TestChatSequenceAJoinerClaimsNoGap(t *testing.T) {
	tr := newChatTracker()
	if got := tr.Observe(msg("a", 97, "we were already talking")); got != chatAccept {
		t.Fatalf("first line = %v, want accept", got)
	}
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("outstanding = %d, want 0 — the joiner invented 96 losses", n)
	}
}

// An author keyed by address alone would read a restart as its whole
// conversation rewinding, and ignore everything after it as a duplicate. The
// node id is what keeps the two apart.
func TestChatSequenceARestartWithoutStateIsANewAuthor(t *testing.T) {
	tr := newChatTracker()
	for s := uint64(1); s <= 9; s++ {
		tr.Observe(msg("old-id", s, "x"))
	}
	// Same address, fresh identity, numbering from 1 again.
	if got := tr.Observe(msg("new-id", 1, "hello again")); got != chatAccept {
		t.Fatalf("a restarted author's first line = %v, want accept", got)
	}
	if got := tr.Observe(msg("new-id", 2, "still here")); got != chatAccept {
		t.Fatalf("a restarted author's second line = %v, want accept", got)
	}
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("outstanding = %d, want 0 — the restart was read as a gap", n)
	}
}

// A node that moves ports is the same author, because the id is what Seq
// counts against.
func TestChatSequenceSurvivesAnAddressChange(t *testing.T) {
	tr := newChatTracker()
	tr.Observe(pb.ChatMessage{Sender: "10.0.0.2:3137", Node: "a", Seq: 1})
	moved := pb.ChatMessage{Sender: "10.0.0.2:4000", Node: "a", Seq: 2}
	if got := tr.Observe(moved); got != chatAccept {
		t.Fatalf("after the move = %v, want accept", got)
	}
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("outstanding = %d, want 0", n)
	}
	// And a pull now goes to where it actually lives.
	tr.Observe(pb.ChatMessage{Sender: "10.0.0.2:4000", Node: "a", Seq: 4})
	plans := tr.PullPlans()
	if len(plans) != 1 || plans[0].Addr != "10.0.0.2:4000" {
		t.Fatalf("pull plans = %+v, want the new address", plans)
	}
}

// A peer older than the field numbers nothing, and must not be reported as
// losing everything.
func TestChatSequenceIgnoresAnUnsequencedSender(t *testing.T) {
	tr := newChatTracker()
	for i := 0; i < 5; i++ {
		if got := tr.Observe(pb.ChatMessage{Sender: "10.0.0.9:3137", Text: "legacy"}); got != chatAccept {
			t.Fatalf("unsequenced line = %v, want accept", got)
		}
	}
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("outstanding = %d, want 0", n)
	}
	if plans := tr.PullPlans(); len(plans) != 0 {
		t.Fatalf("asked an unsequenced peer for a backlog: %+v", plans)
	}
}

// "Gone" and "still waiting" are different answers, and a receiver that is not
// told the difference asks for a rotated-out line forever.
func TestChatSequenceStopsAskingForWhatTheAuthorNoLongerHolds(t *testing.T) {
	tr := newChatTracker()
	tr.Observe(msg("a", 1, "one"))
	tr.Observe(msg("a", 4, "four"))

	tr.MarkUnavailable("10.0.0.2:3137", []uint64{2, 3})
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("outstanding = %d, want 0 after the author said they are gone", n)
	}
	if plans := tr.PullPlans(); len(plans) != 0 {
		t.Fatalf("still asking for lines the author cannot supply: %+v", plans)
	}
	stats := tr.Stats()
	if len(stats) != 1 || stats[0].Lost != 2 {
		t.Fatalf("stats = %+v, want 2 lost", stats)
	}
}

// An author that never answers at all must not be asked forever either.
func TestChatSequenceGivesUpOnAnAuthorThatNeverAnswers(t *testing.T) {
	tr := newChatTracker()
	tr.Observe(msg("a", 1, "one"))
	tr.Observe(msg("a", 3, "three"))

	for round := 0; round < maxPullAttempts; round++ {
		if plans := tr.PullPlans(); len(plans) != 1 {
			t.Fatalf("round %d: plans = %+v, want one", round, plans)
		}
	}
	if plans := tr.PullPlans(); len(plans) != 0 {
		t.Fatalf("still asking after %d attempts: %+v", maxPullAttempts, plans)
	}
	stats := tr.Stats()
	if len(stats) != 1 || stats[0].Lost != 1 {
		t.Fatalf("stats = %+v, want the line counted lost", stats)
	}
}

// One request is one datagram, so a node far behind asks in batches rather
// than naming every gap at once.
func TestChatSequencePullIsBounded(t *testing.T) {
	tr := newChatTracker()
	tr.Observe(msg("a", 1, "one"))
	tr.Observe(msg("a", uint64(pb.MaxChatPull*3), "much later"))

	plans := tr.PullPlans()
	if len(plans) != 1 {
		t.Fatalf("plans = %+v, want one", plans)
	}
	if len(plans[0].Seqs) != pb.MaxChatPull {
		t.Fatalf("asked for %d lines, want at most %d", len(plans[0].Seqs), pb.MaxChatPull)
	}
	// Oldest first, so the conversation fills in from where it broke.
	if plans[0].Seqs[0] != 2 {
		t.Fatalf("first requested seq = %d, want 2", plans[0].Seqs[0])
	}
}

// The gap set cannot grow without bound against a peer that is badly
// unreachable.
func TestChatSequenceBoundsWhatItTracks(t *testing.T) {
	tr := newChatTracker()
	tr.Observe(msg("a", 1, "one"))
	tr.Observe(msg("a", uint64(maxTrackedGaps*4), "far ahead"))

	if n := tr.outstanding(); n > maxTrackedGaps {
		t.Fatalf("tracking %d gaps, want at most %d", n, maxTrackedGaps)
	}
	stats := tr.Stats()
	if len(stats) != 1 || stats[0].Lost == 0 {
		t.Fatalf("stats = %+v, want the dropped gaps counted lost rather than forgotten", stats)
	}
}

// The whole feature, between two nodes over a network that drops a packet:
// before this, the dropped line was gone and nothing anywhere said so.
func TestADroppedChatLineIsDetectedAndRepaired(t *testing.T) {
	net := transport.NewMemNet(7)
	a, b := newPairedNodes(t, net)

	a.SendChat("first")
	waitForChat(t, b, "first")

	// Exactly one line lost — the case that used to be invisible at both ends.
	net.Partition(a.cfg.AdvertiseAddr, b.cfg.AdvertiseAddr, true)
	a.SendChat("the line that never arrives")
	net.Partition(a.cfg.AdvertiseAddr, b.cfg.AdvertiseAddr, false)

	a.SendChat("third")
	waitForChat(t, b, "third")

	if got := b.Metrics.ChatGapsFound.Load(); got != 1 {
		t.Fatalf("gaps found = %d, want 1 — the loss was still invisible", got)
	}
	gaps := b.ChatGaps()
	if len(gaps) != 1 || gaps[0].Outstanding != 1 {
		t.Fatalf("chat gaps = %+v, want one outstanding", gaps)
	}

	// The repair: b asks the author, which is the only node that can answer.
	b.pullChatGaps()
	waitForChat(t, b, "the line that never arrives")

	if got := b.Metrics.ChatRecovered.Load(); got != 1 {
		t.Fatalf("recovered = %d, want 1", got)
	}
	if got := b.ChatGaps(); len(got) != 1 || got[0].Outstanding != 0 || got[0].Recovered != 1 {
		t.Fatalf("chat gaps after repair = %+v, want nothing outstanding", got)
	}

	// And it is back in the conversation where it was said, not after the line
	// that followed it.
	var said []string
	for _, e := range b.Model.ChatLog() {
		if e.Kind == pb.ChatMsg && e.Sender == a.cfg.AdvertiseAddr {
			said = append(said, e.Text)
		}
	}
	want := []string{"first", "the line that never arrives", "third"}
	if len(said) != len(want) {
		t.Fatalf("b holds %v, want %v", said, want)
	}
	for i := range want {
		if said[i] != want[i] {
			t.Fatalf("b holds %v, want %v", said, want)
		}
	}
}

// The repair and the original can both land — a late datagram and a backlog
// racing — and the user must still see the line once.
func TestARepairedLineIsNotDeliveredTwice(t *testing.T) {
	net := transport.NewMemNet(11)
	a, b := newPairedNodes(t, net)

	a.SendChat("first")
	waitForChat(t, b, "first")

	net.Partition(a.cfg.AdvertiseAddr, b.cfg.AdvertiseAddr, true)
	a.SendChat("only once")
	net.Partition(a.cfg.AdvertiseAddr, b.cfg.AdvertiseAddr, false)
	a.SendChat("third")
	waitForChat(t, b, "third")

	// The backlog fills the gap.
	b.pullChatGaps()
	waitForChat(t, b, "only once")

	// And now the original turns up anyway, as a late datagram would.
	a.send(b.cfg.AdvertiseAddr, pb.KindChat, pb.ChatMessage{
		Sender: a.cfg.AdvertiseAddr, Node: a.Model.NodeID, Nick: "a",
		Text: "only once", TS: time.Now().Unix(), Seq: 2,
	})
	waitFor(t, "the late original to be recognised", func() bool {
		return b.Metrics.ChatDuplicates.Load() > 0
	})

	count := 0
	for _, e := range b.Model.ChatLog() {
		if e.Text == "only once" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the line appears %d times, want 1", count)
	}
}

// A backlog may only fill a gap this node is already waiting for.
//
// Found on hardware: one ChatBacklog carrying a line attributed to another
// peer, with a large sequence, was accepted — which raised that author's
// high-water mark so far that every genuine line it sent afterwards sorted
// below it and was dropped as a duplicate. One datagram silenced an author at
// that node permanently, through the mechanism built to stop chat going
// missing.
func TestABacklogCannotIntroduceALineNobodyAskedFor(t *testing.T) {
	net := transport.NewMemNet(17)
	a, b := newPairedNodes(t, net)

	a.SendChat("genuine one")
	waitForChat(t, b, "genuine one")

	// A backlog from b's peer, carrying a line attributed to a third party and
	// numbered far ahead. Nothing is waiting for it.
	a.send(b.cfg.AdvertiseAddr, pb.KindChatBacklog, pb.ChatBacklog{
		From: a.cfg.AdvertiseAddr,
		Messages: []pb.ChatMessage{{
			Sender: "10.0.0.9:3137", Node: "a-third-party", Nick: "mallory",
			Text: "UNASKED", TS: time.Now().Unix(), Seq: 900001,
		}},
	})
	waitFor(t, "the unasked-for line to be refused", func() bool {
		return b.Metrics.ChatUnsolicited.Load() > 0
	})

	for _, e := range b.Model.ChatLog() {
		if e.Text == "UNASKED" {
			t.Fatal("a backlog introduced a line nobody asked for")
		}
	}

	// And the author it was attributed to is not silenced: the high-water mark
	// never moved, so their next line still lands.
	a.SendChat("genuine two")
	waitForChat(t, b, "genuine two")
}

// The same guard must refuse a line the asking node wants, offered by someone
// other than its author — only the author can be sure what its own Seq 7 was.
func TestABacklogFromTheWrongPeerIsRefused(t *testing.T) {
	net := transport.NewMemNet(19)
	a, b := newPairedNodes(t, net)
	c := newTestNode(t, net, "10.0.0.3:3137")
	b.Model.AddPeer(c.cfg.AdvertiseAddr)

	a.SendChat("first")
	waitForChat(t, b, "first")
	net.Partition(a.cfg.AdvertiseAddr, b.cfg.AdvertiseAddr, true)
	a.SendChat("the missing one")
	net.Partition(a.cfg.AdvertiseAddr, b.cfg.AdvertiseAddr, false)
	a.SendChat("third")
	waitForChat(t, b, "third")

	// c answers for a's line. b is waiting for exactly this sequence, but not
	// from c.
	c.send(b.cfg.AdvertiseAddr, pb.KindChatBacklog, pb.ChatBacklog{
		From: c.cfg.AdvertiseAddr,
		Messages: []pb.ChatMessage{{
			Sender: a.cfg.AdvertiseAddr, Node: a.Model.NodeID, Nick: "a",
			Text: "HEARSAY", TS: time.Now().Unix(), Seq: 2,
		}},
	})
	waitFor(t, "the hearsay to be refused", func() bool {
		return b.Metrics.ChatUnsolicited.Load() > 0
	})
	for _, e := range b.Model.ChatLog() {
		if e.Text == "HEARSAY" {
			t.Fatal("took one peer's word for another peer's line")
		}
	}

	// The real author can still answer it.
	b.pullChatGaps()
	waitForChat(t, b, "the missing one")
}

// An author that has rotated the line out of its ring says so, so the receiver
// stops asking and reports it as lost rather than pending forever.
func TestAnAuthorSaysWhenALineIsBeyondRecovery(t *testing.T) {
	net := transport.NewMemNet(13)
	a, b := newPairedNodes(t, net)

	a.SendChat("one")
	waitForChat(t, b, "one")

	// Seq 2 was never sent, so a cannot supply it: the same answer it would
	// give for a line that has aged out of its ring.
	b.chatGaps.Observe(pb.ChatMessage{
		Sender: a.cfg.AdvertiseAddr, Node: a.Model.NodeID, Seq: 3, Text: "later",
	})
	b.pullChatGaps()
	waitFor(t, "the author to say the line is gone", func() bool { return b.Metrics.ChatLost.Load() > 0 })

	gaps := b.ChatGaps()
	if len(gaps) != 1 || gaps[0].Outstanding != 0 || gaps[0].Lost != 1 {
		t.Fatalf("chat gaps = %+v, want 1 lost and nothing outstanding", gaps)
	}
}

// The baseline has to survive a restart, because a restart is exactly when a
// line is most likely to have been missed: the node was not running.
func TestChatSequenceSeedingSurvivesARestart(t *testing.T) {
	tr := newChatTracker()
	// What the state file holds: alice's lines 1..5.
	var log []pb.ChatEntry
	for s := uint64(1); s <= 5; s++ {
		log = append(log, pb.ChatEntry{Sender: "10.0.0.1:3137", Node: "alice", Seq: s, Text: "x"})
	}
	tr.Seed(log)

	// Nothing is claimed missing just because it was loaded in bulk.
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("seeding invented %d gap(s)", n)
	}

	// Line 6 was said while this node was down; 7 arrives once it is back.
	tr.Observe(pb.ChatMessage{Sender: "10.0.0.1:3137", Node: "alice", Seq: 7, Text: "after"})
	if n := tr.outstanding(); n != 1 {
		t.Fatalf("outstanding = %d, want 1 — the line missed during the restart was not noticed", n)
	}
	plans := tr.PullPlans()
	if len(plans) != 1 || len(plans[0].Seqs) != 1 || plans[0].Seqs[0] != 6 {
		t.Fatalf("plans = %+v, want a request for seq 6", plans)
	}
}

// A snapshot is a bounded slice of someone else's ring, so what it does not
// contain is not evidence of a loss. Seeding must never invent a gap, or the
// node asks the author for lines nobody ever said.
func TestChatSequenceSeedingNeverInventsAGap(t *testing.T) {
	tr := newChatTracker()
	tr.Seed([]pb.ChatEntry{
		{Sender: "10.0.0.1:3137", Node: "alice", Seq: 40, Text: "a"},
		{Sender: "10.0.0.1:3137", Node: "alice", Seq: 44, Text: "b"},
	})
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("seeding claimed %d gap(s) from a truncated snapshot", n)
	}
	if plans := tr.PullPlans(); len(plans) != 0 {
		t.Fatalf("asked for lines a snapshot merely did not include: %+v", plans)
	}
	// And it took the highest it saw as the baseline.
	tr.Observe(pb.ChatMessage{Sender: "10.0.0.1:3137", Node: "alice", Seq: 45})
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("outstanding = %d, want 0 — the baseline was not raised to 44", n)
	}
}

// Synced history that happens to contain a line this node had already noticed
// was missing closes that gap, rather than leaving it to be asked for again.
func TestChatSequenceSeedingClosesAKnownGap(t *testing.T) {
	tr := newChatTracker()
	tr.Observe(pb.ChatMessage{Sender: "10.0.0.1:3137", Node: "alice", Seq: 1})
	tr.Observe(pb.ChatMessage{Sender: "10.0.0.1:3137", Node: "alice", Seq: 3})
	if n := tr.outstanding(); n != 1 {
		t.Fatalf("outstanding = %d, want 1", n)
	}

	tr.Seed([]pb.ChatEntry{{Sender: "10.0.0.1:3137", Node: "alice", Seq: 2, Text: "the missing one"}})
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("outstanding = %d, want 0 — the snapshot held the line and the gap stayed open", n)
	}
	if got := tr.Stats()[0].Recovered; got != 1 {
		t.Fatalf("recovered = %d, want 1", got)
	}
}

// An entry from before the field carries no author id; falling back to the
// address keeps it seeding something rather than nothing.
func TestChatSequenceSeedingToleratesAnEntryWithNoAuthorID(t *testing.T) {
	tr := newChatTracker()
	tr.Seed([]pb.ChatEntry{{Sender: "10.0.0.1:3137", Seq: 9, Text: "old format"}})
	tr.Observe(pb.ChatMessage{Sender: "10.0.0.1:3137", Seq: 11})
	if n := tr.outstanding(); n != 1 {
		t.Fatalf("outstanding = %d, want 1 — an address-keyed baseline did not apply", n)
	}
}

// A node never receives its own broadcast: it appends its own lines on send,
// and no peer echoes them back. Taking one is how a single bad line spreads —
// it lands in this node's log, is handed out in every state snapshot
// afterwards, and every node that syncs seeds its high-water mark for this
// author from it. One forged sequence then silences the author cluster-wide.
func TestAChatLineAttributedToOurselfIsRefused(t *testing.T) {
	net := transport.NewMemNet(23)
	a, b := newPairedNodes(t, net)

	a.SendChat("said by a")
	waitForChat(t, b, "said by a")

	// b hands a a line claiming a wrote it, numbered far ahead.
	b.send(a.cfg.AdvertiseAddr, pb.KindChat, pb.ChatMessage{
		Sender: a.cfg.AdvertiseAddr, Node: a.Model.NodeID, Nick: "a",
		Text: "NOT-SAID-BY-A", TS: time.Now().Unix(), Seq: 900001,
	})
	waitFor(t, "the self-attributed line to be refused", func() bool {
		return a.Metrics.ChatSelfAttributed.Load() > 0
	})
	for _, e := range a.Model.ChatLog() {
		if e.Text == "NOT-SAID-BY-A" {
			t.Fatal("took a line claiming to be our own")
		}
	}

	// The log is what every state snapshot is built from, so keeping the line
	// out of it is what stops one forged sequence spreading to every node that
	// syncs from here.
	for _, e := range a.Model.ChatLog() {
		if e.Seq == 900001 {
			t.Fatal("a forged sequence reached the log that snapshots are built from")
		}
	}

	// And this node's own numbering is untouched: its next line follows the
	// one before it rather than the forgery.
	a.SendChat("still audible")
	waitForChat(t, b, "still audible")
	for _, e := range a.Model.ChatLog() {
		if e.Text == "still audible" && e.Seq != 2 {
			t.Fatalf("own sequence = %d, want 2 — the forgery moved our counter", e.Seq)
		}
	}
}

// A heartbeat says where an author's numbering stands, so the baseline is set
// when two nodes meet rather than when one of them first speaks.
func TestChatSequenceAnnouncementSetsTheBaselineOnMeeting(t *testing.T) {
	tr := newChatTracker()
	// Meeting a peer that has already said four things claims nothing: we were
	// not there for them.
	tr.Announce("bob", "10.0.0.2:3137", 4)
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("meeting a peer claimed %d gap(s) from a conversation we were not in", n)
	}

	// Now we are there. bob says a fifth and we hear it.
	tr.Observe(pb.ChatMessage{Sender: "10.0.0.2:3137", Node: "bob", Seq: 5})
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("outstanding = %d, want 0", n)
	}

	// We go away. bob says three more. The next heartbeat is what tells us —
	// and it does not need bob to speak again.
	tr.Announce("bob", "10.0.0.2:3137", 8)
	if n := tr.outstanding(); n != 3 {
		t.Fatalf("outstanding = %d, want 3 (seqs 6, 7, 8)", n)
	}
	plans := tr.PullPlans()
	if len(plans) != 1 || len(plans[0].Seqs) != 3 {
		t.Fatalf("plans = %+v, want one asking for three lines", plans)
	}
}

// The measured case: a peer that had never spoken said three lines while this
// node was off the air, and nothing was reported — the first line heard
// afterwards became the baseline and the three were gone in silence.
func TestChatSequenceNoticesLinesFromAnAuthorNeverHeardBefore(t *testing.T) {
	tr := newChatTracker()
	tr.Announce("bob", "10.0.0.2:3137", 0) // met at join, bob has said nothing
	// bob says three while we cannot hear, then one we do hear.
	tr.Announce("bob", "10.0.0.2:3137", 3)
	tr.Observe(pb.ChatMessage{Sender: "10.0.0.2:3137", Node: "bob", Seq: 4})

	if n := tr.outstanding(); n != 3 {
		t.Fatalf("outstanding = %d, want 3 — the quiet author's lines are still invisible", n)
	}
}

// A stale or reordered heartbeat must not rewind anything.
func TestChatSequenceAnnouncementNeverGoesBackwards(t *testing.T) {
	tr := newChatTracker()
	tr.Announce("bob", "10.0.0.2:3137", 9)
	tr.Announce("bob", "10.0.0.2:3137", 4)
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("a stale heartbeat invented %d gap(s)", n)
	}
	tr.Observe(pb.ChatMessage{Sender: "10.0.0.2:3137", Node: "bob", Seq: 10})
	if n := tr.outstanding(); n != 0 {
		t.Fatalf("outstanding = %d, want 0 — the mark was rewound", n)
	}
}

// The wiring: a heartbeat between two real nodes has to carry the sequence, or
// the tracker's announcement rule never runs.
func TestAHeartbeatCarriesTheAuthorsChatSequence(t *testing.T) {
	net := transport.NewMemNet(29)
	a, b := newPairedNodes(t, net)

	a.SendChat("one")
	a.SendChat("two")
	waitForChat(t, b, "two")

	// A third node meets a mid-conversation and must claim nothing...
	c := newTestNode(t, net, "10.0.0.3:3137")
	c.Model.AddPeer(a.cfg.AdvertiseAddr)
	a.Model.AddPeer(c.cfg.AdvertiseAddr)
	a.helloAllPeers()
	waitFor(t, "c to learn where a's numbering stands", func() bool {
		return len(c.ChatGaps()) == 0 && c.Metrics.ChatGapsFound.Load() == 0
	})

	// ...but a line said while c cannot hear is named by the next heartbeat,
	// without a needing to speak again.
	net.Partition(a.cfg.AdvertiseAddr, c.cfg.AdvertiseAddr, true)
	a.SendChat("said while c was deaf")
	net.Partition(a.cfg.AdvertiseAddr, c.cfg.AdvertiseAddr, false)
	a.helloAllPeers()

	waitFor(t, "the heartbeat to reveal the missing line", func() bool {
		return c.Metrics.ChatGapsFound.Load() > 0
	})
	c.pullChatGaps()
	waitForChat(t, c, "said while c was deaf")
}

// The gaps inside an author were bounded from the start; the authors were not.
// Every heartbeat from every peer names one, and a node that restarts without
// a state file comes back with a new id, so the set grew with cluster churn
// and nothing in the protocol ever shrank it.
func TestChatSequenceBoundsHowManyAuthorsItFollows(t *testing.T) {
	tr := newChatTracker()
	for i := 0; i < maxTrackedAuthors*3; i++ {
		tr.Announce(fmt.Sprintf("author-%d", i), fmt.Sprintf("10.0.0.1:%d", 3000+i), 1)
	}
	tr.mu.Lock()
	n := len(tr.senders)
	tr.mu.Unlock()
	if n > maxTrackedAuthors {
		t.Fatalf("following %d authors, want at most %d", n, maxTrackedAuthors)
	}

	// The most recent survive, and an author dropped and heard from again is
	// simply met anew — which claims nothing rather than inventing a gap.
	last := fmt.Sprintf("author-%d", maxTrackedAuthors*3-1)
	tr.mu.Lock()
	_, keptLast := tr.senders[last]
	tr.mu.Unlock()
	if !keptLast {
		t.Fatal("the most recently active author was dropped")
	}
	tr.Announce("author-0", "10.0.0.1:3000", 50)
	if got := tr.outstanding(); got != 0 {
		t.Fatalf("a re-met author claimed %d gap(s)", got)
	}
}

// Activity keeps an author alive, so a busy peer is never dropped in favour of
// one nothing has been heard from.
func TestChatSequenceKeepsTheAuthorsStillTalking(t *testing.T) {
	tr := newChatTracker()
	tr.Announce("busy", "10.0.0.2:3137", 1)
	for i := 0; i < maxTrackedAuthors*2; i++ {
		tr.Announce(fmt.Sprintf("other-%d", i), fmt.Sprintf("10.0.0.1:%d", 3000+i), 1)
		// busy keeps speaking throughout.
		tr.Observe(pb.ChatMessage{Sender: "10.0.0.2:3137", Node: "busy", Seq: uint64(i + 2)})
	}
	tr.mu.Lock()
	_, kept := tr.senders["busy"]
	tr.mu.Unlock()
	if !kept {
		t.Fatal("the one author that never stopped talking was evicted")
	}
}
