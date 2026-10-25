package node

import (
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"sort"
	"time"
	"unicode/utf8"
)

// PreviewRestore builds a stable diff without changing data. Missing local keys
// are additions; tombstones and optionally missing backup keys are deletions.
func (n *Node) PreviewRestore(data []byte, keys []string, deleteMissing, allowCrossCluster bool) (RepairPlan, error) {
	var backup ExportFile
	if err := json.Unmarshal(data, &backup); err != nil {
		return RepairPlan{}, err
	}
	if backup.Entries == nil {
		return RepairPlan{}, fmt.Errorf("backup must contain an entries array")
	}
	if backup.Cluster != n.cfg.Cluster && !allowCrossCluster {
		return RepairPlan{}, fmt.Errorf("backup cluster %q differs from %q; explicitly allow cross-cluster restore", backup.Cluster, n.cfg.Cluster)
	}
	desired := map[string]pb.KVUpdate{}
	for _, u := range backup.Entries {
		if !utf8.ValidString(u.Key) || !utf8.ValidString(u.Value) || (u.Action != pb.KVSet && u.Action != pb.KVDelete) {
			return RepairPlan{}, fmt.Errorf("invalid backup entry %q", u.Key)
		}
		if _, ok := desired[u.Key]; ok {
			return RepairPlan{}, fmt.Errorf("duplicate backup key %q", u.Key)
		}
		if u.Action == pb.KVSet && u.ExpiresAt != 0 && u.ExpiresAt <= time.Now().Unix() {
			u.Action = pb.KVDelete
			u.Value = ""
			u.ExpiresAt = 0
		}
		desired[u.Key] = u
	}
	current := map[string]pb.KVUpdate{}
	for _, u := range n.Model.Store.Updates() {
		current[u.Key] = u
	}
	selected := map[string]bool{}
	for _, k := range keys {
		selected[k] = true
	}
	if deleteMissing {
		for k := range current {
			if _, ok := desired[k]; !ok {
				desired[k] = pb.KVUpdate{Key: k, Action: pb.KVDelete}
			}
		}
	}
	names := []string{}
	for k := range desired {
		if len(selected) == 0 || selected[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	p := RepairPlan{ID: uuid.NewString(), Kind: "restore", From: n.Addr(), NodeID: n.Model.NodeID, CreatedAt: time.Now().Unix(), SourceCluster: backup.Cluster, SourceNode: backup.Node, Items: []RepairCandidate{}}
	for _, k := range names {
		d := desired[k]
		c, exists := current[k]
		if exists && c.Action == d.Action && (d.Action == pb.KVDelete || (c.Value == d.Value && c.ExpiresAt == d.ExpiresAt)) {
			continue
		}
		item := RepairCandidate{Key: k, Desired: &d, Change: "add"}
		if exists {
			copy := c
			item.Current = &copy
			item.Change = "overwrite"
		}
		if d.Action == pb.KVDelete {
			item.Change = "delete"
		}
		p.Items = append(p.Items, item)
	}
	p.Total = len(p.Items)
	return n.cachePlan(p, 0, 100), nil
}
func (n *Node) ApplyRestore(id string, keys []string) (RepairReport, error) {
	if len(keys) == 0 || len(keys) > 500 {
		return RepairReport{}, fmt.Errorf("select 1–500 keys")
	}
	n.repairMu.Lock()
	p, ok := n.repairPlans[id]
	n.repairMu.Unlock()
	if !ok || p.Kind != "restore" || time.Now().Unix()-p.CreatedAt > 600 {
		return RepairReport{}, fmt.Errorf("restore preview expired; preview again")
	}
	wanted := map[string]bool{}
	for _, k := range keys {
		wanted[k] = true
	}
	chosen := []RepairCandidate{}
	for _, item := range p.Items {
		if wanted[item.Key] {
			chosen = append(chosen, item)
			delete(wanted, item.Key)
		}
	}
	if len(wanted) > 0 {
		return RepairReport{}, fmt.Errorf("selected key is not in this preview")
	}
	p.Items = chosen
	p.Total = len(chosen)
	p.Next = -1
	return n.ApplyRepairPlan(p), nil
}
