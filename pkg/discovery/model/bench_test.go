package model

import (
	"fmt"
	"testing"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// storeSizes are the scales worth asking about: a cluster nobody worries
// about, one where reconciliation starts to be a cost, and the size at which
// the roadmap's Merkle-tree item was proposed.
var storeSizes = []int{100, 10_000, 100_000}

// filled builds a store of n keys with values of a realistic size.
func filled(n int) *KVStore {
	kv := NewKVStore("bench")
	value := make([]byte, 64)
	for i := range value {
		value[i] = byte('a' + i%26)
	}
	for i := 0; i < n; i++ {
		kv.Set(fmt.Sprintf("key/%08d", i), string(value))
	}
	return kv
}

// BenchmarkDigest measures what one anti-entropy round costs the sender.
//
// This runs on the gossip loop, which is the loop whose lateness gets a node
// evicted — so the question is not whether it is fast but whether it stays
// well inside a tick as the store grows.
func BenchmarkDigest(b *testing.B) {
	for _, n := range storeSizes {
		kv := filled(n)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				kv.Digest("", 128)
			}
		})
	}
}

// BenchmarkReconcile measures the receiving side: comparing a peer's digest
// against the local store and deciding what to push and pull.
func BenchmarkReconcile(b *testing.B) {
	for _, n := range storeSizes {
		kv := filled(n)
		peer := filled(n)
		d := peer.Digest("", 128)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				kv.Reconcile(d, 128, 128)
			}
		})
	}
}

// BenchmarkFingerprint is the consistency check's cost. Unlike the other two
// it walks the whole store rather than a slice of it, and it hashes every
// value — which is what makes it the expensive one, and the reason it is not
// on the gossip path.
func BenchmarkFingerprint(b *testing.B) {
	for _, n := range storeSizes {
		kv := filled(n)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				kv.Fingerprint(64)
			}
		})
	}
}

// BenchmarkApply is the replication hot path: one update arriving from a peer.
func BenchmarkApply(b *testing.B) {
	kv := filled(10_000)
	u := pb.KVUpdate{Action: pb.KVSet, Key: "key/00000001", Value: "v"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		u.Version = pb.Version{Counter: uint64(i + 1_000_000), Node: "peer"}
		kv.Apply(u)
	}
}

// BenchmarkState is what every flush pays: the whole store is still copied and
// re-encoded per write burst, which is what the roadmap's append-only log item
// is about.
func BenchmarkState(b *testing.B) {
	for _, n := range storeSizes {
		kv := filled(n)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				kv.State()
			}
		})
	}
}

// BenchmarkSweepExpired runs on the sweep loop over a store with nothing to
// expire, which is the normal case and therefore the one whose cost is paid
// every tick for nothing.
func BenchmarkSweepExpired(b *testing.B) {
	for _, n := range storeSizes {
		kv := filled(n)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				kv.SweepExpired()
			}
		})
	}
}
