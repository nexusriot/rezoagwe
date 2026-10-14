package node

import (
	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// relayChat forwards a newly-accepted broadcast to peers the author appears not
// to have reached.
//
// A chat broadcast goes to exactly the peers its author knew when it was sent.
// That is fine in a full mesh and wrong the moment peer tables are asymmetric,
// which they routinely are — the topology view already names links only one end
// claims. The store survives this because anti-entropy repairs whatever gossip
// missed; chat has no digest, only the author's own sequence, and that repair
// needs the author still running and still holding the line in its ring. A node
// the author never learned about does not even know to ask.
//
// Only an original broadcast is relayed. A line recovered through the repair
// channel is deliberately not: that channel may only ever fill a gap that was
// asked for, and forwarding from it would be the repair path introducing state
// into the cluster.
func (n *Node) relayChat(m pb.ChatMessage) {
	if m.Hops >= pb.MaxChatHops {
		n.Metrics.ChatRelayCapped.Add(1)
		return
	}
	targets := n.relayTargets(m.Sender)
	if len(targets) == 0 {
		return
	}
	m.Hops++
	for _, addr := range targets {
		n.send(addr, pb.KindChat, m)
	}
	n.Metrics.ChatRelayed.Add(uint64(len(targets)))
}

// relayTargets are the peers worth forwarding to: the ones this node knows
// that the author's own gossiped peer list shows it could not reach.
//
// Forwarding needs *evidence* of asymmetry, not the absence of evidence of
// symmetry. An author this node has heard no gossip from is left alone: it may
// well know everyone here, and treating silence as "reaches nobody" turns one
// line into a packet per peer per peer. Measured on the end-to-end cluster:
// one line in a four-node mesh produced ten duplicate deliveries, every one of
// them dropped on arrival, before this rule was narrowed.
//
// The cost of waiting for a view is bounded, and it is not loss. A node the
// author has not met still learns of it through gossip, Hello tells it where
// that author's numbering stands, and the gap repair asks for what it missed.
// Relaying closes that window early where the evidence is already in hand.
func (n *Node) relayTargets(author string) []string {
	view, ok := n.Model.PeerViews()[author]
	if !ok {
		return nil
	}
	known := view.Peers
	reached := make(map[string]struct{}, len(known)+1)
	for _, addr := range known {
		reached[model.NormalizeAddr(addr)] = struct{}{}
	}
	reached[model.NormalizeAddr(author)] = struct{}{}

	var out []string
	for _, addr := range n.Model.GetNodes() {
		if _, ok := reached[model.NormalizeAddr(addr)]; ok {
			continue
		}
		if n.Model.IsSelf(addr) {
			continue
		}
		out = append(out, addr)
	}
	return out
}
