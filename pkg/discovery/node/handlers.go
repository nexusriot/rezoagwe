package node

import (
	"encoding/json"
	"errors"
	"net"
	"time"

	log "github.com/sirupsen/logrus"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// packetLoop authenticates and dispatches inbound datagrams.
func (n *Node) packetLoop() {
	packets := n.tr.Packets()
	for {
		select {
		case <-n.stop:
			return
		case pkt, ok := <-packets:
			if !ok {
				return
			}
			n.handlePacket(pkt.Data)
		}
	}
}

func (n *Node) handlePacket(frame []byte) {
	kind, body, err := n.codec.Decode(frame)
	if err != nil {
		switch {
		case errors.Is(err, pb.ErrBadMAC):
			n.Metrics.AuthFailures.Add(1)
		case errors.Is(err, pb.ErrReplay):
			n.Metrics.ReplayDrops.Add(1)
		case errors.Is(err, pb.ErrClockSkew):
			n.Metrics.SkewDrops.Add(1)
		default:
			n.Metrics.MalformedDrops.Add(1)
		}
		log.Debugf("drop %s packet: %s", kind, err)
		return
	}
	n.Metrics.Recv(kind, len(frame))
	n.dispatch(kind, body)
}

func (n *Node) dispatch(kind pb.MessageKind, body []byte) {
	switch kind {
	case pb.KindKV:
		n.handleKV(body)
	case pb.KindKVBatch:
		n.handleKVBatch(body)
	case pb.KindChat:
		n.handleChat(body)
	case pb.KindDirectMessage:
		n.handleDirectMessage(body)
	case pb.KindStateRequest:
		n.handleStateRequest(body)
	case pb.KindStateResponse:
		n.handleStateResponse(body)
	case pb.KindPeerGossip:
		n.handlePeerGossip(body)
	case pb.KindHello:
		n.handleHello(body)
	case pb.KindGoodbye:
		n.handleGoodbye(body)
	case pb.KindDigest:
		n.handleDigest(body)
	case pb.KindPullRequest:
		n.handlePullRequest(body)
	case pb.KindBootstrapRoster:
		n.handleBootstrapRoster(body)
	default:
		n.Metrics.MalformedDrops.Add(1)
		log.Debugf("unknown kind: %d", kind)
	}
}

func (n *Node) unmarshal(body []byte, v interface{}) bool {
	if err := json.Unmarshal(body, v); err != nil {
		n.Metrics.MalformedDrops.Add(1)
		log.Debugf("unmarshal: %s", err)
		return false
	}
	return true
}

// applyRemote merges one remote update, counting and narrating the outcome.
// A rejected update is not an error — it is a conflict that last-write-wins
// resolved — but it is invisible without the activity feed, which is exactly
// when replication bugs hide.
func (n *Node) applyRemote(u pb.KVUpdate) bool {
	if n.Model.Store.Apply(u) {
		n.Metrics.KVApplied.Add(1)
		who := n.Model.WriterName(u.Version.Node)
		if u.Action == pb.KVDelete {
			n.logActivity("%s deleted %s (v%d)", who, u.Key, u.Version.Counter)
		} else {
			n.logActivity("%s set %s = %s (v%d)", who, u.Key, preview(u.Value), u.Version.Counter)
		}
		return true
	}
	n.Metrics.KVRejectedStale.Add(1)
	n.logActivity("ignored stale %s for %s (v%d from %s)",
		u.Action, u.Key, u.Version.Counter, n.Model.WriterName(u.Version.Node))
	return false
}

func (n *Node) handleKV(body []byte) {
	var u pb.KVUpdate
	if !n.unmarshal(body, &u) {
		return
	}
	if n.applyRemote(u) {
		n.ev.KVChanged()
	}
}

func (n *Node) handleKVBatch(body []byte) {
	var b pb.KVBatch
	if !n.unmarshal(body, &b) {
		return
	}
	changed := false
	for _, u := range b.Updates {
		if n.applyRemote(u) {
			changed = true
		}
	}
	if changed {
		n.ev.KVChanged()
	}
}

func (n *Node) handleChat(body []byte) {
	var m pb.ChatMessage
	if !n.unmarshal(body, &m) {
		return
	}
	n.Model.TouchPeer(m.Sender)
	if m.Nick != "" {
		n.Model.SetNick(m.Sender, m.Nick)
	}
	kind := pb.ChatMsg
	if m.Action {
		kind = pb.ChatAction
	}
	n.appendChat(pb.ChatEntry{
		TS:     m.TS,
		Sender: m.Sender,
		Nick:   m.Nick,
		Text:   m.Text,
		Kind:   kind,
	})
	// Learn about a chat sender we did not know as a peer.
	if n.Model.AddPeer(m.Sender) {
		n.announceJoin(m.Sender, m.Nick)
		n.ev.PeersChanged()
		n.send(m.Sender, pb.KindHello, n.helloMessage())
	}
}

func (n *Node) handleDirectMessage(body []byte) {
	var m pb.ChatMessage
	if !n.unmarshal(body, &m) {
		return
	}
	n.Model.TouchPeer(m.Sender)
	if m.Nick != "" {
		n.Model.SetNick(m.Sender, m.Nick)
	}
	n.appendChat(pb.ChatEntry{
		TS:     m.TS,
		Sender: m.Sender,
		Nick:   m.Nick,
		Text:   m.Text,
		Kind:   pb.ChatDirect,
		To:     n.cfg.NodeAddr,
	})
}

func (n *Node) handleStateRequest(body []byte) {
	var req pb.StateRequest
	if !n.unmarshal(body, &req) {
		return
	}
	n.Model.TouchPeer(req.From)
	// The datagram path has to stay inside one packet, so it ships a bounded
	// slice of the store. A peer that used the stream path gets everything.
	n.Metrics.StateSyncOut.Add(1)
	n.send(req.From, pb.KindStateResponse, n.stateResponse(datagramSyncLimit))
}

func (n *Node) handleStateResponse(body []byte) {
	var resp pb.StateResponse
	if !n.unmarshal(body, &resp) {
		return
	}
	n.mergeState(resp)
}

// mergeState merges a peer snapshot under last-write-wins rather than
// overwriting, so a local edit made before the sync arrived is not clobbered by
// a stale value from the responder.
func (n *Node) mergeState(resp pb.StateResponse) {
	n.Metrics.StateSyncIn.Add(1)
	changed := false
	for _, u := range resp.KV {
		if n.Model.Store.Apply(u) {
			n.Metrics.KVApplied.Add(1)
			changed = true
		}
	}
	if len(resp.Chat) > 0 {
		n.Model.PrependChat(resp.Chat)
		n.ev.ChatChanged()
	}
	if changed {
		n.logActivity("state sync merged %d entries", len(resp.KV))
		n.ev.KVChanged()
	}
}

func (n *Node) handlePeerGossip(body []byte) {
	var g pb.PeerGossip
	if !n.unmarshal(body, &g) {
		return
	}
	n.Model.TouchPeer(g.From)
	if g.Nick != "" {
		n.Model.SetNick(g.From, g.Nick)
	}
	if g.ID != "" {
		n.Model.SetNodeID(g.ID, g.From)
	}
	changed := false
	if n.Model.AddPeer(g.From) {
		changed = true
		n.announceJoin(g.From, g.Nick)
		n.send(g.From, pb.KindHello, n.helloMessage())
	}
	for _, p := range g.Peers {
		if n.Model.AddPeer(p) {
			changed = true
			n.announceJoin(p, n.Model.NickOf(p))
			n.send(p, pb.KindHello, n.helloMessage())
		}
	}
	if changed {
		n.ev.PeersChanged()
	}
}

func (n *Node) handleHello(body []byte) {
	var h pb.Hello
	if !n.unmarshal(body, &h) {
		return
	}
	n.Model.TouchPeer(h.From)
	if h.Nick != "" {
		n.Model.SetNick(h.From, h.Nick)
	}
	if h.ID != "" {
		n.Model.SetNodeID(h.ID, h.From)
	}
	if n.Model.AddPeer(h.From) {
		n.announceJoin(h.From, h.Nick)
		n.ev.PeersChanged()
	}
}

// handleGoodbye drops a peer that announced a clean shutdown. The nick is
// resolved before removal (RemovePeer forgets it) so the "left" line renders
// the same way an eviction would.
func (n *Node) handleGoodbye(body []byte) {
	var g pb.Goodbye
	if !n.unmarshal(body, &g) {
		return
	}
	if !n.Model.HasPeer(g.From) {
		return
	}
	nick := g.Nick
	if nick == "" {
		nick = n.Model.NickOf(g.From)
	}
	n.Model.RemovePeer(g.From)
	n.announceLeave(g.From, nick)
	n.ev.PeersChanged()
}

func (n *Node) handleBootstrapRoster(body []byte) {
	var roster pb.BootstrapRoster
	if !n.unmarshal(body, &roster) {
		return
	}
	n.applyRoster(roster)
}

// streamLoop serves inbound reliable streams. A stream carries exactly one
// request frame and one response frame, which is all state sync needs and
// keeps the connection short-lived.
func (n *Node) streamLoop() {
	streams := n.tr.Streams()
	for {
		select {
		case <-n.stop:
			return
		case conn, ok := <-streams:
			if !ok {
				return
			}
			go n.serveStream(conn)
		}
	}
}

func (n *Node) serveStream(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(streamTimeout))

	frame, err := transport.ReadFrame(conn)
	if err != nil {
		n.Metrics.StreamErrors.Add(1)
		log.Debugf("read stream frame: %s", err)
		return
	}
	kind, body, err := n.codec.Decode(frame)
	if err != nil {
		n.Metrics.AuthFailures.Add(1)
		log.Debugf("drop stream frame: %s", err)
		return
	}
	n.Metrics.Recv(kind, len(frame))

	switch kind {
	case pb.KindStateRequest:
		var req pb.StateRequest
		if !n.unmarshal(body, &req) {
			return
		}
		n.Model.TouchPeer(req.From)
		n.Metrics.StateSyncOut.Add(1)
		resp, err := n.codec.Encode(pb.KindStateResponse, n.stateResponse(0))
		if err != nil {
			log.Errorf("encode state response: %s", err)
			return
		}
		if err := transport.WriteFrame(conn, resp); err != nil {
			n.Metrics.StreamErrors.Add(1)
			log.Debugf("write state response: %s", err)
			return
		}
		n.Metrics.Sent(pb.KindStateResponse, len(resp))
	default:
		// Everything else belongs on the datagram path.
		n.dispatch(kind, body)
	}
}

// preview shortens a value for a one-line activity entry.
func preview(v string) string {
	const max = 40
	if len(v) > max {
		return v[:max-3] + "..."
	}
	return v
}
