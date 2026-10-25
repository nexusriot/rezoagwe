package node

import (
	"fmt"
	"testing"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// divergeAtTheSameVersion puts two nodes in the state nothing can repair: the
// same key, the same version, different values. It is reached in the wild by
// starting two nodes from copies of one state file (§14.1).
func divergeAtTheSameVersion(t *testing.T, a, b *Node, key, aVal, bVal string) {
	t.Helper()
	version := pb.Version{Counter: 7, Node: "shared-identity"}
	for n, value := range map[*Node]string{a: aVal, b: bVal} {
		if !n.Model.Store.Apply(pb.KVUpdate{
			Action: pb.KVSet, Key: key, Value: value, Version: version,
		}) {
			t.Fatalf("%s did not take its half of the divergence", n.Addr())
		}
	}
}

// The property the whole feature exists for. Anti-entropy compares versions,
// finds them equal, and is right to do nothing — so without a repair action
// this cluster stays split for as long as both nodes run.
func TestRepairHealsADivergenceAntiEntropyCannot(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)
	divergeAtTheSameVersion(t, a, b, "colour", "blue", "green")

	// First: confirm anti-entropy really will not touch it.
	a.antiEntropyRound(b.Addr())
	b.antiEntropyRound(a.Addr())
	if av, _ := a.Get("colour"); av != "blue" {
		t.Fatalf("a holds %q after anti-entropy, expected the divergence to survive", av)
	}
	if bv, _ := b.Get("colour"); bv != "green" {
		t.Fatalf("b holds %q after anti-entropy, expected the divergence to survive", bv)
	}

	report := a.Repair([]string{"colour"})
	if report.Rewritten != 1 {
		t.Fatalf("repaired %d keys, want 1: %+v", report.Rewritten, report.Keys)
	}
	if report.From != a.Addr() {
		t.Errorf("report names %q as the source, want %q", report.From, a.Addr())
	}

	waitFor(t, "b to take a's value", func() bool {
		v, _ := b.Get("colour")
		return v == "blue"
	})
	// And the versions now differ, which is what makes it stick: b's copy is
	// genuinely older rather than merely different.
	if ae, _ := a.Entry("colour"); ae.Version.Counter <= 7 {
		t.Errorf("repaired version is %d, want one past the shared counter", ae.Version.Counter)
	}
}

func TestRepairPreservesExpiry(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")

	if _, err := a.Set("session", "token", time.Hour); err != nil {
		t.Fatalf("set: %v", err)
	}
	before, _ := a.Entry("session")

	if report := a.Repair([]string{"session"}); report.Rewritten != 1 {
		t.Fatalf("repaired %d keys, want 1", report.Rewritten)
	}
	after, _ := a.Entry("session")
	if after.ExpiresAt != before.ExpiresAt {
		t.Errorf("expiry changed from %d to %d; a repair that drops a TTL makes the key immortal",
			before.ExpiresAt, after.ExpiresAt)
	}
	if after.Value != "token" {
		t.Errorf("value changed to %q", after.Value)
	}
}

// A tombstone is a value too. Repairing one has to re-assert the delete, not
// resurrect the key.
func TestRepairRewritesATombstoneAsATombstone(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)

	if _, err := a.Set("gone", "value", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	waitFor(t, "b to see it", func() bool { _, ok := b.Get("gone"); return ok })
	if _, err := a.Delete("gone"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if report := a.Repair([]string{"gone"}); report.Rewritten != 1 {
		t.Fatalf("repaired %d keys, want 1: %+v", report.Rewritten, report.Keys)
	}
	if _, ok := a.Get("gone"); ok {
		t.Error("repairing a tombstone resurrected the key")
	}
	// Entry hides tombstones, so the check is against the replication view:
	// what this node would now send a peer for that key.
	updates := a.Model.Store.UpdatesFor([]string{"gone"})
	if len(updates) != 1 || updates[0].Action != pb.KVDelete {
		t.Fatalf("replication view = %+v, want a fresh tombstone", updates)
	}
	// And b takes the re-asserted delete rather than keeping its copy.
	waitFor(t, "b to hold the repaired tombstone", func() bool {
		_, live := b.Get("gone")
		return !live
	})
}

// A key this node does not hold cannot be made authoritative from here, and
// deleting it everywhere instead would be a destructive guess.
func TestRepairReportsAKeyItDoesNotHold(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")

	report := a.Repair([]string{"never-seen"})
	if report.Absent != 1 || report.Rewritten != 0 {
		t.Fatalf("report = %+v, want one absent and nothing rewritten", report)
	}
	if report.Keys[0].Status != RepairAbsent {
		t.Errorf("status = %q, want %q", report.Keys[0].Status, RepairAbsent)
	}
}

// Three peers naming the same key is one disagreement, not three.
func TestRepairRewritesEachKeyOnce(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")
	if _, err := a.Set("colour", "blue", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	before, _ := a.Entry("colour")

	report := a.Repair([]string{"colour", "colour", "colour"})
	if report.Rewritten != 1 {
		t.Fatalf("rewrote %d times, want 1: %+v", report.Rewritten, report.Keys)
	}
	after, _ := a.Entry("colour")
	if after.Version.Counter != before.Version.Counter+1 {
		t.Errorf("clock advanced from %d to %d, want exactly one write",
			before.Version.Counter, after.Version.Counter)
	}
}

// A repair is a write, so everything that refuses a write refuses a repair —
// and says which, rather than reporting success.
func TestRepairReportsARefusal(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")

	if _, err := a.Set("big", "0123456789", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Tightened after the write, which is exactly how a node ends up holding
	// something it can no longer accept.
	a.Model.Store.SetLimits(model.Limits{MaxValueBytes: 4})

	report := a.Repair([]string{"big"})
	if report.Refused != 1 {
		t.Fatalf("report = %+v, want one refusal", report)
	}
	if report.Keys[0].Error == "" {
		t.Error("a refusal with no reason is indistinguishable from a failure to try")
	}
}

// The ergonomic path: take the keys from this node's own consistency check.
func TestRepairDivergentUsesTheConsistencyCheck(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)
	divergeAtTheSameVersion(t, a, b, "colour", "blue", "green")

	consistency, repair := a.RepairDivergent()
	if consistency.Converged {
		t.Fatal("the check reported agreement on a divergent pair")
	}
	if repair.Rewritten != 1 {
		t.Fatalf("repaired %d keys, want the one the check named: %+v", repair.Rewritten, repair.Keys)
	}

	waitFor(t, "b to take a's value", func() bool {
		v, _ := b.Get("colour")
		return v == "blue"
	})
	// And the cluster now agrees, which is the only test that matters.
	if after := a.CheckConsistency(); !after.Converged {
		t.Errorf("replicas still disagree after a repair: %+v", after.Peers)
	}
}

// Nothing to repair must do nothing, rather than rewriting the whole store.
func TestRepairDivergentOnAHealthyClusterRewritesNothing(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)
	if _, err := a.Set("colour", "blue", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	waitFor(t, "b to see it", func() bool { v, _ := b.Get("colour"); return v == "blue" })

	consistency, repair := a.RepairDivergent()
	if !consistency.Converged {
		t.Fatalf("a healthy pair reported divergence: %+v", consistency.Peers)
	}
	if repair.Rewritten != 0 {
		t.Errorf("rewrote %d keys with nothing to repair", repair.Rewritten)
	}
}

func TestRepairPreviewRejectsConcurrentChanges(t *testing.T) {
	n := newTestNode(t, transport.NewMemNet(1), "10.0.0.1:3137")
	n.Set("k", "old", 0)
	plan := n.PreviewRepair([]string{"k"}, 0, 100)
	if plan.Items[0].Current.Value != "old" {
		t.Fatal("preview omitted value")
	}
	n.Set("k", "new", 0)
	r := n.ApplyRepairPlan(plan)
	if r.Changed != 1 || r.Rewritten != 0 || r.Complete {
		t.Fatalf("unsafe repair: %+v", r)
	}
	if v, _ := n.Get("k"); v != "new" {
		t.Fatal("concurrent value overwritten")
	}
	n.Delete("k")
	plan = n.PreviewRepair([]string{"k"}, 0, 100)
	if r := n.ApplyRepairPlan(plan); r.Rewritten != 1 {
		t.Fatalf("tombstone guard failed: %+v", r)
	}
}

func TestRepairBeyondDisplayLimitAndEmptyConflict(t *testing.T) {
	a, b := newPairedNodes(t, transport.NewMemNet(1))
	for i := 0; i < 120; i++ {
		divergeAtTheSameVersion(t, a, b, fmt.Sprintf("k%03d", i), "", "other")
	}
	before := a.CheckConsistency()
	if before.Converged || before.Peers[0].MoreDifferences != 70 {
		t.Fatalf("empty conflict not named: %+v", before)
	}
	keys := a.DivergentKeys(before)
	p := a.PreviewRepair(keys, 0, 50)
	if p.Total != 120 || p.Next != 50 || len(p.Items) != 50 {
		t.Fatalf("page: %+v", p)
	}
	p = a.PreviewRepair(keys, p.Next, 50)
	if p.Offset != 50 || p.Next != 100 {
		t.Fatal("bad next page")
	}
	_, r := a.RepairDivergent()
	if r.Rewritten != 120 {
		t.Fatalf("display cap leaked into repair: %+v", r)
	}
	waitFor(t, "all repairs", func() bool { return a.CheckConsistency().Converged })
}

func TestRepairChosenSourceOutranksNewerPeer(t *testing.T) {
	a, b := newPairedNodes(t, transport.NewMemNet(1))
	a.Model.Store.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "k", Value: "chosen", Version: pb.Version{Counter: 1, Node: "old"}})
	b.Model.Store.Apply(pb.KVUpdate{Action: pb.KVSet, Key: "k", Value: "unwanted", Version: pb.Version{Counter: 100, Node: "new"}})
	plan := a.PreviewRepair([]string{"k"}, 0, 100)
	r := a.ApplyRepairPlan(plan)
	if r.Rewritten != 1 {
		t.Fatalf("repair %+v", r)
	}
	waitFor(t, "chosen source wins", func() bool { v, _ := b.Get("k"); return v == "chosen" })
	e, _ := a.Entry("k")
	if e.Version.Counter <= 100 {
		t.Fatal("repair did not outrank observed peer")
	}
}

// MANUAL says pages are capped at 500; a limit over it fell back to the
// default of 100 instead, a smaller page than either number in play.
func TestARepairPageLimitOverTheCapIsCappedNotReset(t *testing.T) {
	n := newTestNode(t, transport.NewMemNet(1), "10.0.0.1:3137")
	keys := make([]string, 0, 600)
	for i := 0; i < 600; i++ {
		k := fmt.Sprintf("k%03d", i)
		n.Set(k, "v", 0)
		keys = append(keys, k)
	}
	if got := len(n.PreviewRepair(keys, 0, 1000).Items); got != 500 {
		t.Fatalf("limit 1000 gave a page of %d, want the 500 cap", got)
	}
	if got := len(n.PreviewRepair(keys, 0, 0).Items); got != 100 {
		t.Fatalf("no limit gave a page of %d, want the default 100", got)
	}
}
