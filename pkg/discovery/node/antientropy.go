package node

import (
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// batchBytes is the soft byte budget for one KVBatch datagram, leaving room for
// the frame header under the practical UDP limit.
const batchBytes = 48 << 10

// batchEntries caps the entries in one batch regardless of size.
const batchEntries = 64

// antiEntropyRound advertises a slice of the local keyspace to one peer.
//
// This is what closes the last correctness gap in the replication model: a
// dropped KV datagram was previously never re-sent, so two stores stayed
// divergent until the next overlapping write. The digest carries versions, not
// values, so a round is cheap; the peer answers with exactly the repairs that
// are needed in either direction.
//
// The cursor walks the sorted keyspace so a store larger than one digest is
// still covered completely, batch by batch, instead of endlessly re-comparing
// the first N keys. There is one cursor per peer: a shared cursor split the
// keyspace among whichever peers the random target happened to pick, so
// covering the whole store against any one peer took as many wraps as there
// were peers.
func (n *Node) antiEntropyRound(target string) {
	if target == "" {
		return
	}
	n.aeMu.Lock()
	if n.aeCursors == nil {
		n.aeCursors = make(map[string]string)
	}
	d := n.Model.Store.Digest(n.aeCursors[target], n.cfg.DigestBatch)
	if d.Hi == "" {
		delete(n.aeCursors, target) // covered the tail of the keyspace; start over
	} else if len(d.Entries) > 0 {
		n.aeCursors[target] = d.Entries[len(d.Entries)-1].Key
	}
	n.aeMu.Unlock()

	d.From = n.cfg.AdvertiseAddr
	n.Metrics.AERounds.Add(1)
	n.send(target, pb.KindDigest, d)
}

// forgetPeerCursor drops a departed peer's cursor, so its address does not
// accumulate in a long-running node and a peer that returns starts a clean
// sweep rather than resuming a walk from before it left.
func (n *Node) forgetPeerCursor(addr string) {
	n.aeMu.Lock()
	delete(n.aeCursors, addr)
	n.aeMu.Unlock()
}

// handleDigest answers a peer's digest: push what this node holds and the peer
// does not (or holds staler), pull what the peer holds and this node does not.
func (n *Node) handleDigest(body []byte) {
	var d pb.Digest
	if !n.unmarshal(body, &d) {
		return
	}
	n.Model.TouchPeer(d.From)
	push, pull := n.Model.Store.Reconcile(d, n.cfg.MaxPush, n.cfg.MaxPull)

	if len(push) > 0 {
		n.Metrics.AEPushed.Add(uint64(len(push)))
		n.logActivity("anti-entropy: pushing %d entr%s to %s",
			len(push), plural(len(push)), n.peerName(d.From))
		n.sendUpdates(d.From, push)
	}
	if len(pull) > 0 {
		n.Metrics.AEPulled.Add(uint64(len(pull)))
		n.logActivity("anti-entropy: pulling %d entr%s from %s",
			len(pull), plural(len(pull)), n.peerName(d.From))
		n.send(d.From, pb.KindPullRequest, pb.PullRequest{From: n.cfg.AdvertiseAddr, Keys: pull})
	}
}

func (n *Node) handlePullRequest(body []byte) {
	var req pb.PullRequest
	if !n.unmarshal(body, &req) {
		return
	}
	n.Model.TouchPeer(req.From)
	updates := n.Model.Store.UpdatesFor(req.Keys)
	if len(updates) == 0 {
		return
	}
	n.sendUpdates(req.From, updates)
}

// sendUpdates ships updates as one or more batches, splitting on a byte budget
// so a repair of large values does not build a datagram nothing can carry.
func (n *Node) sendUpdates(addr string, updates []pb.KVUpdate) {
	batch := make([]pb.KVUpdate, 0, batchEntries)
	size := 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		n.send(addr, pb.KindKVBatch, pb.KVBatch{Updates: batch})
		batch = make([]pb.KVUpdate, 0, batchEntries)
		size = 0
	}
	for _, u := range updates {
		// Rough per-entry overhead for the JSON keys and the version object.
		entrySize := len(u.Key) + len(u.Value) + 160
		if len(batch) > 0 && (size+entrySize > batchBytes || len(batch) >= batchEntries) {
			flush()
		}
		batch = append(batch, u)
		size += entrySize
	}
	flush()
}

func (n *Node) peerName(addr string) string {
	if nick := n.Model.NickOf(addr); nick != "" {
		return nick
	}
	return addr
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
