// Package metrics counts what a replicating node does, so replication health
// is observable instead of inferred. Anti-entropy in particular is invisible
// when it works — the counters are how you tell it is running at all.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// Metrics is a set of lock-free counters. The zero value is ready to use.
type Metrics struct {
	PacketsSent atomic.Uint64
	PacketsRecv atomic.Uint64
	BytesSent   atomic.Uint64
	BytesRecv   atomic.Uint64
	SendErrors  atomic.Uint64

	AuthFailures   atomic.Uint64
	ReplayDrops    atomic.Uint64
	SkewDrops      atomic.Uint64
	MalformedDrops atomic.Uint64

	KVApplied       atomic.Uint64
	KVRejectedStale atomic.Uint64
	KVLocalWrites   atomic.Uint64
	KVCASFailures   atomic.Uint64
	KVExpired       atomic.Uint64
	KVGCed          atomic.Uint64

	AERounds atomic.Uint64
	AEPushed atomic.Uint64
	AEPulled atomic.Uint64

	StateSyncOut atomic.Uint64
	StateSyncIn  atomic.Uint64
	StreamErrors atomic.Uint64

	HTTPRequests atomic.Uint64

	sentByKind [256]atomic.Uint64
	recvByKind [256]atomic.Uint64
}

func New() *Metrics { return &Metrics{} }

func (m *Metrics) Sent(kind pb.MessageKind, n int) {
	if m == nil {
		return
	}
	m.PacketsSent.Add(1)
	m.BytesSent.Add(uint64(n))
	m.sentByKind[byte(kind)].Add(1)
}

func (m *Metrics) Recv(kind pb.MessageKind, n int) {
	if m == nil {
		return
	}
	m.PacketsRecv.Add(1)
	m.BytesRecv.Add(uint64(n))
	m.recvByKind[byte(kind)].Add(1)
}

// KindCount is one per-message-kind counter pair.
type KindCount struct {
	Kind string
	Sent uint64
	Recv uint64
}

// Snapshot is a consistent-enough read of every counter for rendering. The
// counters are not read atomically as a group: this is a monitoring view, not
// an accounting ledger.
type Snapshot struct {
	PacketsSent uint64
	PacketsRecv uint64
	BytesSent   uint64
	BytesRecv   uint64
	SendErrors  uint64

	AuthFailures   uint64
	ReplayDrops    uint64
	SkewDrops      uint64
	MalformedDrops uint64

	KVApplied       uint64
	KVRejectedStale uint64
	KVLocalWrites   uint64
	KVCASFailures   uint64
	KVExpired       uint64
	KVGCed          uint64

	AERounds uint64
	AEPushed uint64
	AEPulled uint64

	StateSyncOut uint64
	StateSyncIn  uint64
	StreamErrors uint64

	HTTPRequests uint64

	Kinds []KindCount
}

func (m *Metrics) Snapshot() Snapshot {
	s := Snapshot{
		PacketsSent:     m.PacketsSent.Load(),
		PacketsRecv:     m.PacketsRecv.Load(),
		BytesSent:       m.BytesSent.Load(),
		BytesRecv:       m.BytesRecv.Load(),
		SendErrors:      m.SendErrors.Load(),
		AuthFailures:    m.AuthFailures.Load(),
		ReplayDrops:     m.ReplayDrops.Load(),
		SkewDrops:       m.SkewDrops.Load(),
		MalformedDrops:  m.MalformedDrops.Load(),
		KVApplied:       m.KVApplied.Load(),
		KVRejectedStale: m.KVRejectedStale.Load(),
		KVLocalWrites:   m.KVLocalWrites.Load(),
		KVCASFailures:   m.KVCASFailures.Load(),
		KVExpired:       m.KVExpired.Load(),
		KVGCed:          m.KVGCed.Load(),
		AERounds:        m.AERounds.Load(),
		AEPushed:        m.AEPushed.Load(),
		AEPulled:        m.AEPulled.Load(),
		StateSyncOut:    m.StateSyncOut.Load(),
		StateSyncIn:     m.StateSyncIn.Load(),
		StreamErrors:    m.StreamErrors.Load(),
		HTTPRequests:    m.HTTPRequests.Load(),
	}
	for i := 0; i < 256; i++ {
		sent := m.sentByKind[i].Load()
		recv := m.recvByKind[i].Load()
		if sent == 0 && recv == 0 {
			continue
		}
		s.Kinds = append(s.Kinds, KindCount{
			Kind: pb.MessageKind(i).String(),
			Sent: sent,
			Recv: recv,
		})
	}
	sort.Slice(s.Kinds, func(i, j int) bool { return s.Kinds[i].Kind < s.Kinds[j].Kind })
	return s
}

// Prometheus renders the snapshot in the Prometheus text exposition format.
func (s Snapshot) Prometheus() string {
	var b strings.Builder
	counter := func(name, help string, v uint64) {
		fmt.Fprintf(&b, "# HELP rezoagwe_%s %s\n", name, help)
		fmt.Fprintf(&b, "# TYPE rezoagwe_%s counter\n", name)
		fmt.Fprintf(&b, "rezoagwe_%s %d\n", name, v)
	}
	counter("packets_sent_total", "Datagrams handed to the transport.", s.PacketsSent)
	counter("packets_received_total", "Datagrams accepted after authentication.", s.PacketsRecv)
	counter("bytes_sent_total", "Bytes handed to the transport.", s.BytesSent)
	counter("bytes_received_total", "Bytes accepted after authentication.", s.BytesRecv)
	counter("send_errors_total", "Transport send failures.", s.SendErrors)
	counter("auth_failures_total", "Packets dropped with a bad MAC.", s.AuthFailures)
	counter("replay_drops_total", "Packets dropped as replays.", s.ReplayDrops)
	counter("skew_drops_total", "Packets dropped for timestamp skew.", s.SkewDrops)
	counter("malformed_drops_total", "Packets dropped as unparseable.", s.MalformedDrops)
	counter("kv_applied_total", "Remote updates merged into the store.", s.KVApplied)
	counter("kv_rejected_stale_total", "Remote updates rejected as not newer.", s.KVRejectedStale)
	counter("kv_local_writes_total", "Local writes and deletes.", s.KVLocalWrites)
	counter("kv_cas_failures_total", "Compare-and-swap writes rejected.", s.KVCASFailures)
	counter("kv_expired_total", "Keys tombstoned by TTL expiry.", s.KVExpired)
	counter("kv_gc_total", "Tombstones reclaimed.", s.KVGCed)
	counter("anti_entropy_rounds_total", "Digests sent.", s.AERounds)
	counter("anti_entropy_pushed_total", "Entries pushed to a lagging peer.", s.AEPushed)
	counter("anti_entropy_pulled_total", "Entries requested from a peer.", s.AEPulled)
	counter("state_sync_out_total", "Snapshots served.", s.StateSyncOut)
	counter("state_sync_in_total", "Snapshots received.", s.StateSyncIn)
	counter("stream_errors_total", "Stream (TCP) failures.", s.StreamErrors)
	counter("http_requests_total", "HTTP gateway requests.", s.HTTPRequests)

	b.WriteString("# HELP rezoagwe_messages_total Messages by kind and direction.\n")
	b.WriteString("# TYPE rezoagwe_messages_total counter\n")
	for _, k := range s.Kinds {
		fmt.Fprintf(&b, "rezoagwe_messages_total{kind=%q,direction=\"sent\"} %d\n", k.Kind, k.Sent)
		fmt.Fprintf(&b, "rezoagwe_messages_total{kind=%q,direction=\"received\"} %d\n", k.Kind, k.Recv)
	}
	return b.String()
}
