package node

import (
	"sort"

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
func (n *Node) Repair(keys []string) RepairReport {
	report := RepairReport{From: n.cfg.AdvertiseAddr, Keys: make([]RepairOutcome, 0, len(keys))}

	// UpdatesFor is the replication view — exactly what this node would send a
	// peer for these keys — which is the right thing to re-assert and the only
	// view that includes tombstones. GetEntry hides them, and a divergence
	// over whether a key was deleted is one of the things worth repairing.
	held := make(map[string]pb.KVUpdate)
	for _, u := range n.Model.Store.UpdatesFor(keys) {
		held[u.Key] = u
	}

	for _, key := range dedupe(keys) {
		current, ok := held[key]
		if !ok {
			report.Absent++
			report.Keys = append(report.Keys, RepairOutcome{Key: key, Status: RepairAbsent})
			continue
		}

		var u pb.KVUpdate
		var err error
		if current.Action == pb.KVDelete {
			u, err = n.Delete(key)
		} else {
			// Rewritten as itself, expiry included: a repair that silently
			// dropped a TTL would make every repaired key immortal.
			u, err = n.Model.Store.Write(key, current.Value,
				model.WriteOptions{ExpiresAt: current.ExpiresAt})
			if err == nil {
				n.Metrics.KVLocalWrites.Add(1)
				n.broadcast(pb.KindKV, u)
				n.ev.KVChanged()
			}
		}
		if err != nil {
			report.Refused++
			report.Keys = append(report.Keys, RepairOutcome{
				Key: key, Status: RepairRefused, Error: err.Error(),
			})
			continue
		}
		report.Rewritten++
		report.Keys = append(report.Keys, RepairOutcome{
			Key: key, Status: RepairRewritten, Version: formatVersion(u.Version),
		})
	}

	if report.Rewritten > 0 {
		n.logActivity("repaired %d key(s) from this node's copy", report.Rewritten)
	}
	return report
}

// RepairDivergent runs the consistency check and repairs every key it names.
//
// The keys come from this node's own comparison, so this means "make my copy
// win wherever we disagree". That is the right move on the replica an operator
// trusts and a way to overwrite a whole cluster with stale data on any other,
// which is why it is a separate call and not the default.
func (n *Node) RepairDivergent() (ConsistencyReport, RepairReport) {
	report := n.CheckConsistency()
	keys := make([]string, 0)
	for _, peer := range report.Peers {
		for _, d := range peer.Differences {
			keys = append(keys, d.Key)
		}
	}
	return report, n.Repair(keys)
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
