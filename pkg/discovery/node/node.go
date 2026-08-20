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
	Nick           string
	DataPath       string
	PSK            string
	Cluster        string

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
	DigestBatch int
	MaxPush     int
	MaxPull     int

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
		c.DigestBatch = 256
	}
	if c.MaxPush == 0 {
		c.MaxPush = 128
	}
	if c.MaxPull == 0 {
		c.MaxPull = 128
	}
	if c.Cluster == "" {
		c.Cluster = pb.DefaultCluster
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

	aeMu     sync.Mutex
	aeCursor string

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
		Nick:           cfg.Nick,
		DataPath:       cfg.DataPath,
	})
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
	}
	return n, nil
}

// Addr is the address this node advertises.
func (n *Node) Addr() string { return n.cfg.NodeAddr }

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
		n.broadcast(pb.KindGoodbye, pb.Goodbye{From: n.cfg.NodeAddr, Nick: n.Model.Nick()})
		close(n.stop)
		n.tr.Close()
		n.wg.Wait()
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
		n.Metrics.SendErrors.Add(1)
		log.Debugf("send %s to %s: %s", kind, addr, err)
		return
	}
	n.Metrics.Sent(kind, len(pkt))
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
	return pb.Hello{From: n.cfg.NodeAddr, Nick: n.Model.Nick(), ID: n.Model.NodeID}
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
				From:  n.cfg.NodeAddr,
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

// Activity returns the replication activity feed, oldest first.
func (n *Node) Activity() []string {
	n.actMu.Lock()
	defer n.actMu.Unlock()
	out := make([]string, len(n.activity))
	copy(out, n.activity)
	return out
}
