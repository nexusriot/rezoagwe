package node

import (
	"fmt"
	"strings"
	"testing"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

func TestConsistencyReportsTwoAgreeingReplicas(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)

	for i := 0; i < 20; i++ {
		u := a.Set(fmt.Sprintf("k%02d", i), "v", 0)
		b.Model.Store.Apply(u)
	}

	report := a.CheckConsistency()
	if !report.Converged {
		t.Fatalf("two identical replicas reported divergent: %+v", report)
	}
	if len(report.Peers) != 1 || !report.Peers[0].Reachable || !report.Peers[0].Agrees {
		t.Fatalf("peer entry = %+v", report.Peers)
	}
	if report.Peers[0].Keys != 20 || report.Keys != 20 {
		t.Fatalf("key counts = local %d / peer %d, want 20 each", report.Keys, report.Peers[0].Keys)
	}
}

// The failure the check exists to name: a write that never reached a replica.
// Anti-entropy would repair it eventually and report nothing either way.
func TestConsistencyFindsADivergentReplica(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)

	for i := 0; i < 20; i++ {
		u := a.Set(fmt.Sprintf("k%02d", i), "v", 0)
		b.Model.Store.Apply(u)
	}
	// One write that b never sees.
	a.Model.Store.Set("k99", "lost")

	report := a.CheckConsistency()
	if report.Converged {
		t.Fatal("a missing key was reported as converged")
	}
	peer := report.Peers[0]
	if peer.Agrees || len(peer.DifferingBuckets) == 0 {
		t.Fatalf("peer entry = %+v", peer)
	}
	if len(peer.DifferingBuckets) != 1 {
		t.Fatalf("one differing key landed in %d buckets, want 1", len(peer.DifferingBuckets))
	}
	if peer.Keys != 20 || report.Keys != 21 {
		t.Fatalf("key counts = local %d / peer %d", report.Keys, peer.Keys)
	}
}

// Two replicas that hold the same key at different versions have diverged just
// as much as one that is missing it.
func TestConsistencyFindsAStaleValue(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)

	u := a.Set("k", "first", 0)
	b.Model.Store.Apply(u)
	a.Model.Store.Set("k", "second") // b keeps the old version

	report := a.CheckConsistency()
	if report.Converged {
		t.Fatal("a stale value was reported as converged")
	}
	if report.Peers[0].Keys != 1 {
		t.Fatalf("peer holds %d keys, want 1 — it is stale, not missing", report.Peers[0].Keys)
	}
}

// A tombstone one side never saw is divergence, not agreement: the two answer
// a read differently.
func TestConsistencyCountsATombstoneAsDivergence(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)

	u := a.Set("k", "v", 0)
	b.Model.Store.Apply(u)
	a.Model.Store.Delete("k")

	report := a.CheckConsistency()
	if report.Converged {
		t.Fatal("an unreplicated delete was reported as converged")
	}
}

// A peer that does not answer says nothing about whether it agrees, and must
// not be counted as if it did.
func TestConsistencyReportsAnUnreachablePeerSeparately(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")
	a.Model.AddPeer("10.0.0.9:3137") // never started

	report := a.CheckConsistency()
	if report.Unreachable != 1 {
		t.Fatalf("unreachable = %d, want 1", report.Unreachable)
	}
	if !report.Converged {
		t.Fatal("an unreachable peer was counted as a disagreement")
	}
	if report.Peers[0].Reachable || report.Peers[0].Error == "" {
		t.Fatalf("peer entry = %+v", report.Peers[0])
	}
}

// The fingerprint must not depend on the order entries were learned in, or two
// converged replicas would report divergence at random.
func TestFingerprintIsOrderIndependent(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")
	b := newTestNode(t, net, "10.0.0.2:3137")

	keys := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	updates := make([]pb.KVUpdate, 0, len(keys))
	for _, k := range keys {
		updates = append(updates, a.Set(k, "v", 0))
	}
	for i := len(updates) - 1; i >= 0; i-- { // b learns them backwards
		b.Model.Store.Apply(updates[i])
	}

	fa := a.Model.Store.Fingerprint(pb.FingerprintBuckets)
	fb := b.Model.Store.Fingerprint(pb.FingerprintBuckets)
	if strings.Join(fa.Buckets, "") != strings.Join(fb.Buckets, "") {
		t.Fatal("the same entries learned in a different order produced different buckets")
	}
}

func TestEmptyStoresFingerprintIdentically(t *testing.T) {
	a := newTestNode(t, transport.NewMemNet(1), "10.0.0.1:3137")
	b := newTestNode(t, transport.NewMemNet(1), "10.0.0.2:3137")
	fa := a.Model.Store.Fingerprint(pb.FingerprintBuckets)
	fb := b.Model.Store.Fingerprint(pb.FingerprintBuckets)
	if strings.Join(fa.Buckets, "") != strings.Join(fb.Buckets, "") {
		t.Fatal("two empty stores disagree")
	}
	if len(fa.Buckets) != pb.FingerprintBuckets {
		t.Fatalf("bucket count = %d", len(fa.Buckets))
	}
}
