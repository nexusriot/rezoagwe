// Package node is the rezoagwe peer engine: membership, replication, chat and
// anti-entropy over an injectable transport.
//
// It deliberately knows nothing about tview. The TUI, the HTTP gateway and the
// multi-node tests are three front ends onto the same engine, which is what
// makes convergence testable without driving a terminal.
package node

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	"github.com/nexusriot/rezoagwe/pkg/metrics"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// activityRing bounds the in-memory replication activity feed. It is a live
// view, not history: it is neither persisted nor synced.
const activityRing = 300

// Events is how the engine tells a front end that something changed. Every
// method may be called from any goroutine, and must not block.
type Events interface {
	KVChanged()
	PeersChanged()
	ChatChanged()
	ActivityChanged()
}

// NopEvents ignores every notification, for headless nodes and tests.
type NopEvents struct{}

func (NopEvents) KVChanged()       {}
func (NopEvents) PeersChanged()    {}
func (NopEvents) ChatChanged()     {}
func (NopEvents) ActivityChanged() {}

// Config describes a node. Zero-valued durations and sizes take their defaults.
type Config struct {
	BootstrapAddrs []string
	NodeAddr       string
	// AdvertiseAddr is the address peers are told to reach this node at. Empty
	// means "whatever NodeAddr says", which is right on one machine and wrong
	// on a LAN: a node bound to ":3137" advertises ":3137", and every peer
	// resolves that to its own loopback.
	AdvertiseAddr string
	Nick          string
	DataPath      string
	PSK           string
	Cluster       string
	// Version is what the binary reports, carried so a diagnostics report says
	// which build produced it.
	Version string

	GossipInterval    time.Duration
	HeartbeatInterval time.Duration
	EvictThreshold    time.Duration
	SweepInterval     time.Duration
	// TombstoneTTL is how long a tombstone is retained before GC reclaims it.
	// Zero disables GC, which is the safe choice for a cluster that partitions
	// for longer than it.
	TombstoneTTL time.Duration

	// DigestBatch caps how many keys one anti-entropy digest advertises, and
	// MaxPush/MaxPull cap the repair traffic a single round can trigger.
	//
	// DigestBatch must not exceed either budget: the sender's cursor advances
	// by the whole digest, so a range that identified more repairs than one
	// round can carry has its tail stranded until the cursor wraps the entire
	// keyspace. applyDefaults clamps it rather than letting that happen
	// quietly.
	DigestBatch int
	MaxPush     int
	MaxPull     int

	// Limits bound what the store will hold. The zero value is unbounded.
	Limits model.Limits

	// Transport is injectable so tests can run a whole cluster in memory over a
	// lossy network. Nil means real UDP + TCP on NodeAddr.
	Transport transport.Transport
}

func (c *Config) applyDefaults() {
	if c.NodeAddr == "" {
		c.NodeAddr = ":3137"
	}
	if c.GossipInterval == 0 {
		c.GossipInterval = 10 * time.Second
	}
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = 5 * time.Second
	}
	if c.EvictThreshold == 0 {
		c.EvictThreshold = 15 * time.Second
	}
	if c.SweepInterval == 0 {
		c.SweepInterval = 5 * time.Second
	}
	if c.DigestBatch == 0 {
		c.DigestBatch = 128
	}
	if c.MaxPush == 0 {
		c.MaxPush = 128
	}
	if c.MaxPull == 0 {
		c.MaxPull = 128
	}
	if budget := min(c.MaxPush, c.MaxPull); c.DigestBatch > budget {
		log.Debugf("digest batch %d exceeds the repair budget %d; clamping",
			c.DigestBatch, budget)
		c.DigestBatch = budget
	}
	if c.Cluster == "" {
		c.Cluster = pb.DefaultCluster
	}
	if c.AdvertiseAddr == "" {
		c.AdvertiseAddr = c.NodeAddr
	}
}

// Node is a running peer.
type Node struct {
	cfg     Config
	Model   *model.Model
	Metrics *metrics.Metrics

	codec *pb.Codec
	tr    transport.Transport
	ev    Events

	startedAt time.Time

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup

	// One cursor per peer. A single shared cursor divided the keyspace among
	// whichever peers the random target picked, so each peer only ever heard
	// about its share of the ranges and a divergence with one peer waited on
	// rounds spent talking to the others.
	aeMu      sync.Mutex
	aeCursors map[string]string

	// strangers are source addresses that sent packets without being a peer —
	// the signature of a node whose advertised address is not the one its
	// packets come from (a NAT, or a wrong -advertise).
	strangerMu sync.Mutex
	strangers  map[string]int64

	actMu    sync.Mutex
	activity []string

	rndMu sync.Mutex
	rnd   *rand.Rand
}

// New builds a node. It does not touch the network until Start.
func New(cfg Config, ev Events) (*Node, error) {
	cfg.applyDefaults()
	if ev == nil {
		ev = NopEvents{}
	}
	tr := cfg.Transport
	if tr == nil {
		udp, err := transport.ListenUDP(cfg.NodeAddr)
		if err != nil {
			return nil, err
		}
		tr = udp
	}
	m := model.NewModel(model.Config{
		BootstrapAddrs: cfg.BootstrapAddrs,
		NodeAddr:       cfg.NodeAddr,
		AdvertiseAddr:  cfg.AdvertiseAddr,
		Nick:           cfg.Nick,
		DataPath:       cfg.DataPath,
	})
	m.Store.SetLimits(cfg.Limits)
	n := &Node{
		cfg:       cfg,
		Model:     m,
		Metrics:   metrics.New(),
		codec:     pb.NewCodec(cfg.PSK, cfg.Cluster),
		tr:        tr,
		ev:        ev,
		startedAt: time.Now(),
		stop:      make(chan struct{}),
		rnd:       rand.New(rand.NewSource(time.Now().UnixNano())),
		aeCursors: make(map[string]string),
		strangers: make(map[string]int64),
	}
	n.Metrics.SetGauges(func() metrics.Gauges {
		return metrics.Gauges{
			Peers:      len(n.Model.GetNodes()),
			Keys:       n.Model.Store.Len(),
			Tombstones: n.Model.Store.Tombstones(),
			ValueBytes: n.Model.Store.ValueBytes(),
			Clock:      n.Model.Store.Clock(),
			UptimeSec:  int64(n.Uptime().Seconds()),
		}
	})
	return n, nil
}

// Addr is the address this node advertises, which is what a peer needs and
// what every message body carries. BindAddr is the socket it actually listens
// on; the two differ whenever -advertise is set.
func (n *Node) Addr() string { return n.cfg.AdvertiseAddr }

// BindAddr is the address the transport listens on.
func (n *Node) BindAddr() string { return n.cfg.NodeAddr }

// Uptime is how long the node has been running.
func (n *Node) Uptime() time.Duration { return time.Since(n.startedAt) }

// Cluster is the cluster label this node authenticates with.
func (n *Node) Cluster() string { return n.cfg.Cluster }

// Start begins serving: it binds the loops but reaches out to nobody. Call Join
// to contact bootstrap and pull state — separating the two keeps "serving" and
// "joining" independently controllable, which is what lets a test wire a
// cluster up by hand.
func (n *Node) Start() {
	n.loop(n.packetLoop)
	n.loop(n.streamLoop)
	n.loop(n.gossipLoop)
	n.loop(n.heartbeatLoop)
	n.loop(n.evictLoop)
	n.loop(n.sweepLoop)
}

func (n *Node) loop(f func()) {
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		f()
	}()
}

// Stop announces departure, shuts the transport down and waits for the loops.
func (n *Node) Stop() {
	n.stopOnce.Do(func() {
		// Announce first: UDP writes only hand the datagram to the kernel, so
		// peers learn of the departure immediately instead of waiting out the
		// eviction timeout.
		n.broadcast(pb.KindGoodbye, pb.Goodbye{From: n.cfg.AdvertiseAddr, Nick: n.Model.Nick()})
		close(n.stop)
		n.tr.Close()
		n.wg.Wait()
		// Last, so nothing written during shutdown is lost to the debounce.
		n.Model.Close()
	})
}

// join announces this node to bootstrap and to any peer it already remembers.
func (n *Node) join() {
	n.registerWithBootstrap()
	n.discoverFromBootstrap()
	n.helloAllPeers()
	n.requestStateFromRandomPeer()
}

func (n *Node) send(addr string, kind pb.MessageKind, v interface{}) {
	pkt, err := n.codec.Encode(kind, v)
	if err != nil {
		log.Errorf("encode %s: %s", kind, err)
		return
	}
	n.sendRaw(addr, kind, pkt)
}

func (n *Node) sendRaw(addr string, kind pb.MessageKind, pkt []byte) {
	if err := n.tr.Send(addr, pkt); err != nil {
		n.Metrics.SendFailed(model.NormalizeAddr(addr), kind, err, time.Now().Unix())
		log.Debugf("send %s to %s: %s", kind, addr, err)
		return
	}
	n.Metrics.SentTo(model.NormalizeAddr(addr), kind, len(pkt))
}

// broadcast sends one encoded packet to every known peer. Encoding once means
// every peer sees the same nonce, which is harmless: the replay cache is
// per-receiver, and a receiver still rejects a genuine duplicate.
func (n *Node) broadcast(kind pb.MessageKind, v interface{}) {
	pkt, err := n.codec.Encode(kind, v)
	if err != nil {
		log.Errorf("encode %s: %s", kind, err)
		return
	}
	n.Model.Nodes.Range(func(key, _ interface{}) bool {
		n.sendRaw(key.(string), kind, pkt)
		return true
	})
}

// randomPeer returns a random known peer, or "" when the node is alone.
func (n *Node) randomPeer() string {
	peers := n.Model.GetNodes()
	if len(peers) == 0 {
		return ""
	}
	n.rndMu.Lock()
	i := n.rnd.Intn(len(peers))
	n.rndMu.Unlock()
	return peers[i]
}

func (n *Node) helloMessage() pb.Hello {
	return pb.Hello{From: n.cfg.AdvertiseAddr, Nick: n.Model.Nick(), ID: n.Model.NodeID}
}

func (n *Node) helloAllPeers() {
	n.broadcast(pb.KindHello, n.helloMessage())
}

func (n *Node) gossipLoop() {
	t := time.NewTicker(n.cfg.GossipInterval)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-t.C:
			peers := n.Model.GetNodes()
			if len(peers) == 0 {
				continue
			}
			target := n.randomPeer()
			n.send(target, pb.KindPeerGossip, pb.PeerGossip{
				From:  n.cfg.AdvertiseAddr,
				Nick:  n.Model.Nick(),
				ID:    n.Model.NodeID,
				Peers: peers,
			})
			// Piggyback reconciliation on the same tick: gossip already picked a
			// random peer, and the digest is what turns "eventually consistent"
			// from a hope into a mechanism.
			n.antiEntropyRound(target)
		}
	}
}

// heartbeatLoop keeps bootstrap and peers aware that we are still alive.
func (n *Node) heartbeatLoop() {
	t := time.NewTicker(n.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-t.C:
			n.registerWithBootstrap()
			// Alone? The initial join may have raced bootstrap's startup or lost
			// its packet — re-ask bootstrap.
			if len(n.Model.GetNodes()) == 0 {
				n.discoverFromBootstrap()
			}
			n.helloAllPeers()
		}
	}
}

// evictLoop drops peers from which no packet has arrived for a while.
func (n *Node) evictLoop() {
	t := time.NewTicker(n.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-t.C:
			evicted := n.Model.EvictStalePeers(n.cfg.EvictThreshold)
			for _, ep := range evicted {
				log.Debugf("evicted stale peer %s", ep.Addr)
				n.forgetPeerCursor(ep.Addr)
				n.announceLeave(ep.Addr, ep.Nick)
			}
			if len(evicted) > 0 {
				n.ev.PeersChanged()
			}
		}
	}
}

// sweepLoop applies TTL expiry and, if enabled, tombstone GC.
func (n *Node) sweepLoop() {
	t := time.NewTicker(n.cfg.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-t.C:
			if expired := n.Model.Store.SweepExpired(); expired > 0 {
				n.Metrics.KVExpired.Add(uint64(expired))
				n.logActivity("%d key(s) expired", expired)
				n.ev.KVChanged()
			}
			if n.cfg.TombstoneTTL > 0 {
				if gced := n.Model.Store.GCTombstones(n.cfg.TombstoneTTL); gced > 0 {
					n.Metrics.KVGCed.Add(uint64(gced))
					n.logActivity("%d tombstone(s) reclaimed", gced)
				}
			}
		}
	}
}

// logActivity records a line in the replication activity feed.
func (n *Node) logActivity(format string, args ...interface{}) {
	line := time.Now().Format("15:04:05") + " " + fmt.Sprintf(format, args...)
	n.actMu.Lock()
	n.activity = append(n.activity, line)
	if len(n.activity) > activityRing {
		n.activity = n.activity[len(n.activity)-activityRing:]
	}
	n.actMu.Unlock()
	n.ev.ActivityChanged()
}

// noteStranger records traffic that nothing ties to a known peer.
//
// from is the source address the transport saw; claimed is the address the body
// says it came from, empty when the packet never authenticated. A packet is
// only a stranger when *neither* identifies a peer: on any network where peers
// advertise a hostname the source address never matches one, so keying on it
// alone made every healthy peer a permanent anomaly.
func (n *Node) noteStranger(from, claimed string) {
	// This runs on every inbound packet, so both checks have to be map lookups
	// rather than scans.
	if claimed != "" && (n.Model.HasPeerNormalized(claimed) || n.Model.IsSelf(claimed)) {
		return
	}
	if from == "" || n.Model.HasPeerNormalized(from) || n.Model.IsSelf(from) {
		return
	}
	n.strangerMu.Lock()
	if n.strangers == nil {
		n.strangers = make(map[string]int64)
	}
	n.strangers[from] = time.Now().Unix()
	n.strangerMu.Unlock()
}

// Stranger is a source address that sent packets without being a peer.
type Stranger struct {
	Addr      string `json:"addr"`
	LastSeen  int64  `json:"last_seen"`
	PacketsIn uint64 `json:"packets_in"`
	Rejected  uint64 `json:"rejected"`
}

// Strangers lists the addresses seen sending packets that never became peers.
func (n *Node) Strangers() []Stranger {
	n.strangerMu.Lock()
	out := make([]Stranger, 0, len(n.strangers))
	for addr, at := range n.strangers {
		if n.Model.HasPeerNormalized(addr) {
			continue // it introduced itself since
		}
		s := Stranger{Addr: addr, LastSeen: at}
		if c, ok := n.Metrics.PeerCounters(addr); ok {
			s.PacketsIn = c.PacketsIn
			s.Rejected = c.Rejected
		}
		out = append(out, s)
	}
	n.strangerMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// Activity returns the replication activity feed, oldest first.
func (n *Node) Activity() []string {
	n.actMu.Lock()
	defer n.actMu.Unlock()
	out := make([]string, len(n.activity))
	copy(out, n.activity)
	return out
}
