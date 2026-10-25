package node

import (
	"errors"
	"github.com/google/uuid"
	"sort"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// RepairStatus is what happened to one key.
type RepairStatus string

const (
	// RepairRewritten means this node's copy was re-stamped and replicated.
	RepairRewritten RepairStatus = "rewritten"
	// RepairAbsent means this node does not hold the key at all, so there is
	// nothing here to make authoritative.
	RepairAbsent RepairStatus = "absent"
	// RepairRefused means the rewrite was refused — a limit, or a value this
	// node holds that it could not send even once.
	RepairRefused RepairStatus = "refused"
	RepairChanged RepairStatus = "changed"
)

// RepairOutcome is one key's result.
type RepairOutcome struct {
	Key     string       `json:"key"`
	Status  RepairStatus `json:"status"`
	Version string       `json:"version,omitempty"`
	Error   string       `json:"error,omitempty"`
}

// RepairReport is the result of a repair.
type RepairReport struct {
	Changed   int                `json:"changed"`
	Remaining int                `json:"remaining"`
	Complete  bool               `json:"complete"`
	After     *ConsistencyReport `json:"after,omitempty"`
	// From names the node whose copy was made authoritative, because that is
	// the only thing anybody reading this afterwards needs to know.
	From      string          `json:"from"`
	Rewritten int             `json:"rewritten"`
	Absent    int             `json:"absent"`
	Refused   int             `json:"refused"`
	Keys      []RepairOutcome `json:"keys"`
}

// Repair makes this node's copy of the named keys authoritative.
//
// The consistency check (§14) names the keys on which replicas disagree and
// stops there, because anti-entropy cannot resolve a disagreement in which
// both sides hold the same version: it compares versions, finds them equal and
// is right to do nothing. Two replicas reach that state from a copied state
// file (§14.1), and used to reach it from a value that could not survive the
// wire (§5.9). The only cure was a manual delete and rewrite.
//
// This is that cure, as an operation: each key this node holds is re-stamped
// with a fresh version from this node's Lamport clock and replicated, so
// last-write-wins carries it everywhere. A live value is rewritten as itself,
// expiry included; a tombstone is rewritten as a fresh tombstone.
//
// It is deliberately an explicit operator action and not something
// anti-entropy does on its own. Run on the wrong replica it spreads the wrong
// value to the whole cluster, and nothing in the protocol can tell which
// replica was right — that is a judgement about the data, which is why it
// needs a person. A key this node does not hold is reported rather than
// deleted everywhere, for the same reason.
// RepairPlan is a reviewable page. Applying it compares the exact local snapshot
// under the store lock; a newer write is reported, never overwritten by a preview.
type RepairPlan struct {
	Kind          string            `json:"kind,omitempty"`
	SourceCluster string            `json:"source_cluster,omitempty"`
	SourceNode    string            `json:"source_node,omitempty"`
	ID            string            `json:"id"`
	Error         string            `json:"error,omitempty"`
	From          string            `json:"from"`
	NodeID        string            `json:"node_id"`
	CreatedAt     int64             `json:"created_at"`
	Total         int               `json:"total"`
	Offset        int               `json:"offset"`
	Next          int               `json:"next"`
	Items         []RepairCandidate `json:"items"`
}
type RepairCandidate struct {
	Desired *pb.KVUpdate `json:"desired,omitempty"`
	Change  string       `json:"change,omitempty"`
	Key     string       `json:"key"`
	Current *pb.KVUpdate `json:"current"`
}

// PreviewRepair captures stable candidates. Page by ID so applying an earlier
// page cannot shift the offsets of the later pages. Plans expire after 10 minutes.
func (n *Node) PreviewRepair(keys []string, offset, limit int) RepairPlan {
	keys = dedupe(keys)
	p := RepairPlan{ID: uuid.NewString(), From: n.Addr(), NodeID: n.Model.NodeID, CreatedAt: time.Now().Unix(), Total: len(keys), Next: -1, Items: make([]RepairCandidate, 0, len(keys))}
	held := make(map[string]pb.KVUpdate)
	for _, u := range n.Model.Store.UpdatesFor(keys) {
		held[u.Key] = u
	}
	for _, key := range keys {
		c := RepairCandidate{Key: key}
		if u, ok := held[key]; ok {
			copy := u
			c.Current = &copy
		}
		p.Items = append(p.Items, c)
	}
	return n.cachePlan(p, offset, limit)
}

func (n *Node) cachePlan(p RepairPlan, offset, limit int) RepairPlan {
	n.repairMu.Lock()
	if n.repairPlans == nil {
		n.repairPlans = make(map[string]RepairPlan)
	}
	for id, old := range n.repairPlans {
		if time.Now().Unix()-old.CreatedAt > 600 {
			delete(n.repairPlans, id)
		}
	}
	if len(n.repairPlans) >= 4 {
		oldest := ""
		var at int64
		for id, old := range n.repairPlans {
			if oldest == "" || old.CreatedAt < at {
				oldest = id
				at = old.CreatedAt
			}
		}
		delete(n.repairPlans, oldest)
	}
	n.repairPlans[p.ID] = p
	n.repairMu.Unlock()
	return n.RepairPlanPage(p.ID, offset, limit)
}
func (n *Node) RepairPlanPage(id string, offset, limit int) RepairPlan {
	n.repairMu.Lock()
	defer n.repairMu.Unlock()
	p, ok := n.repairPlans[id]
	if !ok || time.Now().Unix()-p.CreatedAt > 600 {
		return RepairPlan{Error: "repair preview expired or unknown; preview again", Next: -1}
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(p.Items) {
		offset = len(p.Items)
	}
	// Capped, as documented: asking for 1000 used to fall back to the default
	// of 100, a smaller page than either number the caller had in mind.
	if limit <= 0 {
		limit = 100
	} else if limit > 500 {
		limit = 500
	}
	end := offset + limit
	if end > len(p.Items) {
		end = len(p.Items)
	}
	p.Offset = offset
	p.Next = -1
	if end < len(p.Items) {
		p.Next = end
	}
	p.Items = append([]RepairCandidate{}, p.Items[offset:end]...)
	for i := range p.Items {
		if p.Items[i].Desired != nil {
			u := *p.Items[i].Desired
			p.Items[i].Desired = &u
		}
		if p.Items[i].Current != nil {
			u := *p.Items[i].Current
			p.Items[i].Current = &u
		}
	}
	return p
}

func (n *Node) ApplyRepairPlan(plan RepairPlan) RepairReport {
	report := RepairReport{From: n.Addr(), Keys: make([]RepairOutcome, 0, len(plan.Items))}
	// A deliberately chosen older source must still win against versions we
	// have observed on peers. Otherwise repair reports a rewrite peers ignore.
	before := n.CheckConsistency()
	floor := before.Clock
	for _, peer := range before.Peers {
		if peer.Reachable && peer.Clock > floor {
			floor = peer.Clock
		}
	}
	for _, item := range plan.Items {
		if plan.NodeID != n.Model.NodeID || plan.From != n.Addr() {
			report.Refused++
			report.Keys = append(report.Keys, RepairOutcome{Key: item.Key, Status: RepairRefused, Error: "preview belongs to another node"})
			continue
		}
		if item.Current == nil && item.Desired == nil {
			report.Absent++
			report.Keys = append(report.Keys, RepairOutcome{Key: item.Key, Status: RepairAbsent})
			continue
		}
		current := pb.KVUpdate{Key: item.Key}
		if item.Current != nil {
			current = *item.Current
		}
		desired := current
		if item.Desired != nil {
			desired = *item.Desired
		}
		if current.Key != item.Key || desired.Key != item.Key {
			report.Refused++
			report.Keys = append(report.Keys, RepairOutcome{Key: item.Key, Status: RepairRefused, Error: "preview key mismatch"})
			continue
		}
		opt := model.WriteOptions{ExpiresAt: desired.ExpiresAt, ExpectUpdate: item.Current, ExpectMissing: item.Current == nil, AfterCounter: floor}
		var u pb.KVUpdate
		var err error
		if desired.Action == pb.KVDelete {
			u, err = n.Model.Store.Remove(item.Key, opt)
		} else {
			u, err = n.Model.Store.Write(item.Key, desired.Value, opt)
		}
		if err != nil {
			status := RepairRefused
			if errors.Is(err, model.ErrVersionMismatch) {
				status = RepairChanged
				report.Changed++
			} else {
				report.Refused++
			}
			report.Keys = append(report.Keys, RepairOutcome{Key: item.Key, Status: status, Error: err.Error()})
			continue
		}
		n.Metrics.KVLocalWrites.Add(1)
		n.broadcast(pb.KindKV, u)
		n.ev.KVChanged()
		report.Rewritten++
		report.Keys = append(report.Keys, RepairOutcome{Key: item.Key, Status: RepairRewritten, Version: formatVersion(u.Version)})
	}
	report.Remaining = report.Absent + report.Refused + report.Changed
	if plan.Next >= 0 {
		report.Remaining += plan.Total - plan.Next
	}
	after := n.CheckConsistency()
	report.After = &after
	report.Complete = report.Remaining == 0 && after.Converged && after.Unreachable == 0
	if report.Rewritten > 0 {
		n.logActivity("repaired %d key(s) from this node's copy", report.Rewritten)
	}
	return report
}

func (n *Node) Repair(keys []string) RepairReport {
	keys = dedupe(keys)
	total := RepairReport{From: n.Addr(), Keys: []RepairOutcome{}}
	first := n.PreviewRepair(keys, 0, 500)
	for offset := 0; offset < len(keys); {
		plan := n.RepairPlanPage(first.ID, offset, 500)
		if plan.Error != "" {
			total.Refused += len(keys) - offset
			break
		}
		r := n.ApplyRepairPlan(plan)
		total.Rewritten += r.Rewritten
		total.Absent += r.Absent
		total.Refused += r.Refused
		total.Changed += r.Changed
		total.Keys = append(total.Keys, r.Keys...)
		total.After = r.After
		if plan.Next < 0 {
			break
		}
		offset = plan.Next
	}
	total.Remaining = total.Absent + total.Refused + total.Changed
	if total.After == nil {
		a := n.CheckConsistency()
		total.After = &a
	}
	total.Complete = total.Remaining == 0 && total.After.Converged && total.After.Unreachable == 0
	return total
}

// DivergentKeys obtains full candidates independently of the display limit.
func (n *Node) DivergentKeys(report ConsistencyReport) []string {
	keys := []string{}
	for _, peer := range report.Peers {
		diffs := peer.Differences
		if peer.MoreDifferences > 0 {
			diffs, _ = n.namedDifferencesLimit(peer.Addr, peer.DifferingBuckets, 0)
		}
		for _, d := range diffs {
			keys = append(keys, d.Key)
		}
	}
	return dedupe(keys)
}

// RepairDivergent runs the consistency check and repairs every key it names.
//
// The keys come from this node's own comparison, so this means "make my copy
// win wherever we disagree". That is the right move on the replica an operator
// trusts and a way to overwrite a whole cluster with stale data on any other,
// which is why it is a separate call and not the default.
func (n *Node) RepairDivergent() (ConsistencyReport, RepairReport) {
	report := n.CheckConsistency()
	return report, n.Repair(n.DivergentKeys(report))
}

// dedupe returns the keys sorted and without repeats, so a key named by three
// peers is rewritten once rather than three times — three versions of the same
// value is noise in the history and three broadcasts is noise on the wire.
func dedupe(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
