package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

func TestLimitRefusesAnOversizeLocalWrite(t *testing.T) {
	kv := NewKVStore("n1")
	kv.SetLimits(Limits{MaxValueBytes: 8})

	if _, ok := kv.Write("k", "12345678", WriteOptions{}); !ok {
		t.Fatal("a value exactly at the limit was refused")
	}
	if _, ok := kv.Write("k", "123456789", WriteOptions{}); ok {
		t.Fatal("a value over the limit was accepted")
	}
	if v, _ := kv.Get("k"); v != "12345678" {
		t.Fatalf("refused write still landed: %q", v)
	}
}

// A limit has to hold against a peer as well as a local writer, or the node it
// protects is the only one that cannot fill it.
func TestLimitRefusesAnOversizeRemoteApply(t *testing.T) {
	kv := NewKVStore("n1")
	kv.SetLimits(Limits{MaxValueBytes: 4})

	u := pb.KVUpdate{
		Action:  pb.KVSet,
		Key:     "k",
		Value:   strings.Repeat("x", 64),
		Version: pb.Version{Counter: 9, Node: "peer"},
	}
	if kv.Apply(u) {
		t.Fatal("an oversize remote update was applied")
	}
	if _, ok := kv.Get("k"); ok {
		t.Fatal("key present after a refused remote update")
	}
	// The Lamport clock still advances: refusing the value is not a reason to
	// let this node's later writes sort before the one it refused.
	if kv.Clock() < 9 {
		t.Fatalf("clock = %d, want it advanced past the refused version", kv.Clock())
	}
}

func TestKeyLimitAllowsUpdatesToKeysAlreadyHeld(t *testing.T) {
	kv := NewKVStore("n1")
	kv.SetLimits(Limits{MaxKeys: 2})

	kv.Set("a", "1")
	kv.Set("b", "1")
	if _, ok := kv.Write("c", "1", WriteOptions{}); ok {
		t.Fatal("a third key was accepted past MaxKeys=2")
	}
	if _, ok := kv.Write("a", "2", WriteOptions{}); !ok {
		t.Fatal("an update to a key already held was refused")
	}
	if v, _ := kv.Get("a"); v != "2" {
		t.Fatalf("a = %q, want the updated value", v)
	}
}

// A deleted key frees its slot: the limit counts what is live, not what was
// ever written.
func TestKeyLimitCountsLiveKeysOnly(t *testing.T) {
	kv := NewKVStore("n1")
	kv.SetLimits(Limits{MaxKeys: 1})

	kv.Set("a", "1")
	if _, ok := kv.Write("b", "1", WriteOptions{}); ok {
		t.Fatal("second key accepted at MaxKeys=1")
	}
	kv.Delete("a")
	if _, ok := kv.Write("b", "1", WriteOptions{}); !ok {
		t.Fatal("a key was refused after the only live key was deleted")
	}
}

func TestValueBytesCountsLiveValuesOnly(t *testing.T) {
	kv := NewKVStore("n1")
	kv.Set("a", "1234")
	kv.Set("b", "12")
	if got := kv.ValueBytes(); got != 6 {
		t.Fatalf("ValueBytes = %d, want 6", got)
	}
	kv.Delete("a")
	if got := kv.ValueBytes(); got != 2 {
		t.Fatalf("ValueBytes after delete = %d, want 2", got)
	}
}

// Persistence is coalesced, so a burst of writes has to cost one file rewrite
// rather than one per write.
func TestPersistenceCoalescesABurst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.json")
	m := newModel(t, path)

	for i := 0; i < 200; i++ {
		m.Store.Set("k", "v")
	}
	m.Close()

	state, found, err := NewPersister(path).Load()
	if err != nil || !found {
		t.Fatalf("state not written: found=%v err=%v", found, err)
	}
	if state.Entries["k"].Value != "v" {
		t.Fatalf("last write not persisted: %+v", state.Entries["k"])
	}
}

// The debounce must not lose the final write: a node that shuts down cleanly
// has to leave the store it actually held.
func TestCloseFlushesTheLastWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.json")
	m := newModel(t, path)
	m.Store.Set("only", "value")
	m.Close()

	state, found, _ := NewPersister(path).Load()
	if !found || state.Entries["only"].Value != "value" {
		t.Fatalf("write lost across a clean shutdown: %+v", state.Entries)
	}
}

// A write with nothing after it still reaches disk on its own, without waiting
// for a shutdown that may never come.
func TestFlushLoopWritesWithoutAClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.json")
	m := newModel(t, path)
	m.Store.Set("k", "v")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if state, found, _ := NewPersister(path).Load(); found && state.Entries["k"].Value == "v" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the flush loop never wrote the state file")
}

// The state file is rewritten in place, so it must never be left indented —
// the size of every flush is the cost the coalescing exists to bound.
func TestStateFileIsCompact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.json")
	m := newModel(t, path)
	m.Store.Set("k", "v")
	m.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "\n  ") {
		t.Fatalf("state file is indented: %s", data)
	}
}

// A node told to advertise a different address must recognise both as itself,
// or gossip echoing its advertised address back adds it to its own peer list.
func TestIsSelfCoversTheAdvertisedAddress(t *testing.T) {
	m := NewModel(Config{NodeAddr: ":3137", AdvertiseAddr: "10.0.0.4:3137"})
	t.Cleanup(m.Close)

	for _, addr := range []string{":3137", "127.0.0.1:3137", "10.0.0.4:3137"} {
		if !m.IsSelf(addr) {
			t.Errorf("IsSelf(%q) = false, want true", addr)
		}
	}
	if m.IsSelf("10.0.0.5:3137") {
		t.Error("IsSelf matched a different host")
	}
	if m.AddPeer("10.0.0.4:3137") {
		t.Error("the node added its own advertised address as a peer")
	}
}

func TestAdvertiseDefaultsToTheBindAddress(t *testing.T) {
	m := NewModel(Config{NodeAddr: "127.0.0.1:3137"})
	t.Cleanup(m.Close)
	if m.AdvertiseAddr != "127.0.0.1:3137" {
		t.Fatalf("AdvertiseAddr = %q, want the bind address", m.AdvertiseAddr)
	}
}

// A key's bucket comes from its name alone, so a value that changes stays put.
// Bucketing on the whole entry relocates it on every write, which lights up two
// buckets for one stale value and makes "they differ in bucket 7" meaningless.
func TestFingerprintBucketsAKeyByNameNotByVersion(t *testing.T) {
	base := NewKVStore("n1")
	base.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "alpha", Value: "one",
		Version: pb.Version{Counter: 3, Node: "n1"}})
	base.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "beta", Value: "two",
		Version: pb.Version{Counter: 7, Node: "n2"}})

	moved := NewKVStore("n1")
	moved.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "alpha", Value: "one",
		Version: pb.Version{Counter: 3, Node: "n1"}})
	moved.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "beta", Value: "two",
		Version: pb.Version{Counter: 7, Node: "n2"}})
	moved.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "alpha", Value: "two",
		Version: pb.Version{Counter: 4, Node: "n1"}})

	a := base.Fingerprint(pb.FingerprintBuckets)
	b := moved.Fingerprint(pb.FingerprintBuckets)
	differing := 0
	for i := range a.Buckets {
		if a.Buckets[i] != b.Buckets[i] {
			differing++
		}
	}
	if differing != 1 {
		t.Fatalf("one key at a different version lit up %d buckets, want exactly 1", differing)
	}
}

// The vectors the desktop port is held to. If these change, every JavaScript
// node reports every Go node as divergent — so they are a protocol constant,
// not an implementation detail.
func TestFingerprintMatchesThePublishedVectors(t *testing.T) {
	kv := NewKVStore("node-a")
	kv.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "alpha", Value: "one",
		Version: pb.Version{Counter: 3, Node: "node-a"}})
	kv.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "beta", Value: "two",
		Version: pb.Version{Counter: 7, Node: "node-b"}})
	kv.Apply(pb.KVUpdate{Action: pb.KVDelete, Key: "gamma",
		Version: pb.Version{Counter: 9, Node: "node-a"}, DeletedAt: 1700000000})

	const zero = "0000000000000000000000000000000000000000000000000000000000000000"
	want := map[int]string{
		7:  "a2026aaa5ddb78cb47d1db8e5973b787420dafb7fc1ad30cca5944a5495f9898",
		13: "5afa98b8cab4abc9294acac825fad1b3f02d6125ec2f5b0def0456acb56e3abc",
	}

	f := kv.Fingerprint(pb.FingerprintBuckets)
	if f.Keys != 2 || f.Tombstones != 1 || f.Clock != 9 {
		t.Fatalf("counts = %d keys, %d tombstones, clock %d", f.Keys, f.Tombstones, f.Clock)
	}
	for i, got := range f.Buckets {
		expect := zero
		if v, ok := want[i]; ok {
			expect = v
		}
		if got != expect {
			t.Errorf("bucket %d = %s, want %s", i, got, expect)
		}
	}
}
