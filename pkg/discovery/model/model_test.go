package model

import (
	"net"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// A remote update wins only if its version is strictly newer than what's held.
func TestApplyLastWriteWins(t *testing.T) {
	kv := NewKVStore(":self")
	kv.Set("k", "local") // local write → version {1, ":self"}

	// Older counter: ignored.
	if kv.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "k", Value: "stale", Version: pb.Version{Counter: 0, Node: ":peer"}}) {
		t.Fatal("Apply of older version reported a change")
	}
	if v, _ := kv.Get("k"); v != "local" {
		t.Fatalf("k = %q, want %q after stale apply", v, "local")
	}

	// Newer counter: wins.
	if !kv.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "k", Value: "fresh", Version: pb.Version{Counter: 9, Node: ":peer"}}) {
		t.Fatal("Apply of newer version reported no change")
	}
	if v, _ := kv.Get("k"); v != "fresh" {
		t.Fatalf("k = %q, want %q after newer apply", v, "fresh")
	}
}

// A tombstone must block a stale set from resurrecting the key, but a newer
// set must still bring it back.
func TestTombstoneBlocksStaleResurrect(t *testing.T) {
	kv := NewKVStore(":self")
	kv.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "k", Value: "v", Version: pb.Version{Counter: 2, Node: ":a"}})
	kv.Apply(pb.KVUpdate{Action: pb.KVDelete, Key: "k", Version: pb.Version{Counter: 5, Node: ":a"}})

	if _, ok := kv.Get("k"); ok {
		t.Fatal("key present after delete")
	}
	// Stale set (older than the tombstone) must not resurrect.
	kv.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "k", Value: "zombie", Version: pb.Version{Counter: 3, Node: ":b"}})
	if _, ok := kv.Get("k"); ok {
		t.Fatal("stale set resurrected a tombstoned key")
	}
	// Newer set must resurrect.
	kv.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "k", Value: "back", Version: pb.Version{Counter: 6, Node: ":b"}})
	if v, ok := kv.Get("k"); !ok || v != "back" {
		t.Fatalf("newer set did not resurrect: (%q, %v)", v, ok)
	}
}

// Applying a remote version must advance the Lamport clock so this node's
// next local write sorts strictly after it.
func TestLamportClockAdvancesOnApply(t *testing.T) {
	kv := NewKVStore(":self")
	kv.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "k", Value: "v", Version: pb.Version{Counter: 100, Node: ":peer"}})

	u := kv.Set("mine", "x")
	if u.Version.Counter <= 100 {
		t.Fatalf("local write counter = %d, want > 100 (clock didn't advance)", u.Version.Counter)
	}
}

// Two nodes writing the same key concurrently must converge to the same value
// on both sides after exchanging updates — the whole point of versioning.
func TestConvergenceOnConcurrentWrite(t *testing.T) {
	a := NewKVStore(":a")
	b := NewKVStore(":b")

	ua := a.Set("k", "from-a") // version {1, ":a"}
	ub := b.Set("k", "from-b") // version {1, ":b"}

	a.Apply(ub)
	b.Apply(ua)

	va, _ := a.Get("k")
	vb, _ := b.Get("k")
	if va != vb {
		t.Fatalf("stores diverged: a=%q b=%q", va, vb)
	}
	// Equal counters → higher node id wins deterministically: ":b" > ":a".
	if va != "from-b" {
		t.Fatalf("converged on %q, want %q (node-id tiebreak)", va, "from-b")
	}
}

// The Lamport clock must be persisted: after a restart, a new local write has
// to outrank writes the node made before restarting.
func TestClockSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.json")

	m1 := NewModel(":9999", ":3137", "self", path)
	m1.Store.Set("k", "v1")
	last := m1.Store.Set("k", "v2")

	m2 := NewModel(":9999", ":3137", "self", path)
	u := m2.Store.Set("k", "v3")
	if u.Version.Counter <= last.Version.Counter {
		t.Fatalf("post-restart write counter = %d, want > %d (clock not restored)",
			u.Version.Counter, last.Version.Counter)
	}
}

// The startup-hang fix: DiscoverNodes must return promptly when bootstrap is
// unreachable, instead of blocking forever on a deadline-less read.
func TestDiscoverNodesTimesOutInsteadOfHanging(t *testing.T) {
	m := NewModel("127.0.0.1:1", ":3137", "self", "") // no UDP listener there
	m.DiscoverTimeout = 200 * time.Millisecond

	done := make(chan []string, 1)
	go func() { done <- m.DiscoverNodes() }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Fatalf("expected no peers from an unreachable bootstrap, got %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DiscoverNodes hung on an unreachable bootstrap")
	}
}

func TestDiscoverNodesParsesRoster(t *testing.T) {
	srv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen fake bootstrap: %v", err)
	}
	defer srv.Close()
	go func() {
		// A DISCOVER marshals to zero bytes (proto3 default enum), so the
		// datagram is empty — reply on any successful read, as bootstrap does.
		buf := make([]byte, 4096)
		_, addr, err := srv.ReadFromUDP(buf)
		if err != nil {
			return
		}
		_, _ = srv.WriteToUDP([]byte(":3138,:3139"), addr)
	}()

	m := NewModel(srv.LocalAddr().String(), ":3137", "self", "")
	m.DiscoverTimeout = 2 * time.Second
	if got, want := m.DiscoverNodes(), []string{":3138", ":3139"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DiscoverNodes = %v, want %v", got, want)
	}
}

// The headline persistence behavior: keys written by one node instance are
// present when a fresh instance loads the same data file — i.e. they survive
// a restart.
func TestPersistenceSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.json")

	m1 := NewModel(":9999", ":3137", "self", path)
	m1.Store.Set("hello", "world")
	m1.Store.Set("n", "42")
	m1.Store.Delete("n")

	m2 := NewModel(":9999", ":3137", "self", path)
	if got, ok := m2.Store.Get("hello"); !ok || got != "world" {
		t.Fatalf(`after restart Get("hello") = (%q, %v), want ("world", true)`, got, ok)
	}
	if _, ok := m2.Store.Get("n"); ok {
		t.Fatal(`deleted key "n" reappeared after restart`)
	}
}

// With no data path, nothing is written and there is no persistence wiring.
func TestNoPersistenceWhenPathEmpty(t *testing.T) {
	m := NewModel(":9999", ":3137", "self", "")
	m.Store.Set("k", "v") // must not panic or attempt any file I/O
	if got, _ := m.Store.Get("k"); got != "v" {
		t.Fatalf("Get = %q, want %q", got, "v")
	}
}

func TestPrependChatOrderAndCap(t *testing.T) {
	m := NewModel(":9999", ":3137", "self", "")
	m.AppendChat("local-1")
	m.AppendChat("local-2")

	m.PrependChat([]string{"hist-1", "hist-2"})

	want := []string{"hist-1", "hist-2", "local-1", "local-2"}
	if got := m.ChatLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ChatLog = %v, want %v", got, want)
	}

	// Empty history is a no-op.
	before := m.ChatLog()
	m.PrependChat(nil)
	if got := m.ChatLog(); !reflect.DeepEqual(got, before) {
		t.Fatalf("PrependChat(nil) changed the log: %v", got)
	}
}

// Locks the invariant handleGoodbye relies on: a departed peer is fully
// dropped, and its nick is gone after removal — so the handler must capture
// the nick *before* calling RemovePeer to render the "left" line.
func TestRemovePeerForgetsNick(t *testing.T) {
	m := NewModel(":9999", ":3137", "self", "")
	const peer = ":3138"

	if !m.AddPeer(peer) {
		t.Fatalf("AddPeer(%s) = false, want true for a new peer", peer)
	}
	m.SetNick(peer, "bob")

	if !m.HasPeer(peer) {
		t.Fatal("HasPeer = false after AddPeer")
	}

	nick := m.NickOf(peer) // what handleGoodbye captures before removal
	m.RemovePeer(peer)

	if m.HasPeer(peer) {
		t.Fatal("HasPeer = true after RemovePeer")
	}
	if got := m.NickOf(peer); got != "" {
		t.Fatalf("NickOf after RemovePeer = %q, want empty", got)
	}
	if nick != "bob" {
		t.Fatalf("captured nick = %q, want %q", nick, "bob")
	}
}
