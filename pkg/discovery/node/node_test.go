package node

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// idleInterval keeps the background loops from firing during a test: the tests
// drive gossip and anti-entropy explicitly, so a round is a step the test took
// rather than a timer that happened to land.
const idleInterval = time.Hour

func newTestNode(t *testing.T, net *transport.MemNet, addr string) *Node {
	t.Helper()
	n, err := New(Config{
		NodeAddr:          addr,
		Nick:              addr,
		Transport:         net.Node(addr),
		GossipInterval:    idleInterval,
		HeartbeatInterval: idleInterval,
		EvictThreshold:    idleInterval,
		SweepInterval:     idleInterval,
	}, nil)
	if err != nil {
		t.Fatalf("new node %s: %v", addr, err)
	}
	n.Start()
	t.Cleanup(n.Stop)
	return n
}

// newPairedNodes builds two nodes that already know about each other.
func newPairedNodes(t *testing.T, net *transport.MemNet) (*Node, *Node) {
	t.Helper()
	a := newTestNode(t, net, "10.0.0.1:3137")
	b := newTestNode(t, net, "10.0.0.2:3137")
	a.Model.AddPeer(b.Addr())
	b.Model.AddPeer(a.Addr())
	return a, b
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestReplicationReachesPeer(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)

	a.Set("k", "v", 0)
	waitFor(t, "peer to receive the write", func() bool {
		v, ok := b.Get("k")
		return ok && v == "v"
	})

	a.Delete("k")
	waitFor(t, "peer to receive the delete", func() bool {
		_, ok := b.Get("k")
		return !ok
	})
}

// The gap anti-entropy exists to close: a write whose datagram was dropped is
// never re-sent by the write path, so without reconciliation the two stores
// stay divergent forever.
func TestAntiEntropyRepairsDroppedWrite(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)

	net.SetLoss(1.0)
	a.Set("lost", "value", 0)
	time.Sleep(20 * time.Millisecond)
	if _, ok := b.Get("lost"); ok {
		t.Fatal("write arrived despite total packet loss")
	}

	net.SetLoss(0)
	a.antiEntropyRound(b.Addr())
	waitFor(t, "anti-entropy to repair the peer", func() bool {
		v, ok := b.Get("lost")
		return ok && v == "value"
	})
	if b.Metrics.AEPulled.Load() == 0 {
		t.Fatal("peer never issued a pull request")
	}
}

// Reconciliation has to work in both directions from a single digest.
func TestAntiEntropyPushesAndPulls(t *testing.T) {
	net := transport.NewMemNet(2)
	a, b := newPairedNodes(t, net)

	net.SetLoss(1.0)
	a.Set("only-a", "1", 0)
	b.Set("only-b", "2", 0)
	time.Sleep(20 * time.Millisecond)
	net.SetLoss(0)

	a.antiEntropyRound(b.Addr())
	waitFor(t, "both sides to converge", func() bool {
		_, aHasB := a.Get("only-b")
		_, bHasA := b.Get("only-a")
		return aHasB && bHasA
	})
}

// A tombstone must survive reconciliation: if the delete is not replicated as a
// versioned tombstone, the peer that still holds the value pushes it back and
// the key returns from the dead.
func TestAntiEntropyDoesNotResurrectDeletedKeys(t *testing.T) {
	net := transport.NewMemNet(3)
	a, b := newPairedNodes(t, net)

	a.Set("k", "v", 0)
	waitFor(t, "peer to receive the write", func() bool {
		_, ok := b.Get("k")
		return ok
	})

	net.SetLoss(1.0)
	a.Delete("k")
	time.Sleep(20 * time.Millisecond)
	net.SetLoss(0)

	// b still holds the value and gets to advertise first.
	b.antiEntropyRound(a.Addr())
	waitFor(t, "the delete to win", func() bool {
		_, ok := b.Get("k")
		return !ok
	})
	if _, ok := a.Get("k"); ok {
		t.Fatal("deleted key came back on the deleting node")
	}
}

// A whole cluster has to converge on its own under sustained loss — the
// property the digest exchange is there to provide.
func TestClusterConvergesUnderPacketLoss(t *testing.T) {
	net := transport.NewMemNet(7)
	const nodes = 5

	cluster := make([]*Node, 0, nodes)
	for i := 0; i < nodes; i++ {
		cluster = append(cluster, newTestNode(t, net, fmt.Sprintf("10.0.0.%d:3137", i+1)))
	}
	for _, n := range cluster {
		for _, peer := range cluster {
			if peer != n {
				n.Model.AddPeer(peer.Addr())
			}
		}
	}

	net.SetLoss(0.3)
	for i, n := range cluster {
		for k := 0; k < 4; k++ {
			n.Set(fmt.Sprintf("key-%d-%d", i, k), fmt.Sprintf("value-%d-%d", i, k), 0)
		}
	}

	want := nodes * 4
	converged := func() bool {
		for _, n := range cluster {
			if n.Model.Store.Len() != want {
				return false
			}
		}
		return true
	}
	// Each round is one digest exchange per ordered pair; loss makes some of
	// them useless, which is exactly what repeated rounds are for.
	for round := 0; round < 60 && !converged(); round++ {
		for _, n := range cluster {
			for _, peer := range cluster {
				if peer != n {
					n.antiEntropyRound(peer.Addr())
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}

	if !converged() {
		for _, n := range cluster {
			t.Logf("%s holds %d keys", n.Addr(), n.Model.Store.Len())
		}
		t.Fatal("cluster did not converge under 30% packet loss")
	}

	reference := cluster[0].Model.Store.Snapshot()
	for _, n := range cluster[1:] {
		if got := n.Model.Store.Snapshot(); !reflect.DeepEqual(got, reference) {
			t.Fatalf("%s diverged from the reference snapshot", n.Addr())
		}
	}
}

// A store larger than a datagram used to sync as nothing at all: the send
// simply failed and the joiner was left empty.
func TestStateSyncOverStreamCarriesOversizedStore(t *testing.T) {
	net := transport.NewMemNet(4)
	a, b := newPairedNodes(t, net)

	value := strings.Repeat("x", 2048)
	const keys = 100 // ~200 KB, far past what one datagram can hold
	for i := 0; i < keys; i++ {
		a.Set(fmt.Sprintf("key-%03d", i), value, 0)
	}
	waitFor(t, "the peer to see the writes", func() bool { return b.Model.Store.Len() == keys })

	// A fresh joiner pulls the whole snapshot in one exchange.
	c := newTestNode(t, net, "10.0.0.3:3137")
	c.Model.AddPeer(a.Addr())
	c.requestStateFrom(a.Addr())

	waitFor(t, "the joiner to receive the whole store", func() bool {
		return c.Model.Store.Len() == keys
	})
	if c.Metrics.StreamErrors.Load() != 0 {
		t.Fatalf("stream errors during state sync: %d", c.Metrics.StreamErrors.Load())
	}
}

// With no stream available the datagram path still has to work, bounded.
func TestStateSyncFallsBackToDatagram(t *testing.T) {
	net := transport.NewMemNet(5)
	a, b := newPairedNodes(t, net)
	a.Set("k", "v", 0)
	waitFor(t, "peer to receive the write", func() bool { _, ok := b.Get("k"); return ok })

	c := newTestNode(t, net, "10.0.0.3:3137")
	c.Model.AddPeer(a.Addr())
	// Cut streams only: Dial fails, datagrams still flow.
	net.Partition("10.0.0.3:3137", a.Addr(), true)
	net.Partition("10.0.0.3:3137", a.Addr(), false)

	c.send(a.Addr(), pb.KindStateRequest, pb.StateRequest{From: c.Addr()})
	waitFor(t, "datagram state sync", func() bool { _, ok := c.Get("k"); return ok })
}

func TestChatReachesPeer(t *testing.T) {
	net := transport.NewMemNet(6)
	a, b := newPairedNodes(t, net)

	a.SendChat("hello cluster")
	waitFor(t, "chat to arrive", func() bool {
		for _, e := range b.Chat() {
			if e.Text == "hello cluster" && e.Kind == pb.ChatMsg {
				return true
			}
		}
		return false
	})
}

// A direct message goes to one peer only.
func TestDirectMessage(t *testing.T) {
	net := transport.NewMemNet(8)
	a := newTestNode(t, net, "10.0.0.1:3137")
	b := newTestNode(t, net, "10.0.0.2:3137")
	c := newTestNode(t, net, "10.0.0.3:3137")
	for _, pair := range [][2]*Node{{a, b}, {a, c}, {b, c}} {
		pair[0].Model.AddPeer(pair[1].Addr())
		pair[1].Model.AddPeer(pair[0].Addr())
	}
	b.Model.SetNick(b.Addr(), "bob")
	a.Model.SetNick(b.Addr(), "bob")

	a.Submit("/msg bob psst")
	waitFor(t, "dm to reach bob", func() bool {
		for _, e := range b.Chat() {
			if e.Text == "psst" && e.Kind == pb.ChatDirect {
				return true
			}
		}
		return false
	})
	time.Sleep(20 * time.Millisecond)
	for _, e := range c.Chat() {
		if e.Text == "psst" {
			t.Fatal("direct message leaked to a third node")
		}
	}
}

func TestChatCommands(t *testing.T) {
	net := transport.NewMemNet(9)
	a, _ := newPairedNodes(t, net)

	a.Submit("/set colour blue")
	if v, ok := a.Get("colour"); !ok || v != "blue" {
		t.Fatalf("/set did not write: (%q, %v)", v, ok)
	}
	a.Submit("/del colour")
	if _, ok := a.Get("colour"); ok {
		t.Fatal("/del did not delete")
	}
	a.Submit("/setttl token 60 secret")
	e, ok := a.Entry("token")
	if !ok || e.ExpiresAt == 0 {
		t.Fatalf("/setttl did not set an expiry: %+v", e)
	}
	a.Submit("/nick zoe")
	if got := a.Model.Nick(); got != "zoe" {
		t.Fatalf("nick = %q, want zoe", got)
	}
	a.Submit("/bogus")
	last := a.Chat()[len(a.Chat())-1]
	if !strings.Contains(last.Text, "unknown command") {
		t.Fatalf("unknown command feedback = %q", last.Text)
	}
}

func TestGoodbyeDropsPeerImmediately(t *testing.T) {
	net := transport.NewMemNet(10)
	a, b := newPairedNodes(t, net)

	b.Stop()
	waitFor(t, "the peer to be dropped", func() bool { return !a.Model.HasPeer(b.Addr()) })

	// Wait on the line itself rather than reading it right after the peer
	// disappears: the handler removes the peer first and posts the line second,
	// so the two are not observable at the same instant.
	waitFor(t, "the 'left' line to be posted", func() bool {
		for _, e := range a.Chat() {
			if e.Kind == pb.ChatSystem && strings.Contains(e.Text, "left") {
				return true
			}
		}
		return false
	})
}

func TestCompareAndSetReplicates(t *testing.T) {
	net := transport.NewMemNet(11)
	a, b := newPairedNodes(t, net)

	if _, ok := a.CompareAndSet("lock", "a", 0, pb.Version{}); !ok {
		t.Fatal("claiming a free key failed")
	}
	waitFor(t, "the claim to replicate", func() bool { _, ok := b.Get("lock"); return ok })

	if _, ok := b.CompareAndSet("lock", "b", 0, pb.Version{}); ok {
		t.Fatal("second node claimed a held lock")
	}
	if b.Metrics.KVCASFailures.Load() == 0 {
		t.Fatal("refused compare-and-swap was not counted")
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	net := transport.NewMemNet(12)
	a, _ := newPairedNodes(t, net)
	a.Set("one", "1", 0)
	a.Set("two", "2", 0)

	data, err := a.Export()
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	c := newTestNode(t, net, "10.0.0.9:3137")
	n, err := c.Import(data, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if n != 2 {
		t.Fatalf("imported %d entries, want 2", n)
	}
	if v, ok := c.Get("one"); !ok || v != "1" {
		t.Fatalf("imported value = (%q, %v)", v, ok)
	}
}

// A merge import must not overwrite a newer value the cluster already has;
// seeding, which re-stamps every entry as a local write, must.
func TestImportMergeRespectsVersionsAndSeedOverrides(t *testing.T) {
	net := transport.NewMemNet(13)
	a, _ := newPairedNodes(t, net)
	a.Set("k", "old", 0)
	data, _ := a.Export()

	b := newTestNode(t, net, "10.0.0.9:3137")
	for i := 0; i < 5; i++ {
		b.Set("k", "new", 0) // several writes: a strictly higher counter
	}

	if _, err := b.Import(data, false); err != nil {
		t.Fatalf("merge import: %v", err)
	}
	if v, _ := b.Get("k"); v != "new" {
		t.Fatalf("merge import clobbered a newer value: %q", v)
	}

	if _, err := b.Import(data, true); err != nil {
		t.Fatalf("seed import: %v", err)
	}
	if v, _ := b.Get("k"); v != "old" {
		t.Fatalf("seed import did not win: %q", v)
	}
}

// Expiry is deterministic, so replicas agree without exchanging a message.
func TestTTLExpiresOnEveryReplica(t *testing.T) {
	net := transport.NewMemNet(14)
	a, b := newPairedNodes(t, net)

	now := time.Now()
	clock := func() time.Time { return now }
	a.Model.Store.SetClock(clock)
	b.Model.Store.SetClock(clock)

	a.Set("session", "token", 30*time.Second)
	waitFor(t, "the key to replicate", func() bool { _, ok := b.Get("session"); return ok })

	now = now.Add(31 * time.Second)
	if _, ok := a.Get("session"); ok {
		t.Fatal("key still readable on the writer after expiry")
	}
	if _, ok := b.Get("session"); ok {
		t.Fatal("key still readable on the replica after expiry")
	}
}

// Packets from another cluster must be ignored entirely, not merged.
func TestForeignClusterPacketsAreRejected(t *testing.T) {
	net := transport.NewMemNet(15)
	a := newTestNode(t, net, "10.0.0.1:3137")

	intruder := pb.NewCodec("", "someone-elses-cluster")
	frame, err := intruder.Encode(pb.KindKV, pb.KVUpdate{
		Action:  pb.KVSet,
		Key:     "injected",
		Value:   "evil",
		Version: pb.Version{Counter: 99, Node: "attacker"},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	net.Node("10.9.9.9:1").Send(a.Addr(), frame)

	waitFor(t, "the packet to be counted as a failure", func() bool {
		return a.Metrics.AuthFailures.Load() > 0
	})
	if _, ok := a.Get("injected"); ok {
		t.Fatal("a foreign cluster wrote into the store")
	}
}

// The activity feed is the only place a rejected update is visible, which is
// precisely when a replication bug would otherwise hide.
func TestActivityFeedRecordsStaleRejections(t *testing.T) {
	net := transport.NewMemNet(16)
	a := newTestNode(t, net, "10.0.0.1:3137")
	for i := 0; i < 3; i++ {
		a.Set("k", "current", 0)
	}

	a.handleKV(mustJSON(t, pb.KVUpdate{
		Action:  pb.KVSet,
		Key:     "k",
		Value:   "outdated",
		Version: pb.Version{Counter: 1, Node: "some-peer"},
	}))

	if v, _ := a.Get("k"); v != "current" {
		t.Fatalf("value = %q, want the newer local write to survive", v)
	}
	var found bool
	for _, line := range a.Activity() {
		if strings.Contains(line, "ignored stale") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no stale-rejection line in the activity feed: %v", a.Activity())
	}
	if a.Metrics.KVRejectedStale.Load() == 0 {
		t.Fatal("stale rejection not counted")
	}
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}
