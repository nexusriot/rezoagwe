package topology

import (
	"testing"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
)

func at(sec int) time.Time { return time.Unix(int64(sec), 0) }

func find(t *testing.T, g Graph, addr string) Node {
	t.Helper()
	for _, n := range g.Nodes {
		if n.Addr == addr {
			return n
		}
	}
	t.Fatalf("node %q not in graph %+v", addr, g.Nodes)
	return Node{}
}

func linkKind(g Graph, a, b string) (LinkKind, bool) {
	for _, l := range g.Links {
		if (l.A == a && l.B == b) || (l.A == b && l.B == a) {
			return l.Kind, true
		}
	}
	return "", false
}

func TestSelfIsAtTheCentreAndPeersAreDirect(t *testing.T) {
	g := Build(Input{
		SelfAddr: "10.0.0.1:3137",
		SelfNick: "alice",
		Peers:    []Peer{{Addr: "10.0.0.2:3137", Nick: "bob", LastSeen: at(100)}},
		Now:      at(200),
	})

	self := find(t, g, "10.0.0.1:3137")
	if self.Role != RoleSelf || self.Label != "alice" {
		t.Fatalf("self node = %+v", self)
	}
	if self.X != 0 || self.Y != 0 {
		t.Fatalf("self is not at the centre: (%v,%v)", self.X, self.Y)
	}
	bob := find(t, g, "10.0.0.2:3137")
	if bob.Role != RoleDirect || bob.Label != "bob" || bob.LastSeen != 100 {
		t.Fatalf("peer node = %+v", bob)
	}
	if k, ok := linkKind(g, "10.0.0.1:3137", "10.0.0.2:3137"); !ok || k != LinkDirect {
		t.Fatalf("link to a peer = %q (present=%v), want direct", k, ok)
	}
}

// A node only a peer has mentioned is in the graph, but marked as something we
// have never spoken to.
func TestGossipedStrangerIsIndirect(t *testing.T) {
	g := Build(Input{
		SelfAddr: "10.0.0.1:3137",
		Peers:    []Peer{{Addr: "10.0.0.2:3137", LastSeen: at(100)}},
		Views: map[string]model.PeerView{
			"10.0.0.2:3137": {Peers: []string{"10.0.0.1:3137", "10.0.0.3:3137"}, At: at(150)},
		},
		Now: at(200),
	})

	third := find(t, g, "10.0.0.3:3137")
	if third.Role != RoleIndirect {
		t.Fatalf("third node role = %q, want indirect", third.Role)
	}
	if third.LastSeen != 150 {
		t.Fatalf("third node last seen = %d, want the mention time 150", third.LastSeen)
	}
	if third.Advertised != -1 {
		t.Fatalf("a node that never gossiped advertises %d, want -1", third.Advertised)
	}
	if find(t, g, "10.0.0.2:3137").Advertised != 2 {
		t.Fatal("the gossiping peer's advertised count was not recorded")
	}
}

// The distinction the whole graph exists for: a link both ends claim, against
// one only a single end has reported.
func TestMutualAndObservedLinksAreDistinguished(t *testing.T) {
	g := Build(Input{
		SelfAddr: "10.0.0.1:3137",
		Peers:    []Peer{{Addr: "10.0.0.2:3137"}, {Addr: "10.0.0.3:3137"}},
		Views: map[string]model.PeerView{
			// b and c agree they see each other.
			"10.0.0.2:3137": {Peers: []string{"10.0.0.3:3137"}, At: at(100)},
			"10.0.0.3:3137": {Peers: []string{"10.0.0.2:3137", "10.0.0.9:3137"}, At: at(100)},
		},
		Now: at(200),
	})

	if k, _ := linkKind(g, "10.0.0.2:3137", "10.0.0.3:3137"); k != LinkMutual {
		t.Fatalf("b–c link = %q, want mutual", k)
	}
	if k, _ := linkKind(g, "10.0.0.3:3137", "10.0.0.9:3137"); k != LinkObserved {
		t.Fatalf("c–9 link = %q, want observed", k)
	}
	un := Unconfirmed(g)
	if len(un) != 1 || un[0].B != "10.0.0.9:3137" {
		t.Fatalf("unconfirmed = %+v, want just the c–9 link", un)
	}
}

// A link that touches this node was seen first-hand, so it outranks whatever a
// peer claims about it.
func TestOurOwnLinksOutrankAGossipedClaim(t *testing.T) {
	g := Build(Input{
		SelfAddr: "10.0.0.1:3137",
		Peers:    []Peer{{Addr: "10.0.0.2:3137"}},
		Views: map[string]model.PeerView{
			"10.0.0.2:3137": {Peers: []string{"10.0.0.1:3137"}, At: at(100)},
		},
		Now: at(200),
	})
	if k, _ := linkKind(g, "10.0.0.1:3137", "10.0.0.2:3137"); k != LinkDirect {
		t.Fatalf("self link = %q, want direct even though both ends claim it", k)
	}
}

func TestComponentsSeeAHealthyCluster(t *testing.T) {
	g := Build(Input{
		SelfAddr: "10.0.0.1:3137",
		Peers:    []Peer{{Addr: "10.0.0.2:3137"}, {Addr: "10.0.0.3:3137"}},
		Now:      at(200),
	})
	if got := Components(g); len(got) != 1 {
		t.Fatalf("components = %v, want one group", got)
	}
}

// The failure the graph exists to surface: two halves that each converge and
// silently diverge from the other.
func TestComponentsSeeAPartition(t *testing.T) {
	g := Build(Input{
		SelfAddr: "10.0.0.1:3137",
		Peers:    []Peer{{Addr: "10.0.0.2:3137"}},
		Views: map[string]model.PeerView{
			// A pair that knows only each other, reachable to nobody here.
			"10.0.0.8:3137": {Peers: []string{"10.0.0.9:3137"}, At: at(100)},
			"10.0.0.9:3137": {Peers: []string{"10.0.0.8:3137"}, At: at(100)},
		},
		Now: at(200),
	})
	groups := Components(g)
	if len(groups) != 2 {
		t.Fatalf("components = %v, want two groups", groups)
	}
	if len(groups[0]) != 2 || len(groups[1]) != 2 {
		t.Fatalf("groups = %v, want two pairs", groups)
	}
}

// Two spellings of one address must not be drawn as two nodes.
func TestAddressesAreNormalisedToOneNode(t *testing.T) {
	g := Build(Input{
		SelfAddr: ":3137",
		Peers:    []Peer{{Addr: "127.0.0.1:3138"}},
		Views: map[string]model.PeerView{
			"127.0.0.1:3138": {Peers: []string{"localhost:3137"}, At: at(100)},
		},
		Now: at(200),
	})
	if len(g.Nodes) != 2 {
		t.Fatalf("graph has %d nodes, want 2: %+v", len(g.Nodes), g.Nodes)
	}
	if find(t, g, "127.0.0.1:3137").Role != RoleSelf {
		t.Fatal("the normalised self address is not marked as self")
	}
}

// Placement must be stable: a graph that reshuffles every refresh is unreadable.
func TestPlacementIsDeterministic(t *testing.T) {
	in := Input{
		SelfAddr: "10.0.0.1:3137",
		Peers:    []Peer{{Addr: "10.0.0.2:3137"}, {Addr: "10.0.0.3:3137"}, {Addr: "10.0.0.4:3137"}},
		Now:      at(200),
	}
	first, second := Build(in), Build(in)
	for i := range first.Nodes {
		if first.Nodes[i] != second.Nodes[i] {
			t.Fatalf("node %d differs between builds: %+v vs %+v", i, first.Nodes[i], second.Nodes[i])
		}
	}
	for _, n := range first.Nodes {
		if n.Role == RoleSelf {
			continue
		}
		if r := n.X*n.X + n.Y*n.Y; r < 0.1 || r > 1.01 {
			t.Fatalf("node %s placed at radius^2 %v, outside the unit circle", n.Addr, r)
		}
	}
}

func TestNeighbours(t *testing.T) {
	g := Build(Input{
		SelfAddr: "10.0.0.1:3137",
		Peers:    []Peer{{Addr: "10.0.0.2:3137"}, {Addr: "10.0.0.3:3137"}},
		Now:      at(200),
	})
	got := Neighbours(g, "10.0.0.1:3137")
	if len(got) != 2 || got[0] != "10.0.0.2:3137" || got[1] != "10.0.0.3:3137" {
		t.Fatalf("neighbours = %v", got)
	}
}

// An isolated node still gets a graph: itself, and nothing else.
func TestLoneNodeIsOneComponent(t *testing.T) {
	g := Build(Input{SelfAddr: "10.0.0.1:3137", Now: at(200)})
	if len(g.Nodes) != 1 || len(g.Links) != 0 {
		t.Fatalf("lone graph = %+v / %+v", g.Nodes, g.Links)
	}
	if got := Components(g); len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("components = %v", got)
	}
}
