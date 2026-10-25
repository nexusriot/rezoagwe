package node

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
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
			n.handlePacket(pkt.From, pkt.Data)
		}
	}
}

// handlePacket authenticates one datagram. from is the source address the
// transport observed, which is generally *not* the address the sender
// advertises: a peer is keyed by the host:port it tells the cluster about, and
// its packets arrive from a resolved IP and an ephemeral-looking source.
//
// Received traffic is therefore attributed to the address the body claims,
// falling back to the source when there is none. Keying it on the source
// instead left every peer's receive count at zero on any network where the
// advertised address is a hostname — which then read, to the diagnostics, as
// "this peer never answered".
func (n *Node) handlePacket(from string, frame []byte) {
	src := model.NormalizeAddr(from)
	kind, body, err := n.codec.Decode(frame)
	if err != nil {
		// An unauthenticated packet has no trustworthy body, so there is
		// nothing to attribute it to but where it came from.
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
		n.Metrics.RejectedFrom(src)
		n.noteStranger(src, "")
		log.Debugf("drop %s packet from %s: %s", kind, from, err)
		return
	}
	claimed := senderOf(body)
	attribution := src
	if claimed != "" {
		attribution = model.NormalizeAddr(claimed)
	}
	n.Metrics.RecvFrom(attribution, kind, len(frame))
	// A roster only ever comes from a rendezvous service, which is not a peer
	// and never will be. Noting it first keeps the configured seed out of the
	// strangers list when the roster arrives by datagram — the fallback path,
	// taken whenever the stream exchange cannot be made.
	if kind == pb.KindBootstrapRoster {
		n.noteRendezvous(src)
	}
	n.noteStranger(src, claimed)
	n.dispatch(kind, body)
}

// senderOf reads the address a body claims to come from. Almost every message
// carries one — "from" on the protocol messages, "sender" on chat — and it is
// the address the cluster knows the peer by. Protocol code trusts the body over
// the transport for exactly this reason (see transport.Packet).
func senderOf(body []byte) string {
	var v struct {
		From   string `json:"from"`
		Sender string `json:"sender"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	if v.From != "" {
		return v.From
	}
	return v.Sender
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
	case pb.KindFingerprint:
		n.handleFingerprint(body)
	case pb.KindFingerprintReply:
		n.handleFingerprintReply(body)
	case pb.KindChatPull:
		n.handleChatPull(body)
	case pb.KindChatBacklog:
		n.handleChatBacklog(body)
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
	applied, refused := n.Model.Store.ApplyWithReason(u)
	if applied {
		n.Metrics.KVApplied.Add(1)
		who := n.Model.WriterName(u.Version.Node)
		if u.Action == pb.KVDelete {
			n.logActivity("%s deleted %s (v%d)", who, preview(u.Key), u.Version.Counter)
		} else {
			n.logActivity("%s set %s = %s (v%d)", who, preview(u.Key), preview(u.Value), u.Version.Counter)
		}
		return true
	}
	// "Stale" and "refused" are not the same event and must not share a
	// counter. A node at its key limit used to narrate every entry it would
	// never hold as "ignored stale", for keys it had never seen, while its
	// diagnostics reported it healthy — the one node in the cluster that could
	// not converge was the one claiming nothing was wrong.
	if refused != nil {
		n.Metrics.KVRefused.Add(1)
		n.logActivity("refused %s for %s (v%d from %s): %s",
			u.Action, u.Key, u.Version.Counter, n.Model.WriterName(u.Version.Node), refused)
		return false
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
	if !n.acceptChat(m) {
		return
	}
	n.relayChat(m)
	// Learn about a chat sender we did not know as a peer.
	if n.Model.AddPeer(m.Sender) {
		n.announceJoin(m.Sender, m.Nick)
		n.ev.PeersChanged()
		n.send(m.Sender, pb.KindHello, n.helloMessage())
	}
}

// acceptChat runs one broadcast past the sequence tracker and appends it if it
// is new, reporting whether it was.
//
// A duplicate is dropped rather than appended. That matters more now than it
// did: re-delivering a line is exactly what the repair path does, so without
// this the cure would put a second copy of every recovered line on screen.
func (n *Node) acceptChat(m pb.ChatMessage) bool {
	// Nothing legitimate ever delivers this node its own broadcast: it appends
	// its own lines on send, and no peer echoes them back. A message claiming
	// to be ours is therefore either a bug or a forgery, and taking it is how
	// one bad line spreads — it lands in *our* log, is handed out in every
	// state snapshot afterwards, and every node that syncs seeds its
	// high-water mark for us from it. A single forged sequence then silences
	// this node across the whole cluster. Measured: one datagram, and three
	// peers stopped hearing the author entirely.
	if m.Node != "" && m.Node == n.Model.NodeID {
		n.Metrics.ChatSelfAttributed.Add(1)
		return false
	}
	if m.Node == "" && m.Sender != "" && m.Sender == n.cfg.AdvertiseAddr {
		n.Metrics.ChatSelfAttributed.Add(1)
		return false
	}
	before := n.chatGaps.outstanding()
	recovered := false
	switch n.chatGaps.Observe(m) {
	case chatDuplicate:
		n.Metrics.ChatDuplicates.Add(1)
		return false
	case chatFilledGap:
		n.Metrics.ChatRecovered.Add(1)
		recovered = true
	default:
		if grew := n.chatGaps.outstanding() - before; grew > 0 {
			n.Metrics.ChatGapsFound.Add(uint64(grew))
		}
	}
	kind := pb.ChatMsg
	if m.Action {
		kind = pb.ChatAction
	}
	e := pb.ChatEntry{
		TS:     m.TS,
		Sender: m.Sender,
		Nick:   m.Nick,
		Text:   m.Text,
		Kind:   kind,
		Seq:    m.Seq,
		Node:   m.Node,
	}
	if recovered {
		// Back where it belongs, not at the bottom: a repaired line is older
		// than everything that followed it.
		if e.TS == 0 {
			e.TS = time.Now().Unix()
		}
		n.Model.InsertChat(e)
		n.ev.ChatChanged()
		return true
	}
	n.appendChat(e)
	return true
}

// handleChatPull answers a peer asking for lines of ours it never received.
//
// Only our own broadcasts are served. A third party's copy of someone else's
// Seq 7 is hearsay — it would propagate whatever that node happened to hold,
// including nothing.
func (n *Node) handleChatPull(body []byte) {
	var req pb.ChatPull
	if !n.unmarshal(body, &req) {
		return
	}
	if req.From == "" || len(req.Seqs) == 0 {
		return
	}
	if len(req.Seqs) > pb.MaxChatPull {
		req.Seqs = req.Seqs[:pb.MaxChatPull]
	}
	entries, gone := n.Model.OwnChat(n.cfg.AdvertiseAddr, n.Model.NodeID, req.Seqs)
	reply := pb.ChatBacklog{From: n.cfg.AdvertiseAddr, Unavailable: gone}
	for _, e := range entries {
		reply.Messages = append(reply.Messages, pb.ChatMessage{
			Sender: e.Sender,
			Nick:   e.Nick,
			Text:   e.Text,
			TS:     e.TS,
			Action: e.Kind == pb.ChatAction,
			Seq:    e.Seq,
			Node:   n.Model.NodeID,
		})
	}
	n.Metrics.ChatPullsServed.Add(1)
	n.send(req.From, pb.KindChatBacklog, reply)
}

// handleChatBacklog folds a backlog into the log, and records what the author
// said it can no longer supply.
func (n *Node) handleChatBacklog(body []byte) {
	var reply pb.ChatBacklog
	if !n.unmarshal(body, &reply) {
		return
	}
	for _, m := range reply.Messages {
		// A repair repairs; it never introduces. Anything in a backlog that
		// this node is not already waiting for from this peer is dropped: see
		// chatTracker.Expecting for what accepting it costs.
		if !n.chatGaps.Expecting(authorOf(m), reply.From, m.Seq) {
			n.Metrics.ChatUnsolicited.Add(1)
			n.logActivity("ignored an unasked-for chat line in a backlog from %s",
				n.Model.NickOf(reply.From))
			continue
		}
		n.acceptChat(m)
	}
	if len(reply.Unavailable) > 0 {
		n.chatGaps.MarkUnavailable(reply.From, reply.Unavailable)
		n.Metrics.ChatLost.Add(uint64(len(reply.Unavailable)))
		n.logActivity("%s can no longer supply %d chat line(s) this node missed",
			n.Model.NickOf(reply.From), len(reply.Unavailable))
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
		To:     n.cfg.AdvertiseAddr,
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
		applied, refused := n.Model.Store.ApplyWithReason(u)
		switch {
		case applied:
			n.Metrics.KVApplied.Add(1)
			changed = true
		case refused != nil:
			n.Metrics.KVRefused.Add(1)
		default:
			n.Metrics.KVRejectedStale.Add(1)
		}
	}
	if len(resp.Chat) > 0 {
		n.Model.PrependChat(resp.Chat)
		// A snapshot delivers history in bulk rather than line by line, so the
		// tracker has to be told what it contains — otherwise this node keeps
		// asking the author for lines it is now holding.
		n.chatGaps.Seed(resp.Chat)
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
	n.Model.RecordPeerView(g.From, g.Peers)
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
	n.limitsMu.Lock()
	if h.Limits != nil {
		n.peerLimits[h.From] = *h.Limits
	} else {
		delete(n.peerLimits, h.From)
	}
	n.limitsMu.Unlock()
	n.Model.TouchPeer(h.From)
	if h.Nick != "" {
		n.Model.SetNick(h.From, h.Nick)
	}
	if h.ID != "" {
		n.Model.SetNodeID(h.ID, h.From)
	}
	// The heartbeat says how far this author's own numbering has got, which is
	// how a quiet peer's lines become recoverable at all: without it the
	// baseline waits for them to speak, and anything they said before that is
	// neither delivered nor reported.
	author := h.ID
	if author == "" {
		author = h.From
	}
	before := n.chatGaps.outstanding()
	n.chatGaps.Announce(author, h.From, h.ChatSeq)
	if grew := n.chatGaps.outstanding() - before; grew > 0 {
		n.Metrics.ChatGapsFound.Add(uint64(grew))
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
	n.forgetPeerCursor(g.From)
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

	frame, err := transport.ReadFrameFrom(conn, streamTimeout)
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
		// A whole store can be tens of megabytes; the deadline follows it.
		if err := transport.WriteFrameTo(conn, streamTimeout, resp); err != nil {
			n.Metrics.StreamErrors.Add(1)
			log.Debugf("write state response: %s", err)
			return
		}
		n.Metrics.Sent(pb.KindStateResponse, len(resp))
	case pb.KindFingerprint:
		var req pb.Fingerprint
		if !n.unmarshal(body, &req) {
			return
		}
		n.Model.TouchPeer(req.From)
		// A stream has no datagram ceiling, so this is where a request that
		// named buckets gets the keys in them answered.
		resp, err := n.codec.Encode(pb.KindFingerprintReply, n.fingerprintResponse(req))
		if err != nil {
			log.Errorf("encode fingerprint: %s", err)
			return
		}
		if err := transport.WriteFrameTo(conn, streamTimeout, resp); err != nil {
			n.Metrics.StreamErrors.Add(1)
			log.Debugf("write fingerprint: %s", err)
			return
		}
		n.Metrics.Sent(pb.KindFingerprintReply, len(resp))
	default:
		// Everything else belongs on the datagram path.
		n.dispatch(kind, body)
	}
}

// preview shortens a value for a one-line activity entry.
// preview renders a key or a value for a one-line activity entry.
//
// A key and a value are both arbitrary UTF-8. A newline in either one breaks
// the line in two in every pane that shows it, so one entry reads as two — and
// slicing by byte rather than by rune cuts a multi-byte character in half.
// Both were only ever applied to the value, and only the length half of it.
func preview(v string) string {
	const max = 40
	v = strings.ReplaceAll(v, "\n", " ⏎ ")
	v = strings.ReplaceAll(v, "\r", "")
	v = strings.ReplaceAll(v, "\t", " ")
	r := []rune(v)
	if len(r) > max {
		return string(r[:max-3]) + "..."
	}
	return v
}
