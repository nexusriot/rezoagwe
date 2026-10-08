package model

import (
	"errors"
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

	if _, err := kv.Write("k", "12345678", WriteOptions{}); err != nil {
		t.Fatalf("a value exactly at the limit was refused: %v", err)
	}
	if _, err := kv.Write("k", "123456789", WriteOptions{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a value over the limit returned %v, want ErrTooLarge", err)
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
	if _, err := kv.Write("c", "1", WriteOptions{}); !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("a third key past MaxKeys=2 returned %v, want ErrTooManyKeys", err)
	}
	if _, err := kv.Write("a", "2", WriteOptions{}); err != nil {
		t.Fatalf("an update to a key already held was refused: %v", err)
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
	if _, err := kv.Write("b", "1", WriteOptions{}); err == nil {
		t.Fatal("second key accepted at MaxKeys=1")
	}
	kv.Delete("a")
	if _, err := kv.Write("b", "1", WriteOptions{}); err != nil {
		t.Fatalf("a key was refused after the only live key was deleted: %v", err)
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

// A value no peer could ever receive is refused where it is written, not
// stored and reported as written.
//
// Found by end-to-end testing: the datagram path refuses it on size, the
// stream path refuses the frame, and a state sync builds that same frame — so
// it was accepted locally and sat on one replica forever with nothing able to
// repair it. Unlike MaxValueBytes this is not configurable, because it is not
// a policy: no setting makes such a value replicate.
func TestAValueTooLargeToReplicateIsRefusedWithoutAnyLimitSet(t *testing.T) {
	kv := NewKVStore("n1")

	ok := strings.Repeat("x", pb.MaxReplicableValueBytes)
	if _, err := kv.Write("fits", ok, WriteOptions{}); err != nil {
		t.Fatalf("a value exactly at the ceiling was refused: %v", err)
	}
	if _, err := kv.Write("over", ok+"x", WriteOptions{}); !errors.Is(err, ErrUnshippable) {
		t.Fatalf("a value over the ceiling returned %v, want ErrUnshippable", err)
	}
	if _, present := kv.Get("over"); present {
		t.Fatal("a refused value was stored anyway")
	}
}

// The ceiling is measured as the value will be written, not as it sits in
// memory. Bytes that are not valid UTF-8 become U+FFFD — six bytes each — so a
// blob a sixth of the ceiling is already at it, and a raw length check would
// wave it through to strand itself.
func TestTheReplicationCeilingCountsEscapedBytes(t *testing.T) {
	kv := NewKVStore("n1")

	// Comfortably under the ceiling by raw length, far over it once encoded.
	binary := strings.Repeat("\xff", pb.MaxReplicableValueBytes/4)
	if len(binary) >= pb.MaxReplicableValueBytes {
		t.Fatal("the test value is not under the ceiling by raw length")
	}
	if _, err := kv.Write("blob", binary, WriteOptions{}); !errors.Is(err, ErrUnshippable) {
		t.Fatalf("a blob that sextuples on the wire returned %v, want ErrUnshippable", err)
	}
}

// A peer cannot push past the ceiling either — though in practice it could
// never have sent one, since the same limit stops it leaving.
func TestAnUnshippableRemoteApplyIsRefused(t *testing.T) {
	kv := NewKVStore("n1")
	u := pb.KVUpdate{
		Action:  pb.KVSet,
		Key:     "k",
		Value:   strings.Repeat("x", pb.MaxReplicableValueBytes+1),
		Version: pb.Version{Counter: 9, Node: "peer"},
	}
	if kv.Apply(u) {
		t.Fatal("an unshippable remote update was applied")
	}
}

// Why a write was refused has to be answerable: "false" reads the same for a
// lost race and a value that will never fit, and the caller owes the user
// different answers.
func TestARefusedWriteSaysWhichRuleRefusedIt(t *testing.T) {
	kv := NewKVStore("n1")
	kv.SetLimits(Limits{MaxValueBytes: 4, MaxKeys: 1})

	if _, err := kv.Write("a", "toolong", WriteOptions{}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over the size limit: %v, want ErrTooLarge", err)
	}
	if _, err := kv.Write("a", "ok", WriteOptions{}); err != nil {
		t.Fatalf("a fine write was refused: %v", err)
	}
	if _, err := kv.Write("b", "ok", WriteOptions{}); !errors.Is(err, ErrTooManyKeys) {
		t.Errorf("past the key limit: %v, want ErrTooManyKeys", err)
	}
	stale := pb.Version{Counter: 99, Node: "nobody"}
	if _, err := kv.Write("a", "ok", WriteOptions{Expect: &stale}); !errors.Is(err, ErrVersionMismatch) {
		t.Errorf("guarded against a stale version: %v, want ErrVersionMismatch", err)
	}
}

// Apply collapsed "I have something newer" and "I will not hold this at all"
// into one false, and every caller read the pair as staleness. They need
// opposite responses — one is last-write-wins working, the other is a replica
// that cannot converge — so the store has to say which it was.
func TestApplyWithReasonSeparatesRefusalFromStaleness(t *testing.T) {
	kv := NewKVStore("n1")
	kv.SetLimits(Limits{MaxValueBytes: 8, MaxKeys: 2})

	if applied, err := kv.ApplyWithReason(pb.KVUpdate{
		Action: pb.KVSet, Key: "a", Value: "ok", Version: pb.Version{Counter: 1, Node: "p"},
	}); !applied || err != nil {
		t.Fatalf("first write = (%v, %v), want (true, nil)", applied, err)
	}

	// Genuinely stale: the store already holds a newer version. No error.
	applied, err := kv.ApplyWithReason(pb.KVUpdate{
		Action: pb.KVSet, Key: "a", Value: "old", Version: pb.Version{Counter: 0, Node: "p"},
	})
	if applied || err != nil {
		t.Fatalf("stale write = (%v, %v), want (false, nil)", applied, err)
	}

	// Over the value limit: refused, and it says so.
	applied, err = kv.ApplyWithReason(pb.KVUpdate{
		Action: pb.KVSet, Key: "b", Value: "far too long to fit", Version: pb.Version{Counter: 9, Node: "p"},
	})
	if applied || !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized write = (%v, %v), want (false, ErrTooLarge)", applied, err)
	}

	// Past the key limit: refused, with the other reason.
	if applied, err := kv.ApplyWithReason(pb.KVUpdate{
		Action: pb.KVSet, Key: "b", Value: "fits", Version: pb.Version{Counter: 10, Node: "p"},
	}); !applied || err != nil {
		t.Fatalf("second key = (%v, %v), want (true, nil)", applied, err)
	}
	applied, err = kv.ApplyWithReason(pb.KVUpdate{
		Action: pb.KVSet, Key: "c", Value: "fits", Version: pb.Version{Counter: 11, Node: "p"},
	})
	if applied || !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("over-limit write = (%v, %v), want (false, ErrTooManyKeys)", applied, err)
	}

	// Apply keeps its bool contract for every caller that only asks "did the
	// store change?".
	if kv.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "d", Value: "fits", Version: pb.Version{Counter: 12, Node: "p"}}) {
		t.Fatal("Apply returned true for an update the limits refused")
	}
}
