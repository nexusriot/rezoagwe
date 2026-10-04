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
	// Differences names the keys behind those buckets, sorted, at most
	// MaxReportedDifferences of them. Empty against a peer too old to be
	// asked, which is why the buckets above are still reported.
	Differences []KeyDifference `json:"differences,omitempty"`
	// MoreDifferences is how many keys were left out of Differences.
	MoreDifferences int `json:"more_differences,omitempty"`
}

// MaxReportedDifferences bounds the named keys per peer. A report is something
// a person reads: two replicas that share nothing would otherwise print the
// whole keyspace, and the first fifty names say as much as the first fifty
// thousand.
const MaxReportedDifferences = 50

// KeyDifference is one key two replicas do not hold the same way.
//
// Either side may be empty, and that is the interesting case: a key present
// here and absent there is a write that never arrived, which reads very
// differently from the same key at two versions.
type KeyDifference struct {
	Key string `json:"key"`
	// Local and Remote are versions formatted "counter.node", or empty where
	// that replica does not hold the key at all.
	Local  string `json:"local,omitempty"`
	Remote string `json:"remote,omitempty"`
	// LocalDeleted and RemoteDeleted mark a side holding a tombstone rather
	// than a value: same key, same disagreement, different fix.
	LocalDeleted  bool `json:"local_deleted,omitempty"`
	RemoteDeleted bool `json:"remote_deleted,omitempty"`
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
	if !out.Agrees {
		// A second round, now that the digests have said where to look. The
		// first round is the cheap one every peer answers; this one ships a
		// key list for a few sixteenths of the keyspace, and only to the peers
		// that actually disagree.
		out.Differences, out.MoreDifferences = n.namedDifferences(addr, out.DifferingBuckets)
	}
	return out
}

// namedDifferences asks a peer what it holds in the buckets that disagreed and
// diffs it against what this node holds there.
//
// A peer too old to understand the request answers with no entries, and the
// report falls back to the bucket indices rather than failing: "they differ in
// bucket 7" is worse than a key name but much better than an error.
func (n *Node) namedDifferences(addr string, buckets []int) ([]KeyDifference, int) {
	body, err := n.requestOverStream(addr, pb.KindFingerprint,
		pb.Fingerprint{From: n.cfg.AdvertiseAddr, Buckets: buckets})
	if err != nil {
		return nil, 0
	}
	var reply pb.FingerprintReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, 0
	}

	remote := make(map[string]pb.KeyVersion, len(reply.Entries))
	for _, e := range reply.Entries {
		remote[e.Key] = e
	}
	local := n.Model.Store.BucketEntries(pb.FingerprintBuckets, buckets)
	seen := make(map[string]bool, len(local))

	diffs := make([]KeyDifference, 0, 8)
	for _, l := range local {
		seen[l.Key] = true
		r, ok := remote[l.Key]
		if ok && r.Version.Equal(l.Version) && r.Deleted == l.Deleted {
			continue
		}
		d := KeyDifference{Key: l.Key, Local: formatVersion(l.Version), LocalDeleted: l.Deleted}
		if ok {
			d.Remote = formatVersion(r.Version)
			d.RemoteDeleted = r.Deleted
		}
		diffs = append(diffs, d)
	}
	// Keys the peer holds and this node has never seen at all: the direction
	// a local-only walk cannot find, and the one that means a write never
	// arrived here.
	for _, r := range reply.Entries {
		if seen[r.Key] {
			continue
		}
		diffs = append(diffs, KeyDifference{
			Key: r.Key, Remote: formatVersion(r.Version), RemoteDeleted: r.Deleted,
		})
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Key < diffs[j].Key })

	if len(diffs) > MaxReportedDifferences {
		return diffs[:MaxReportedDifferences], len(diffs) - MaxReportedDifferences
	}
	return diffs, 0
}

// formatVersion renders a version the way the gateway's ETags do, so a name in
// a report can be pasted into an If-Match.
func formatVersion(v pb.Version) string {
	return fmt.Sprintf("%d.%s", v.Counter, v.Node)
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

	pkt, err := n.codec.Encode(kind, v)
	if err != nil {
		return nil, err
	}
	if err := transport.WriteFrameTo(conn, consistencyTimeout, pkt); err != nil {
		n.Metrics.StreamErrors.Add(1)
		return nil, err
	}
	n.Metrics.Sent(kind, len(pkt))

	// A reply naming the keys in several buckets is bounded but not small.
	frame, err := transport.ReadFrameFrom(conn, consistencyTimeout)
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
	if respKind != pb.KindFingerprintReply {
		return nil, fmt.Errorf("peer answered with %s", respKind)
	}
	return body, nil
}

// fingerprintReply builds this node's summary, labelled so a report can name
// the peer that produced it. A request naming buckets also gets the keys held
// in them, which is what lets the checker turn a bucket index into a name.
func (n *Node) fingerprintReply(buckets ...int) pb.FingerprintReply {
	r := n.Model.Store.Fingerprint(pb.FingerprintBuckets)
	r.From = n.cfg.AdvertiseAddr
	r.Nick = n.Model.Nick()
	if len(buckets) > 0 {
		r.Entries = n.Model.Store.BucketEntries(pb.FingerprintBuckets, buckets)
	}
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
	// The datagram path answers digests only: a key listing has no bound that
	// fits a packet, and the checker uses the stream path anyway.
	n.send(req.From, pb.KindFingerprintReply, n.fingerprintReply())
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
