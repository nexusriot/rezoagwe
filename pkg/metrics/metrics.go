// Package metrics counts what a replicating node does, so replication health
// is observable instead of inferred. Anti-entropy in particular is invisible
// when it works — the counters are how you tell it is running at all.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
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

	// Per-address counters are what turn "packets are being dropped" into
	// "this peer never answers": a global counter cannot tell one-way UDP
	// (a firewall) from ordinary loss.
	peerMu sync.Mutex
	peers  map[string]*peerCounters

	errMu     sync.Mutex
	lastError *SendError

	gaugeMu sync.Mutex
	gauges  func() Gauges
}

// peerCounters is the traffic seen for one address.
type peerCounters struct {
	packetsOut uint64
	packetsIn  uint64
	bytesOut   uint64
	bytesIn    uint64
	sendErrors uint64
	rejected   uint64
}

// SendError is the most recent transport failure, kept because the counter
// alone never says which peer stopped routing.
type SendError struct {
	Addr    string `json:"addr"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	At      int64  `json:"at"`
}

// Gauges are the levels a counter cannot express: how much is in the store
// right now, not how much has passed through it. The node supplies them,
// since the metrics package does not know what a store is.
type Gauges struct {
	Peers      int    `json:"peers"`
	Keys       int    `json:"keys"`
	Tombstones int    `json:"tombstones"`
	ValueBytes int    `json:"value_bytes"`
	Clock      uint64 `json:"clock"`
	UptimeSec  int64  `json:"uptime_sec"`
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

// SentTo records a datagram handed to the transport for one address.
func (m *Metrics) SentTo(addr string, kind pb.MessageKind, n int) {
	if m == nil {
		return
	}
	m.Sent(kind, n)
	c := m.counters(addr)
	if c == nil {
		return
	}
	m.peerMu.Lock()
	c.packetsOut++
	c.bytesOut += uint64(n)
	m.peerMu.Unlock()
}

// RecvFrom records an authenticated datagram observed from one source address.
func (m *Metrics) RecvFrom(addr string, kind pb.MessageKind, n int) {
	if m == nil {
		return
	}
	m.Recv(kind, n)
	c := m.counters(addr)
	if c == nil {
		return
	}
	m.peerMu.Lock()
	c.packetsIn++
	c.bytesIn += uint64(n)
	m.peerMu.Unlock()
}

// SendFailed records a transport failure against the address it was aimed at,
// and keeps the most recent one whole: "12 send errors" is not actionable,
// "no route to 10.0.0.4:3137" is.
func (m *Metrics) SendFailed(addr string, kind pb.MessageKind, err error, now int64) {
	if m == nil {
		return
	}
	m.SendErrors.Add(1)
	if c := m.counters(addr); c != nil {
		m.peerMu.Lock()
		c.sendErrors++
		m.peerMu.Unlock()
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	m.errMu.Lock()
	m.lastError = &SendError{Addr: addr, Kind: kind.String(), Message: msg, At: now}
	m.errMu.Unlock()
}

// RejectedFrom records a packet from addr that never authenticated, which is
// how a node with the wrong key is told apart from one that is merely quiet.
func (m *Metrics) RejectedFrom(addr string) {
	if m == nil {
		return
	}
	if c := m.counters(addr); c != nil {
		m.peerMu.Lock()
		c.rejected++
		m.peerMu.Unlock()
	}
}

// counters returns the per-address counters, creating them on first use.
func (m *Metrics) counters(addr string) *peerCounters {
	if addr == "" {
		return nil
	}
	m.peerMu.Lock()
	defer m.peerMu.Unlock()
	if m.peers == nil {
		m.peers = make(map[string]*peerCounters)
	}
	c, ok := m.peers[addr]
	if !ok {
		c = &peerCounters{}
		m.peers[addr] = c
	}
	return c
}

// SetGauges registers the source of the level readings. It is a callback
// rather than a set of fields so a snapshot always reports the store as it is
// now, not as it was when something last remembered to update a number.
func (m *Metrics) SetGauges(f func() Gauges) {
	if m == nil {
		return
	}
	m.gaugeMu.Lock()
	m.gauges = f
	m.gaugeMu.Unlock()
}

// PeerCount is the traffic seen for one address.
type PeerCount struct {
	Addr       string `json:"addr"`
	PacketsOut uint64 `json:"packets_out"`
	PacketsIn  uint64 `json:"packets_in"`
	BytesOut   uint64 `json:"bytes_out"`
	BytesIn    uint64 `json:"bytes_in"`
	SendErrors uint64 `json:"send_errors"`
	Rejected   uint64 `json:"rejected"`
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
	Peers []PeerCount
	// Gauges is the zero value unless a gauge source was registered.
	Gauges        Gauges
	LastSendError *SendError
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

	m.peerMu.Lock()
	for addr, c := range m.peers {
		s.Peers = append(s.Peers, PeerCount{
			Addr:       addr,
			PacketsOut: c.packetsOut,
			PacketsIn:  c.packetsIn,
			BytesOut:   c.bytesOut,
			BytesIn:    c.bytesIn,
			SendErrors: c.sendErrors,
			Rejected:   c.rejected,
		})
	}
	m.peerMu.Unlock()
	sort.Slice(s.Peers, func(i, j int) bool { return s.Peers[i].Addr < s.Peers[j].Addr })

	m.errMu.Lock()
	if m.lastError != nil {
		e := *m.lastError
		s.LastSendError = &e
	}
	m.errMu.Unlock()

	m.gaugeMu.Lock()
	f := m.gauges
	m.gaugeMu.Unlock()
	if f != nil {
		s.Gauges = f()
	}
	return s
}

// PeerCounters returns the per-address traffic for one address.
func (m *Metrics) PeerCounters(addr string) (PeerCount, bool) {
	m.peerMu.Lock()
	defer m.peerMu.Unlock()
	c, ok := m.peers[addr]
	if !ok {
		return PeerCount{}, false
	}
	return PeerCount{
		Addr:       addr,
		PacketsOut: c.packetsOut,
		PacketsIn:  c.packetsIn,
		BytesOut:   c.bytesOut,
		BytesIn:    c.bytesIn,
		SendErrors: c.sendErrors,
		Rejected:   c.rejected,
	}, true
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

	gauge := func(name, help string, v uint64) {
		fmt.Fprintf(&b, "# HELP rezoagwe_%s %s\n", name, help)
		fmt.Fprintf(&b, "# TYPE rezoagwe_%s gauge\n", name)
		fmt.Fprintf(&b, "rezoagwe_%s %d\n", name, v)
	}
	// Levels, not totals. These are what an alert is written against — "keys
	// stopped growing", "peers dropped to zero" — and a counter cannot say it.
	gauge("peers", "Peers currently known.", uint64(s.Gauges.Peers))
	gauge("keys", "Live keys currently held.", uint64(s.Gauges.Keys))
	gauge("tombstones", "Tombstones currently retained.", uint64(s.Gauges.Tombstones))
	gauge("value_bytes", "Bytes of live values currently held.", uint64(s.Gauges.ValueBytes))
	gauge("lamport_clock", "This node's Lamport counter.", s.Gauges.Clock)
	gauge("uptime_seconds", "Seconds since this node started.", uint64(s.Gauges.UptimeSec))

	b.WriteString("# HELP rezoagwe_messages_total Messages by kind and direction.\n")
	b.WriteString("# TYPE rezoagwe_messages_total counter\n")
	for _, k := range s.Kinds {
		fmt.Fprintf(&b, "rezoagwe_messages_total{kind=%q,direction=\"sent\"} %d\n", k.Kind, k.Sent)
		fmt.Fprintf(&b, "rezoagwe_messages_total{kind=%q,direction=\"received\"} %d\n", k.Kind, k.Recv)
	}

	if len(s.Peers) > 0 {
		peerCounter := func(name, help string, pick func(PeerCount) uint64) {
			fmt.Fprintf(&b, "# HELP rezoagwe_%s %s\n", name, help)
			fmt.Fprintf(&b, "# TYPE rezoagwe_%s counter\n", name)
			for _, p := range s.Peers {
				fmt.Fprintf(&b, "rezoagwe_%s{peer=%q} %d\n", name, p.Addr, pick(p))
			}
		}
		peerCounter("peer_packets_sent_total", "Datagrams sent, by peer.",
			func(p PeerCount) uint64 { return p.PacketsOut })
		peerCounter("peer_packets_received_total", "Datagrams received, by peer.",
			func(p PeerCount) uint64 { return p.PacketsIn })
		peerCounter("peer_bytes_sent_total", "Bytes sent, by peer.",
			func(p PeerCount) uint64 { return p.BytesOut })
		peerCounter("peer_bytes_received_total", "Bytes received, by peer.",
			func(p PeerCount) uint64 { return p.BytesIn })
		peerCounter("peer_send_errors_total", "Send failures, by peer.",
			func(p PeerCount) uint64 { return p.SendErrors })
		peerCounter("peer_rejected_total", "Packets that failed authentication, by source.",
			func(p PeerCount) uint64 { return p.Rejected })
	}
	return b.String()
}
