package node

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// consistencyTimeout bounds one peer's answer. A peer that cannot summarise its
// store in this long is reported unreachable rather than holding up the report
// for everyone else.
const consistencyTimeout = 5 * time.Second

// PeerConsistency is one peer's answer to "what do you hold?".
type PeerConsistency struct {
	Addr string `json:"addr"`
	Nick string `json:"nick,omitempty"`
	// Reachable is false when the peer never answered; Error says why.
	Reachable  bool   `json:"reachable"`
	Error      string `json:"error,omitempty"`
	Keys       int    `json:"keys"`
	Tombstones int    `json:"tombstones"`
	Clock      uint64 `json:"clock"`
	// Agrees is true when every bucket matched.
	Agrees bool `json:"agrees"`
	// DifferingBuckets are the indices whose digests did not match, which
	// localises a divergence to a region of the keyspace.
	DifferingBuckets []int `json:"differing_buckets,omitempty"`
}

// ConsistencyReport is the answer to the question anti-entropy never asks out
// loud: are the replicas actually the same right now?
//
// Replication repairs divergence but reports nothing, so a cluster can sit
// split — or a peer can quietly refuse everything it is sent — for as long as
// nobody looks. This is the looking.
type ConsistencyReport struct {
	Addr       string            `json:"addr"`
	Keys       int               `json:"keys"`
	Tombstones int               `json:"tombstones"`
	Clock      uint64            `json:"clock"`
	CheckedAt  int64             `json:"checked_at"`
	Peers      []PeerConsistency `json:"peers"`
	// Converged is true when every peer that answered agreed on every bucket.
	Converged bool `json:"converged"`
	// Unreachable counts the peers that never answered; a report over a peer
	// that did not reply says nothing about whether it agrees.
	Unreachable int `json:"unreachable"`
}

// CheckConsistency asks every peer to summarise its store and compares the
// answers against this node's.
//
// It goes over streams, not datagrams: a dropped answer would read as a peer
// that disagrees, which is exactly the wrong conclusion to draw from packet
// loss.
func (n *Node) CheckConsistency() ConsistencyReport {
	local := n.Model.Store.Fingerprint(pb.FingerprintBuckets)
	report := ConsistencyReport{
		Addr:       n.cfg.AdvertiseAddr,
		Keys:       local.Keys,
		Tombstones: local.Tombstones,
		Clock:      local.Clock,
		CheckedAt:  time.Now().Unix(),
		Converged:  true,
		Peers:      make([]PeerConsistency, 0),
	}

	peers := n.Model.GetNodes()
	results := make([]PeerConsistency, len(peers))
	var wg sync.WaitGroup
	for i, addr := range peers {
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			results[i] = n.fingerprintPeer(addr, local)
		}(i, addr)
	}
	wg.Wait()

	for _, r := range results {
		if !r.Reachable {
			report.Unreachable++
		} else if !r.Agrees {
			report.Converged = false
		}
		report.Peers = append(report.Peers, r)
	}
	sort.Slice(report.Peers, func(i, j int) bool { return report.Peers[i].Addr < report.Peers[j].Addr })
	return report
}

// fingerprintPeer asks one peer for its summary and compares it with ours.
func (n *Node) fingerprintPeer(addr string, local pb.FingerprintReply) PeerConsistency {
	out := PeerConsistency{Addr: addr, Nick: n.Model.NickOf(addr)}

	body, err := n.requestOverStream(addr, pb.KindFingerprint, pb.Fingerprint{From: n.cfg.AdvertiseAddr})
	if err != nil {
		out.Error = err.Error()
		return out
	}
	var reply pb.FingerprintReply
	if err := json.Unmarshal(body, &reply); err != nil {
		out.Error = "malformed reply: " + err.Error()
		return out
	}

	out.Reachable = true
	out.Keys = reply.Keys
	out.Tombstones = reply.Tombstones
	out.Clock = reply.Clock
	if reply.Nick != "" {
		out.Nick = reply.Nick
	}
	if len(reply.Buckets) != len(local.Buckets) {
		// A peer summarising at a different granularity cannot be compared
		// bucket by bucket; say so rather than reporting a false divergence.
		out.Error = fmt.Sprintf("peer reported %d buckets, this node uses %d",
			len(reply.Buckets), len(local.Buckets))
		return out
	}
	for i := range local.Buckets {
		if local.Buckets[i] != reply.Buckets[i] {
			out.DifferingBuckets = append(out.DifferingBuckets, i)
		}
	}
	out.Agrees = len(out.DifferingBuckets) == 0
	return out
}

// requestOverStream performs one framed request/response round trip and hands
// the response body back, instead of dispatching it like exchangeStream does.
// A consistency check needs the answer, not a side effect.
func (n *Node) requestOverStream(peer string, kind pb.MessageKind, v interface{}) ([]byte, error) {
	conn, err := n.tr.Dial(peer)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(consistencyTimeout))

	pkt, err := n.codec.Encode(kind, v)
	if err != nil {
		return nil, err
	}
	if err := transport.WriteFrame(conn, pkt); err != nil {
		n.Metrics.StreamErrors.Add(1)
		return nil, err
	}
	n.Metrics.Sent(kind, len(pkt))

	frame, err := transport.ReadFrame(conn)
	if err != nil {
		n.Metrics.StreamErrors.Add(1)
		return nil, err
	}
	respKind, body, err := n.codec.Decode(frame)
	if err != nil {
		n.Metrics.AuthFailures.Add(1)
		return nil, err
	}
	n.Metrics.Recv(respKind, len(frame))
	if respKind != pb.KindFingerprintOK {
		return nil, fmt.Errorf("peer answered with %s", respKind)
	}
	return body, nil
}

// fingerprintReply builds this node's summary, labelled so a report can name
// the peer that produced it.
func (n *Node) fingerprintReply() pb.FingerprintReply {
	r := n.Model.Store.Fingerprint(pb.FingerprintBuckets)
	r.From = n.cfg.AdvertiseAddr
	r.Nick = n.Model.Nick()
	return r
}

// handleFingerprint answers a summary request that arrived as a datagram. The
// stream path answers on the connection instead, which is what a checker uses;
// this exists so the message is not silently unknown on the datagram path.
func (n *Node) handleFingerprint(body []byte) {
	var req pb.Fingerprint
	if !n.unmarshal(body, &req) {
		return
	}
	if req.From == "" {
		return
	}
	n.Model.TouchPeer(req.From)
	n.send(req.From, pb.KindFingerprintOK, n.fingerprintReply())
}

// handleFingerprintReply exists so a datagram answer is accepted rather than
// counted as an unknown kind. The checker reads its answers off the stream, so
// nothing here has to correlate them.
func (n *Node) handleFingerprintReply(body []byte) {
	var reply pb.FingerprintReply
	if !n.unmarshal(body, &reply) {
		return
	}
	n.Model.TouchPeer(reply.From)
	log.Debugf("fingerprint from %s: %d keys, clock %d", reply.From, reply.Keys, reply.Clock)
}
