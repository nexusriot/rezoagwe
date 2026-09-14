// Package topology assembles the cluster graph from what a node already holds:
// its own peer table, and the peer lists its peers have gossiped.
//
// Nothing here needs a byte of extra protocol. The picture is exactly as
// complete as gossip has made it, which is itself the diagnostic: a link only
// one end claims is a link one end cannot see, and a graph in two pieces is a
// partition that no KV counter reports.
package topology

import (
	"math"
	"sort"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
)

// Role is what a node is to the node doing the looking.
type Role string

const (
	// RoleSelf is the node building the graph.
	RoleSelf Role = "self"
	// RoleDirect is a peer this node exchanges packets with.
	RoleDirect Role = "direct"
	// RoleIndirect is a node only ever mentioned by someone else.
	RoleIndirect Role = "indirect"
)

// LinkKind is how much is known about a link.
type LinkKind string

const (
	// LinkDirect touches this node, so it was observed first-hand.
	LinkDirect LinkKind = "direct"
	// LinkMutual is claimed by both ends.
	LinkMutual LinkKind = "mutual"
	// LinkObserved is claimed by one end only, and cannot be confirmed.
	LinkObserved LinkKind = "observed"
)

const (
	// shellCapacity is how many nodes share a ring before another is started,
	// so labels stay readable.
	shellCapacity = 10
	// innerRadius is where the first ring sits, leaving the middle to self.
	innerRadius = 0.45
)

// Node is one node in the graph.
type Node struct {
	Addr  string `json:"addr"`
	Label string `json:"label"`
	Role  Role   `json:"role"`
	// LastSeen is when a packet last arrived from this node, or — for one we
	// have never heard from — when anyone last mentioned it.
	LastSeen int64 `json:"last_seen"`
	Degree   int   `json:"degree"`
	// Advertised is how many peers this node claimed the last time it gossiped,
	// and -1 when it never has.
	Advertised int `json:"advertised"`
	// X and Y place the node for drawing, normalised to [-1, 1].
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Link is one edge.
type Link struct {
	A    string   `json:"a"`
	B    string   `json:"b"`
	Kind LinkKind `json:"kind"`
}

// Graph is the assembled picture.
type Graph struct {
	Nodes       []Node `json:"nodes"`
	Links       []Link `json:"links"`
	GeneratedAt int64  `json:"generated_at"`
}

// Input is everything Build needs, so the assembly stays a pure function of
// state a caller can fabricate in a test.
type Input struct {
	SelfAddr string
	SelfNick string
	Peers    []Peer
	Views    map[string]model.PeerView
	Now      time.Time
}

// Peer is one entry of the looking node's own peer table.
type Peer struct {
	Addr     string
	Nick     string
	LastSeen time.Time
}

// Build assembles the graph.
func Build(in Input) Graph {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	self := model.NormalizeAddr(in.SelfAddr)

	direct := make(map[string]Peer, len(in.Peers))
	for _, p := range in.Peers {
		direct[model.NormalizeAddr(p.Addr)] = p
	}

	// claims[a] is the set of nodes a says it is connected to.
	claims := make(map[string]map[string]struct{})
	claim := func(from, to string) {
		if from == "" || to == "" || from == to {
			return
		}
		if claims[from] == nil {
			claims[from] = make(map[string]struct{})
		}
		claims[from][to] = struct{}{}
	}
	for addr := range direct {
		claim(self, addr)
	}
	views := make(map[string]model.PeerView, len(in.Views))
	for owner, v := range in.Views {
		norm := model.NormalizeAddr(owner)
		views[norm] = v
		for _, target := range v.Peers {
			claim(norm, model.NormalizeAddr(target))
		}
	}

	addrs := map[string]struct{}{self: {}}
	for addr := range direct {
		addrs[addr] = struct{}{}
	}
	for from, targets := range claims {
		addrs[from] = struct{}{}
		for to := range targets {
			addrs[to] = struct{}{}
		}
	}

	links := buildLinks(self, claims)

	degrees := make(map[string]int, len(addrs))
	for _, l := range links {
		degrees[l.A]++
		degrees[l.B]++
	}

	// For a node we have never exchanged packets with, the last time anyone
	// mentioned it is the only freshness there is.
	mentioned := make(map[string]int64)
	for owner, v := range views {
		at := v.At.Unix()
		if at > mentioned[owner] {
			mentioned[owner] = at
		}
		for _, target := range v.Peers {
			key := model.NormalizeAddr(target)
			if at > mentioned[key] {
				mentioned[key] = at
			}
		}
	}

	nodes := make([]Node, 0, len(addrs))
	for addr := range addrs {
		peer, isPeer := direct[addr]
		view, hasView := views[addr]

		n := Node{Addr: addr, Degree: degrees[addr], Advertised: -1}
		switch {
		case addr == self:
			n.Role = RoleSelf
			n.Label = in.SelfNick
			if n.Label == "" {
				n.Label = "this node"
			}
		case isPeer:
			n.Role = RoleDirect
			n.Label = peer.Nick
		default:
			n.Role = RoleIndirect
		}
		if n.Label == "" {
			n.Label = addr
		}
		switch {
		case isPeer && !peer.LastSeen.IsZero():
			n.LastSeen = peer.LastSeen.Unix()
		case hasView:
			n.LastSeen = view.At.Unix()
		default:
			n.LastSeen = mentioned[addr]
		}
		if hasView {
			n.Advertised = len(view.Peers)
		}
		nodes = append(nodes, n)
	}

	roleOrder := map[Role]int{RoleSelf: 0, RoleDirect: 1, RoleIndirect: 2}
	sort.Slice(nodes, func(i, j int) bool {
		if roleOrder[nodes[i].Role] != roleOrder[nodes[j].Role] {
			return roleOrder[nodes[i].Role] < roleOrder[nodes[j].Role]
		}
		return nodes[i].Addr < nodes[j].Addr
	})

	g := Graph{Nodes: nodes, Links: links, GeneratedAt: now.Unix()}
	place(&g)
	return g
}

// buildLinks turns the claim sets into one edge per pair.
func buildLinks(self string, claims map[string]map[string]struct{}) []Link {
	froms := make([]string, 0, len(claims))
	for from := range claims {
		froms = append(froms, from)
	}
	sort.Strings(froms)

	seen := make(map[string]struct{})
	links := make([]Link, 0)
	for _, from := range froms {
		targets := make([]string, 0, len(claims[from]))
		for to := range claims[from] {
			targets = append(targets, to)
		}
		sort.Strings(targets)
		for _, to := range targets {
			pair := from + "|" + to
			if from > to {
				pair = to + "|" + from
			}
			if _, dup := seen[pair]; dup {
				continue
			}
			seen[pair] = struct{}{}

			_, bothWays := claims[to][from]
			kind := LinkObserved
			switch {
			case from == self || to == self:
				// Our own links were observed first-hand, which outranks a
				// gossiped claim about them.
				kind = LinkDirect
			case bothWays:
				kind = LinkMutual
			}
			links = append(links, Link{A: from, B: to, Kind: kind})
		}
	}
	return links
}

// Components groups nodes that can reach each other through known links.
//
// More than one group means the cluster is split: each half converges
// internally and diverges from the other, which is the failure a KV store
// cannot report on its own.
func Components(g Graph) [][]string {
	adjacency := make(map[string]map[string]struct{})
	for _, l := range g.Links {
		if l.A == l.B {
			continue
		}
		if adjacency[l.A] == nil {
			adjacency[l.A] = make(map[string]struct{})
		}
		if adjacency[l.B] == nil {
			adjacency[l.B] = make(map[string]struct{})
		}
		adjacency[l.A][l.B] = struct{}{}
		adjacency[l.B][l.A] = struct{}{}
	}

	remaining := make(map[string]struct{}, len(g.Nodes))
	order := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		remaining[n.Addr] = struct{}{}
		order = append(order, n.Addr)
	}
	sort.Strings(order)

	groups := make([][]string, 0)
	for _, seed := range order {
		if _, ok := remaining[seed]; !ok {
			continue
		}
		delete(remaining, seed)
		group := []string{}
		queue := []string{seed}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			group = append(group, cur)
			neighbours := make([]string, 0, len(adjacency[cur]))
			for next := range adjacency[cur] {
				neighbours = append(neighbours, next)
			}
			sort.Strings(neighbours)
			for _, next := range neighbours {
				if _, ok := remaining[next]; ok {
					delete(remaining, next)
					queue = append(queue, next)
				}
			}
		}
		sort.Strings(group)
		groups = append(groups, group)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if len(groups[i]) != len(groups[j]) {
			return len(groups[i]) > len(groups[j])
		}
		return groups[i][0] < groups[j][0]
	})
	return groups
}

// Unconfirmed returns the links only one end has told us about.
func Unconfirmed(g Graph) []Link {
	out := make([]Link, 0)
	for _, l := range g.Links {
		if l.Kind == LinkObserved {
			out = append(out, l)
		}
	}
	return out
}

// Neighbours lists the nodes linked to addr.
func Neighbours(g Graph, addr string) []string {
	set := make(map[string]struct{})
	for _, l := range g.Links {
		switch addr {
		case l.A:
			set[l.B] = struct{}{}
		case l.B:
			set[l.A] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// place assigns drawing coordinates: this node in the middle, peers on the
// first ring, nodes we only heard about further out.
//
// Coordinates are normalised so the layout is independent of whatever draws it,
// and the order comes from the sorted address so a node does not jump between
// frames.
func place(g *Graph) {
	byRole := func(role Role) []string {
		var out []string
		for _, n := range g.Nodes {
			if n.Role == role {
				out = append(out, n.Addr)
			}
		}
		sort.Strings(out)
		return out
	}
	chunk := func(addrs []string) [][]string {
		var out [][]string
		for i := 0; i < len(addrs); i += shellCapacity {
			end := min(i+shellCapacity, len(addrs))
			out = append(out, addrs[i:end])
		}
		return out
	}

	pos := make(map[string][2]float64)
	for _, addr := range byRole(RoleSelf) {
		pos[addr] = [2]float64{0, 0}
	}
	// Peers first, then nodes we only heard about: a shell never mixes the two,
	// so distance from the middle means "how far from us", not "how many fitted".
	shells := append(chunk(byRole(RoleDirect)), chunk(byRole(RoleIndirect))...)
	for i, shell := range shells {
		radius := (innerRadius + 1) / 2
		if len(shells) > 1 {
			radius = innerRadius + (1-innerRadius)*float64(i)/float64(len(shells)-1)
		}
		// Every other shell is rotated half a slot so a node never sits exactly
		// behind the one on the shell inside it.
		offset := 0.0
		if i%2 == 1 {
			offset = math.Pi / float64(len(shell))
		}
		for slot, addr := range shell {
			angle := -math.Pi/2 + offset + 2*math.Pi*float64(slot)/float64(len(shell))
			pos[addr] = [2]float64{radius * math.Cos(angle), radius * math.Sin(angle)}
		}
	}
	for i := range g.Nodes {
		if p, ok := pos[g.Nodes[i].Addr]; ok {
			g.Nodes[i].X, g.Nodes[i].Y = p[0], p[1]
		}
	}
}
