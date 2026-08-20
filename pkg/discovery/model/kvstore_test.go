package model

import (
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
	kv.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "k", Value: "zombie", Version: pb.Version{Counter: 3, Node: ":b"}})
	if _, ok := kv.Get("k"); ok {
		t.Fatal("stale set resurrected a tombstoned key")
	}
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
	a := NewKVStore("node-a")
	b := NewKVStore("node-b")

	ua := a.Set("k", "from-a")
	ub := b.Set("k", "from-b")

	a.Apply(ub)
	b.Apply(ua)

	va, _ := a.Get("k")
	vb, _ := b.Get("k")
	if va != vb {
		t.Fatalf("stores diverged: a=%q b=%q", va, vb)
	}
	if va != "from-b" {
		t.Fatalf("converged on %q, want %q (node-id tiebreak)", va, "from-b")
	}
}

// Compare-and-swap is the primitive that makes a lock possible: the write must
// land only when the key still holds the version the caller saw.
func TestCompareAndSwap(t *testing.T) {
	kv := NewKVStore("self")
	first := kv.Set("lock", "alice")

	if _, ok := kv.Write("lock", "bob", WriteOptions{Expect: &pb.Version{Counter: 99, Node: "x"}}); ok {
		t.Fatal("CAS succeeded against the wrong version")
	}
	if v, _ := kv.Get("lock"); v != "alice" {
		t.Fatalf("value = %q after failed CAS, want alice", v)
	}
	if _, ok := kv.Write("lock", "bob", WriteOptions{Expect: &first.Version}); !ok {
		t.Fatal("CAS against the current version failed")
	}
	if v, _ := kv.Get("lock"); v != "bob" {
		t.Fatalf("value = %q after successful CAS, want bob", v)
	}
}

// A zero expected version means "the key must not exist" — claim semantics.
func TestCompareAndSwapRequiresAbsence(t *testing.T) {
	kv := NewKVStore("self")
	zero := pb.Version{}

	if _, ok := kv.Write("lock", "alice", WriteOptions{Expect: &zero}); !ok {
		t.Fatal("claiming a free key failed")
	}
	if _, ok := kv.Write("lock", "bob", WriteOptions{Expect: &zero}); ok {
		t.Fatal("claimed a key that was already held")
	}
	if v, _ := kv.Get("lock"); v != "alice" {
		t.Fatalf("holder = %q, want alice", v)
	}
}

func TestCompareAndDelete(t *testing.T) {
	kv := NewKVStore("self")
	u := kv.Set("k", "v")

	if _, ok := kv.Remove("k", WriteOptions{Expect: &pb.Version{Counter: 42}}); ok {
		t.Fatal("guarded delete succeeded against the wrong version")
	}
	if _, ok := kv.Remove("k", WriteOptions{Expect: &u.Version}); !ok {
		t.Fatal("guarded delete failed against the current version")
	}
	if _, ok := kv.Get("k"); ok {
		t.Fatal("key still present after guarded delete")
	}
}

// clockAt drives a store's notion of now, so TTL and GC are testable without
// sleeping.
func clockAt(kv *KVStore, t *time.Time) {
	kv.SetClock(func() time.Time { return *t })
}

func TestTTLHidesExpiredKey(t *testing.T) {
	now := time.Unix(1000, 0)
	kv := NewKVStore("self")
	clockAt(kv, &now)

	kv.Write("session", "token", WriteOptions{ExpiresAt: now.Add(10 * time.Second).Unix()})
	if _, ok := kv.Get("session"); !ok {
		t.Fatal("key missing before expiry")
	}

	now = now.Add(11 * time.Second)
	if _, ok := kv.Get("session"); ok {
		t.Fatal("expired key still readable")
	}
	if kv.Len() != 0 {
		t.Fatalf("Len = %d, want 0 with only an expired key", kv.Len())
	}
}

// Expiry must not bump the Lamport clock: a sweep that outranked a concurrent
// legitimate write would silently destroy it.
func TestSweepExpiredKeepsVersion(t *testing.T) {
	now := time.Unix(1000, 0)
	kv := NewKVStore("self")
	clockAt(kv, &now)
	u, _ := kv.Write("session", "token", WriteOptions{ExpiresAt: now.Add(time.Second).Unix()})

	now = now.Add(2 * time.Second)
	if got := kv.SweepExpired(); got != 1 {
		t.Fatalf("swept %d, want 1", got)
	}
	updates := kv.UpdatesFor([]string{"session"})
	if len(updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(updates))
	}
	if updates[0].Action != pb.KVDelete {
		t.Fatalf("action = %s, want delete", updates[0].Action)
	}
	if !updates[0].Version.Equal(u.Version) {
		t.Fatalf("version = %+v, want the pre-expiry %+v", updates[0].Version, u.Version)
	}

	// A later write must still win over the swept tombstone.
	kv.Set("session", "fresh")
	if v, ok := kv.Get("session"); !ok || v != "fresh" {
		t.Fatalf("post-expiry write lost: (%q, %v)", v, ok)
	}
}

func TestGCTombstones(t *testing.T) {
	now := time.Unix(1000, 0)
	kv := NewKVStore("self")
	clockAt(kv, &now)
	kv.Set("a", "1")
	kv.Delete("a")
	kv.Set("b", "2")

	if got := kv.GCTombstones(time.Hour); got != 0 {
		t.Fatalf("GC reclaimed %d fresh tombstones, want 0", got)
	}
	now = now.Add(2 * time.Hour)
	if got := kv.GCTombstones(time.Hour); got != 1 {
		t.Fatalf("GC reclaimed %d, want 1", got)
	}
	if kv.Tombstones() != 0 {
		t.Fatalf("tombstones = %d after GC, want 0", kv.Tombstones())
	}
	if _, ok := kv.Get("b"); !ok {
		t.Fatal("GC removed a live key")
	}
}

// The digest walks the keyspace in batches; the reported range is what lets the
// receiver spot keys the sender never mentioned.
func TestDigestPaginates(t *testing.T) {
	kv := NewKVStore("self")
	for _, k := range []string{"a", "b", "c", "d"} {
		kv.Set(k, k)
	}

	first := kv.Digest("", 2)
	if len(first.Entries) != 2 || first.Entries[0].Key != "a" || first.Entries[1].Key != "b" {
		t.Fatalf("first batch = %+v", first.Entries)
	}
	if first.Hi == "" {
		t.Fatal("truncated digest must report an upper bound")
	}

	second := kv.Digest("b", 2)
	if len(second.Entries) != 2 || second.Entries[0].Key != "c" {
		t.Fatalf("second batch = %+v", second.Entries)
	}
	if second.Hi != "" {
		t.Fatalf("final batch Hi = %q, want empty (end of keyspace)", second.Hi)
	}
}

// Reconcile is the heart of anti-entropy: it must ask for what it lacks and
// offer what the peer lacks, in one comparison.
func TestReconcilePushesAndPulls(t *testing.T) {
	local := NewKVStore("local")
	local.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "shared", Value: "new", Version: pb.Version{Counter: 5, Node: "x"}})
	local.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "only-local", Value: "v", Version: pb.Version{Counter: 1, Node: "x"}})
	local.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "stale-local", Value: "old", Version: pb.Version{Counter: 1, Node: "x"}})

	// The peer advertises the whole keyspace.
	d := pb.Digest{
		From: "peer",
		Entries: []pb.KeyVersion{
			{Key: "shared", Version: pb.Version{Counter: 2, Node: "x"}},
			{Key: "stale-local", Version: pb.Version{Counter: 9, Node: "x"}},
			{Key: "only-peer", Version: pb.Version{Counter: 1, Node: "x"}},
		},
	}
	push, pull := local.Reconcile(d, 100, 100)

	pushed := map[string]bool{}
	for _, u := range push {
		pushed[u.Key] = true
	}
	if !pushed["shared"] || !pushed["only-local"] {
		t.Fatalf("push = %v, want shared + only-local", keysOf(push))
	}
	if pushed["stale-local"] {
		t.Fatal("pushed an entry the peer holds a newer version of")
	}
	want := map[string]bool{"stale-local": true, "only-peer": true}
	if len(pull) != 2 || !want[pull[0]] || !want[pull[1]] {
		t.Fatalf("pull = %v, want stale-local + only-peer", pull)
	}
}

// A key outside the digest's advertised range must not be pushed: the sender
// simply had not reached that part of the keyspace yet.
func TestReconcileRespectsDigestRange(t *testing.T) {
	local := NewKVStore("local")
	local.Set("a", "1")
	local.Set("m", "2")
	local.Set("z", "3")

	d := pb.Digest{From: "peer", Lo: "", Hi: "n", Entries: []pb.KeyVersion{
		{Key: "a", Version: pb.Version{Counter: 99, Node: "x"}},
	}}
	push, _ := local.Reconcile(d, 100, 100)
	for _, u := range push {
		if u.Key == "z" {
			t.Fatalf("pushed %q from outside the digest range: %v", u.Key, keysOf(push))
		}
	}
	if len(push) != 1 || push[0].Key != "m" {
		t.Fatalf("push = %v, want just m", keysOf(push))
	}
}

func TestHistoryRecordsWriters(t *testing.T) {
	kv := NewKVStore("self")
	kv.Set("k", "v1")
	kv.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "k", Value: "v2", Version: pb.Version{Counter: 50, Node: "peer"}})

	h := kv.History("k")
	if len(h) != 2 {
		t.Fatalf("history = %d entries, want 2", len(h))
	}
	if !h[0].Local {
		t.Fatal("first entry should be a local write")
	}
	if h[1].Local {
		t.Fatal("second entry should be a remote write")
	}
	if h[1].Value != "v2" {
		t.Fatalf("history value = %q, want v2", h[1].Value)
	}
}

func TestHistoryRingIsBounded(t *testing.T) {
	kv := NewKVStore("self")
	for i := 0; i < historyPerKey*2; i++ {
		kv.Set("k", "v")
	}
	if got := len(kv.History("k")); got != historyPerKey {
		t.Fatalf("history = %d entries, want %d", got, historyPerKey)
	}
}

func keysOf(updates []pb.KVUpdate) []string {
	out := make([]string, 0, len(updates))
	for _, u := range updates {
		out = append(out, u.Key)
	}
	return out
}
