package node

import (
	"encoding/json"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
	"strings"
	"testing"
)

func TestRestoreSelectionAndGuards(t *testing.T) {
	n := newTestNode(t, transport.NewMemNet(1), "10.0.0.1:3137")
	n.Set("old", "current", 0)
	n.Set("extra", "delete explicitly", 0)
	data, _ := json.Marshal(ExportFile{Cluster: n.Cluster(), Entries: []pb.KVUpdate{{Key: "old", Value: "backup", Action: pb.KVSet}, {Key: "new", Value: "addition", Action: pb.KVSet}}})
	p, err := n.PreviewRestore(data, nil, true, false)
	if err != nil || p.Total != 3 {
		t.Fatal(p, err)
	}
	if v, _ := n.Get("old"); v != "current" {
		t.Fatal("preview wrote data")
	}
	n.Set("new", "concurrent", 0)
	r, err := n.ApplyRestore(p.ID, []string{"old", "new"})
	if err != nil || r.Rewritten != 1 || r.Changed != 1 {
		t.Fatal(r, err)
	}
	if v, _ := n.Get("extra"); v == "" {
		t.Fatal("unselected key deleted")
	}
	if v, _ := n.Get("new"); v != "concurrent" {
		t.Fatal("concurrent addition lost")
	}
	if _, err = n.PreviewRestore([]byte(strings.Replace(string(data), n.Cluster(), "different", 1)), nil, false, false); err == nil {
		t.Fatal("cross-cluster restore accepted")
	}
}
