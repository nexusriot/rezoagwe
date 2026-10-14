package node

import (
	"encoding/json"
	"testing"
	"time"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// chatTexts is what a node's log actually holds, for asserting delivery.
func chatTexts(n *Node) []string {
	var out []string
	for _, e := range n.Chat() {
		if e.Kind == pb.ChatSystem {
			continue
		}
		out = append(out, e.Text)
	}
	return out
}

func hasText(n *Node, want string) bool {
	for _, got := range chatTexts(n) {
		if got == want {
			return true
		}
	}
	return false
}

// The shape this feature exists for: A and C both know B, but not each other.
// A's broadcast reaches only B, and before relaying C never heard it — and had
// no way to know it had missed anything, because it did not know A existed.
func TestChatReachesAPeerTheAuthorDoesNotKnow(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")
	b := newTestNode(t, net, "10.0.0.2:3137")
	c := newTestNode(t, net, "10.0.0.3:3137")

	a.Model.AddPeer(b.Addr())
	b.Model.AddPeer(a.Addr())
	b.Model.AddPeer(c.Addr())
	c.Model.AddPeer(b.Addr())
	// A has gossiped its peer list to B, so B can see that A knows only B —
	// which is the evidence forwarding needs.
	b.Model.RecordPeerView(a.Addr(), []string{b.Addr()})

	a.SendChat("only B was told")

	waitFor(t, "C to receive the relayed line", func() bool {
		return hasText(c, "only B was told")
	})
	if b.Metrics.ChatRelayed.Load() == 0 {
		t.Error("B did not count the relay it performed")
	}
}

// In a full mesh the author reached everyone already, so nothing should be
// forwarded: the cost is paid only by the asymmetry that makes it necessary.
func TestChatIsNotRelayedWhenTheAuthorReachedEveryone(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")
	b := newTestNode(t, net, "10.0.0.2:3137")
	c := newTestNode(t, net, "10.0.0.3:3137")
	for _, pair := range [][2]*Node{{a, b}, {a, c}, {b, c}} {
		pair[0].Model.AddPeer(pair[1].Addr())
		pair[1].Model.AddPeer(pair[0].Addr())
	}
	// B and C both know what A can reach.
	b.Model.RecordPeerView(a.Addr(), []string{b.Addr(), c.Addr()})
	c.Model.RecordPeerView(a.Addr(), []string{b.Addr(), c.Addr()})

	a.SendChat("everyone heard this directly")

	waitFor(t, "both peers to receive it", func() bool {
		return hasText(b, "everyone heard this directly") && hasText(c, "everyone heard this directly")
	})
	// Give any stray forward time to land before asserting it did not happen.
	time.Sleep(50 * time.Millisecond)
	if relayed := b.Metrics.ChatRelayed.Load() + c.Metrics.ChatRelayed.Load(); relayed != 0 {
		t.Errorf("%d line(s) forwarded into a full mesh", relayed)
	}
}

// A relayed line must arrive once. Deduplication by (author, Seq) is what stops
// it circulating, and a duplicate on screen is what a broken relay looks like.
func TestRelayedChatIsNotShownTwice(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")
	b := newTestNode(t, net, "10.0.0.2:3137")
	c := newTestNode(t, net, "10.0.0.3:3137")

	// A fully-connected graph in which B and C have both been told, wrongly,
	// that A reaches only the other one — so both forward the same line and
	// each receives the other's copy.
	for _, pair := range [][2]*Node{{a, b}, {a, c}, {b, c}} {
		pair[0].Model.AddPeer(pair[1].Addr())
		pair[1].Model.AddPeer(pair[0].Addr())
	}
	b.Model.RecordPeerView(a.Addr(), []string{b.Addr()})
	c.Model.RecordPeerView(a.Addr(), []string{c.Addr()})

	a.SendChat("said once")

	waitFor(t, "both peers to receive it", func() bool {
		return hasText(b, "said once") && hasText(c, "said once")
	})
	time.Sleep(100 * time.Millisecond) // let any echo arrive

	for _, n := range []*Node{b, c} {
		count := 0
		for _, got := range chatTexts(n) {
			if got == "said once" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("%s holds the line %d times, want 1", n.Addr(), count)
		}
	}
	// The author must never take its own line back.
	if a.Metrics.ChatSelfAttributed.Load() > 0 {
		t.Log("the author rejected an echo of its own line, as it should")
	}
	for _, got := range chatTexts(a) {
		if got != "said once" {
			t.Errorf("author's log holds an unexpected line %q", got)
		}
	}
}

// The repair channel may only fill a gap that was asked for. Forwarding from it
// would be the one path in the system that introduces chat into the cluster.
func TestRecoveredChatIsNotRelayed(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")
	b := newTestNode(t, net, "10.0.0.2:3137")
	c := newTestNode(t, net, "10.0.0.3:3137")
	for _, pair := range [][2]*Node{{a, b}, {a, c}, {b, c}} {
		pair[0].Model.AddPeer(pair[1].Addr())
		pair[1].Model.AddPeer(pair[0].Addr())
	}
	// C believes A reaches B, so C forwards nothing and the repair path is the
	// only thing that can fill B's gap. Without this the relay heals it first —
	// which is the feature working, and no way to test what this test is about.
	c.Model.RecordPeerView(a.Addr(), []string{b.Addr(), c.Addr()})
	// B, meanwhile, has evidence that A reaches nobody but B, so B relays the
	// ordinary broadcasts it accepts — which is what makes "the repair added
	// none" a statement about the repair rather than about relaying being off.
	b.Model.RecordPeerView(a.Addr(), []string{b.Addr()})

	a.SendChat("first")
	waitForChat(t, b, "first")

	net.Partition(a.Addr(), b.Addr(), true)
	a.SendChat("the line B never got")
	net.Partition(a.Addr(), b.Addr(), false)

	a.SendChat("third")
	waitForChat(t, b, "third")
	if got := b.ChatGaps(); len(got) != 1 || got[0].Outstanding != 1 {
		t.Fatalf("chat gaps = %+v, want one outstanding", got)
	}

	// Everything up to here is ordinary broadcast traffic, which B does relay.
	// The question is only whether the *repair* adds to it.
	before := b.Metrics.ChatRelayed.Load()

	b.pullChatGaps()
	waitForChat(t, b, "the line B never got")

	if got := b.Metrics.ChatRelayed.Load(); got != before {
		t.Errorf("the repaired line was forwarded (%d relay(s)); a repair channel must never introduce a line",
			got-before)
	}
}

func TestChatRelayStopsAtTheHopLimit(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")
	b := newTestNode(t, net, "10.0.0.2:3137")
	a.Model.AddPeer(b.Addr())
	b.Model.AddPeer(a.Addr())

	a.relayChat(pb.ChatMessage{
		Sender: "10.0.0.9:3137", Node: "other", Text: "travelled far",
		Seq: 1, Hops: pb.MaxChatHops,
	})
	if a.Metrics.ChatRelayCapped.Load() != 1 {
		t.Error("a line at the hop limit was not counted as capped")
	}
	if a.Metrics.ChatRelayed.Load() != 0 {
		t.Error("a line at the hop limit was forwarded anyway")
	}
}

// Hops has to survive the wire, or the limit never applies.
func TestHopsRoundTripOnTheWire(t *testing.T) {
	c := pb.NewCodec("psk", "cluster")
	pkt, err := c.Encode(pb.KindChat, pb.ChatMessage{Sender: "a", Text: "x", Seq: 1, Hops: 2})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got pb.ChatMessage
	if _, err := pb.NewCodec("psk", "cluster").DecodeInto(pkt, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Hops != 2 {
		t.Errorf("hops = %d, want 2", got.Hops)
	}

	// An original broadcast must encode byte-for-byte as it did before the
	// field existed, or every other implementation's parity test breaks.
	plain, err := c.Encode(pb.KindChat, pb.ChatMessage{Sender: "a", Text: "x", Seq: 1})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, body, err := pb.NewCodec("psk", "cluster").Decode(plain); err != nil {
		t.Fatalf("decode: %v", err)
	} else if string(body) != `{"sender":"a","nick":"","text":"x","ts":0,"seq":1}` {
		t.Errorf("unrelayed chat body = %s, want no hops field", body)
	}
}

// mustEncodeBody marshals a message body the way a handler receives it: the
// codec has already authenticated the frame and stripped the header by then.
func mustEncodeBody(t *testing.T, v interface{}) []byte {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	return body
}

// Forwarding needs evidence of asymmetry, not the absence of evidence of
// symmetry. Treating an unknown author as unreachable turned one line into a
// packet per peer per peer on the end-to-end cluster.
func TestChatIsNotRelayedWithoutAViewOfTheAuthor(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")
	b := newTestNode(t, net, "10.0.0.2:3137")
	c := newTestNode(t, net, "10.0.0.3:3137")
	for _, pair := range [][2]*Node{{a, b}, {a, c}, {b, c}} {
		pair[0].Model.AddPeer(pair[1].Addr())
		pair[1].Model.AddPeer(pair[0].Addr())
	}
	// Nobody has heard A's peer list yet.

	a.SendChat("nothing is known about who heard this")
	waitFor(t, "both peers to receive it", func() bool {
		return hasText(b, "nothing is known about who heard this") &&
			hasText(c, "nothing is known about who heard this")
	})
	time.Sleep(50 * time.Millisecond)

	if relayed := b.Metrics.ChatRelayed.Load() + c.Metrics.ChatRelayed.Load(); relayed != 0 {
		t.Errorf("%d line(s) forwarded on no evidence at all", relayed)
	}
	for _, n := range []*Node{a, b, c} {
		if dup := n.Metrics.ChatDuplicates.Load(); dup != 0 {
			t.Errorf("%s counted %d duplicate(s) with nothing re-sending", n.Addr(), dup)
		}
	}
}
