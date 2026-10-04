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
		u, _ := a.Set(fmt.Sprintf("k%02d", i), "v", 0)
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
		u, _ := a.Set(fmt.Sprintf("k%02d", i), "v", 0)
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

	u, _ := a.Set("k", "first", 0)
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

	u, _ := a.Set("k", "v", 0)
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
		u, err := a.Set(k, "v", 0)
		if err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
		updates = append(updates, u)
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

// "They differ somewhere in bucket 7" is where a report used to stop, which on
// a store of a few hundred keys is a sixteenth of the keyspace to go and read
// by hand. The check knows which key it is; it just never said.
func TestAConsistencyReportNamesTheDivergingKey(t *testing.T) {
	net := transport.NewMemNet(40)
	a, b := newPairedNodes(t, net)

	for _, k := range []string{"alpha", "beta", "gamma", "delta"} {
		if _, err := a.Set(k, "shared", 0); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	waitFor(t, "the peer to take the writes", func() bool { return b.Model.Store.Len() == 4 })

	// One key written only on b, behind a partition, so the stores diverge on
	// exactly one name.
	net.Partition(a.Addr(), b.Addr(), true)
	if _, err := b.Set("beta", "only-on-b", 0); err != nil {
		t.Fatalf("set on b: %v", err)
	}
	net.Partition(a.Addr(), b.Addr(), false)

	report := a.CheckConsistency()
	if report.Converged {
		t.Fatal("the report claims convergence over a key the two hold differently")
	}
	peer := report.Peers[0]
	if len(peer.Differences) != 1 {
		t.Fatalf("named %d differences, want exactly 1: %+v", len(peer.Differences), peer.Differences)
	}
	d := peer.Differences[0]
	if d.Key != "beta" {
		t.Errorf("named %q, want beta", d.Key)
	}
	if d.Local == "" || d.Remote == "" || d.Local == d.Remote {
		t.Errorf("both versions should be present and different: local=%q remote=%q", d.Local, d.Remote)
	}
}

// A key one replica has never seen is the divergence that matters most — a
// write that never arrived — and it cannot be found by walking the local
// store, which is the direction a naive diff takes.
func TestAReportNamesAKeyThisNodeHasNeverSeen(t *testing.T) {
	net := transport.NewMemNet(41)
	a, b := newPairedNodes(t, net)

	net.Partition(a.Addr(), b.Addr(), true)
	if _, err := b.Set("orphan", "never-arrived", 0); err != nil {
		t.Fatalf("set on b: %v", err)
	}
	net.Partition(a.Addr(), b.Addr(), false)

	peer := a.CheckConsistency().Peers[0]
	var found *KeyDifference
	for i := range peer.Differences {
		if peer.Differences[i].Key == "orphan" {
			found = &peer.Differences[i]
		}
	}
	if found == nil {
		t.Fatalf("a key only the peer holds was not named: %+v", peer.Differences)
	}
	if found.Local != "" {
		t.Errorf("local version = %q, want empty for a key this node never saw", found.Local)
	}
	if found.Remote == "" {
		t.Error("the peer's version should be named")
	}
}

// A key deleted on one side and live on the other is a divergence the version
// alone does not explain, so the report has to say which side holds a
// tombstone.
func TestAReportDistinguishesATombstoneFromAValue(t *testing.T) {
	net := transport.NewMemNet(42)
	a, b := newPairedNodes(t, net)

	if _, err := a.Set("doomed", "v", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	waitFor(t, "the peer to take the write", func() bool { _, ok := b.Get("doomed"); return ok })

	net.Partition(a.Addr(), b.Addr(), true)
	if _, err := b.Delete("doomed"); err != nil {
		t.Fatalf("delete on b: %v", err)
	}
	net.Partition(a.Addr(), b.Addr(), false)

	peer := a.CheckConsistency().Peers[0]
	if len(peer.Differences) != 1 || peer.Differences[0].Key != "doomed" {
		t.Fatalf("differences = %+v, want just doomed", peer.Differences)
	}
	d := peer.Differences[0]
	if d.LocalDeleted {
		t.Error("this node holds the value, not a tombstone")
	}
	if !d.RemoteDeleted {
		t.Error("the peer holds a tombstone and the report does not say so")
	}
}

// A report is something a person reads. Two replicas sharing nothing must not
// print the whole keyspace.
func TestNamedDifferencesAreCapped(t *testing.T) {
	net := transport.NewMemNet(43)
	a, b := newPairedNodes(t, net)

	net.Partition(a.Addr(), b.Addr(), true)
	const keys = MaxReportedDifferences + 25
	for i := 0; i < keys; i++ {
		if _, err := b.Set(fmt.Sprintf("k%03d", i), "v", 0); err != nil {
			t.Fatalf("set: %v", err)
		}
	}
	net.Partition(a.Addr(), b.Addr(), false)

	peer := a.CheckConsistency().Peers[0]
	if len(peer.Differences) != MaxReportedDifferences {
		t.Fatalf("named %d keys, want the cap of %d", len(peer.Differences), MaxReportedDifferences)
	}
	if peer.MoreDifferences != keys-MaxReportedDifferences {
		t.Errorf("MoreDifferences = %d, want %d", peer.MoreDifferences, keys-MaxReportedDifferences)
	}
	// Capped, but still sorted, so the same run twice names the same keys.
	for i := 1; i < len(peer.Differences); i++ {
		if peer.Differences[i-1].Key >= peer.Differences[i].Key {
			t.Fatalf("differences are not sorted: %q then %q",
				peer.Differences[i-1].Key, peer.Differences[i].Key)
		}
	}
}

// Agreement still reports nothing to look at: a check that names keys on a
// healthy cluster is a check nobody trusts.
func TestAConvergedReportNamesNothing(t *testing.T) {
	net := transport.NewMemNet(44)
	a, b := newPairedNodes(t, net)
	if _, err := a.Set("k", "v", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	waitFor(t, "the peer to take the write", func() bool { _, ok := b.Get("k"); return ok })

	report := a.CheckConsistency()
	if !report.Converged {
		t.Fatal("a converged cluster reported as divergent")
	}
	if len(report.Peers[0].Differences) != 0 {
		t.Fatalf("an agreeing peer was given differences: %+v", report.Peers[0].Differences)
	}
}
