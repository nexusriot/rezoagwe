package controller

import (
	"strconv"
	"strings"
	"testing"

	"github.com/nexusriot/rezoagwe/pkg/discovery/node"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

func TestTopologyModalOpens(t *testing.T) {
	c := newRunningController(t)
	c.node.Model.AddPeer("10.0.0.2:3137")
	c.node.Model.SetNick("10.0.0.2:3137", "bob")

	var open bool
	onUI(t, c, func() {
		c.topology()
		open = c.view.Pages.HasPage("modal")
		c.closeModal()
	})
	if !open {
		t.Fatal("topology modal did not open")
	}
}

func TestDiagnosticsModalOpens(t *testing.T) {
	c := newRunningController(t)
	var open bool
	onUI(t, c, func() {
		c.diagnostics()
		open = c.view.Pages.HasPage("modal")
		c.closeModal()
	})
	if !open {
		t.Fatal("diagnostics modal did not open")
	}
}

// The check dials every peer, so opening it must not block the event loop: the
// modal appears first and fills in when the answers arrive.
func TestConsistencyModalOpensBeforeTheCheckFinishes(t *testing.T) {
	c := newRunningController(t)
	c.node.Model.AddPeer("10.0.0.9:3137") // never answers

	var open bool
	onUI(t, c, func() {
		c.consistency()
		open = c.view.Pages.HasPage("modal")
	})
	if !open {
		t.Fatal("consistency modal did not open immediately")
	}
	onUI(t, c, c.closeModal)
}

func TestConsistencyRenderingNamesWhoDisagrees(t *testing.T) {
	converged := renderConsistency(node.ConsistencyReport{
		Keys: 3, Converged: true,
		Peers: []node.PeerConsistency{{Addr: "10.0.0.2:3137", Nick: "bob", Reachable: true, Agrees: true, Keys: 3}},
	})
	if !strings.Contains(converged, "every peer agrees") || !strings.Contains(converged, "bob") {
		t.Fatalf("converged report = %s", converged)
	}

	split := renderConsistency(node.ConsistencyReport{
		Keys: 4,
		Peers: []node.PeerConsistency{
			{Addr: "10.0.0.2:3137", Reachable: true, Keys: 3, DifferingBuckets: []int{1, 5}},
			{Addr: "10.0.0.3:3137", Error: "dial refused"},
		},
		Unreachable: 1,
	})
	if !strings.Contains(split, "replicas disagree") {
		t.Fatalf("divergent report does not say so: %s", split)
	}
	if !strings.Contains(split, "differs in 2 of") {
		t.Fatalf("report does not localise the divergence: %s", split)
	}
	if !strings.Contains(split, "dial refused") {
		t.Fatalf("report hides why a peer was unreachable: %s", split)
	}
	// An unreachable peer must not read as agreement.
	if !strings.Contains(split, "says nothing about whether it agrees") {
		t.Fatalf("report does not qualify the silent peer: %s", split)
	}
}

func TestConsistencyRenderingOnALoneNode(t *testing.T) {
	out := renderConsistency(node.ConsistencyReport{Converged: true})
	if !strings.Contains(out, "no peers to compare against") {
		t.Fatalf("lone-node report = %s", out)
	}
}

// The bucket count in the report has to be the one the protocol actually uses,
// or "differs in 2 of 16" is a sentence about nothing.
func TestConsistencyRenderingUsesTheProtocolBucketCount(t *testing.T) {
	out := renderConsistency(node.ConsistencyReport{
		Peers: []node.PeerConsistency{{Addr: "a", Reachable: true, DifferingBuckets: []int{0}}},
	})
	if !strings.Contains(out, "of "+strconv.Itoa(pb.FingerprintBuckets)) {
		t.Fatalf("report = %s", out)
	}
}
