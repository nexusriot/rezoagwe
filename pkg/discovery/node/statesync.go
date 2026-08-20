package node

import (
	"time"

	log "github.com/sirupsen/logrus"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

const (
	// streamTimeout bounds a whole request/response exchange on a stream.
	streamTimeout = 10 * time.Second
	// datagramSyncLimit caps the entries in a state response that has to fit in
	// one datagram. The stream path has no such limit — this only applies when
	// a peer could not be reached over a stream.
	datagramSyncLimit = 200
	// stateSyncChatLines caps how much chat history a joiner is given.
	stateSyncChatLines = 200
)

// stateResponse builds a snapshot for a joining peer. A limit of 0 means "the
// whole store", which is only safe on the stream path.
func (n *Node) stateResponse(limit int) pb.StateResponse {
	updates := n.Model.Store.Updates()
	if limit > 0 && len(updates) > limit {
		updates = updates[:limit]
	}
	return pb.StateResponse{
		KV:   updates,
		Chat: lastChat(n.Model.ChatLog(), stateSyncChatLines),
	}
}

// lastChat returns the newest n entries a joiner may see.
//
// Direct messages are stripped: they live in the same log as public chat, and
// handing a joining node someone else's private conversation because it asked
// for history would be a quiet privacy leak.
func lastChat(entries []pb.ChatEntry, n int) []pb.ChatEntry {
	out := make([]pb.ChatEntry, 0, len(entries))
	for _, e := range entries {
		if e.Kind == pb.ChatDirect {
			continue
		}
		out = append(out, e)
	}
	if len(out) <= n {
		return out
	}
	return out[len(out)-n:]
}

func (n *Node) requestStateFromRandomPeer() {
	if peer := n.randomPeer(); peer != "" {
		n.requestStateFrom(peer)
	}
}

// requestStateFrom pulls a peer's snapshot, preferring the stream path so the
// response is not capped by the datagram size. A store or chat history larger
// than a datagram used to sync as nothing at all: the send simply failed.
func (n *Node) requestStateFrom(peer string) {
	req := pb.StateRequest{From: n.cfg.NodeAddr}
	if n.exchangeStream(peer, pb.KindStateRequest, req) {
		return
	}
	n.send(peer, pb.KindStateRequest, req)
}

// exchangeStream performs one framed request/response round trip and reports
// whether it succeeded.
func (n *Node) exchangeStream(peer string, kind pb.MessageKind, v interface{}) bool {
	conn, err := n.tr.Dial(peer)
	if err != nil {
		log.Debugf("stream dial %s: %s", peer, err)
		return false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(streamTimeout))

	pkt, err := n.codec.Encode(kind, v)
	if err != nil {
		log.Errorf("encode %s: %s", kind, err)
		return false
	}
	if err := transport.WriteFrame(conn, pkt); err != nil {
		n.Metrics.StreamErrors.Add(1)
		return false
	}
	n.Metrics.Sent(kind, len(pkt))

	frame, err := transport.ReadFrame(conn)
	if err != nil {
		n.Metrics.StreamErrors.Add(1)
		log.Debugf("stream read from %s: %s", peer, err)
		return false
	}
	respKind, body, err := n.codec.Decode(frame)
	if err != nil {
		n.Metrics.AuthFailures.Add(1)
		return false
	}
	n.Metrics.Recv(respKind, len(frame))
	n.dispatch(respKind, body)
	return true
}

// registerWithBootstrap announces this node to every configured seed. Several
// seeds mean the rendezvous service is no longer a single point of failure for
// joiners.
func (n *Node) registerWithBootstrap() {
	reg := pb.BootstrapRegister{From: n.cfg.NodeAddr, Nick: n.Model.Nick()}
	for _, seed := range n.cfg.BootstrapAddrs {
		if seed == "" {
			continue
		}
		n.send(seed, pb.KindBootstrapRegister, reg)
	}
}

// discoverFromBootstrap asks the seeds for the roster. The stream path is tried
// first (an unbounded roster), and the datagram path is the fallback — its
// reply arrives asynchronously through the normal packet loop, so a dead
// bootstrap can never stall startup.
func (n *Node) discoverFromBootstrap() {
	req := pb.BootstrapDiscover{From: n.cfg.NodeAddr}
	for _, seed := range n.cfg.BootstrapAddrs {
		if seed == "" {
			continue
		}
		if n.exchangeStream(seed, pb.KindBootstrapDiscover, req) {
			return
		}
	}
	for _, seed := range n.cfg.BootstrapAddrs {
		if seed == "" {
			continue
		}
		n.send(seed, pb.KindBootstrapDiscover, req)
	}
}

// applyRoster adopts the peers a bootstrap reported.
func (n *Node) applyRoster(roster pb.BootstrapRoster) {
	learned := false
	for _, p := range roster.Peers {
		if p.Nick != "" {
			n.Model.SetNick(p.Addr, p.Nick)
		}
		if n.Model.AddPeer(p.Addr) {
			learned = true
		}
	}
	if !learned {
		return
	}
	n.ev.PeersChanged()
	n.helloAllPeers()
	n.requestStateFromRandomPeer()
}
