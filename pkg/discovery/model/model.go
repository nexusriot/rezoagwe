package model

import (
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// chatRing bounds the in-memory (and persisted) chat history.
const chatRing = 500

// flushInterval is how long a mutation waits for company before the state file
// is rewritten.
//
// Persistence used to be synchronous on every mutation, which meant one PUT
// serialised the whole store — and a 128-entry anti-entropy repair serialised
// it 128 times. Coalescing trades up to this much unflushed work on a kill -9
// for a write cost that no longer grows with the store: a clean shutdown
// flushes, and anything lost to a hard kill is what anti-entropy re-fetches
// from a peer anyway.
const flushInterval = 250 * time.Millisecond

// Config is everything a node needs to know about itself before it talks to
// anyone.
type Config struct {
	BootstrapAddrs []string
	NodeAddr       string
	// AdvertiseAddr is what peers are told to reach this node at, when that
	// differs from the bind address. Empty means "advertise the bind address".
	AdvertiseAddr string
	Nick          string
	DataPath      string // "" disables persistence
}

// Model owns a node's local state: its identity, its KV replica, who its peers
// are and what has been said in chat. It knows nothing about transports — the
// node engine does the talking.
type Model struct {
	Store    *KVStore
	Nodes    *sync.Map // address -> bool
	lastSeen sync.Map  // address -> time.Time (last packet observed)
	nicks    sync.Map  // address -> string (peer-reported nickname)
	views    sync.Map  // address -> PeerView (the peer list that peer last gossiped)
	// normalized is the peer set keyed by NormalizeAddr, so a lookup by the
	// address a datagram *came from* is O(1). Peers are stored under the
	// address they advertise, which is rarely the same string.
	normalized     sync.Map // NormalizeAddr(address) -> address
	ids            sync.Map // node id -> address (attributes a version to a peer)
	BootstrapAddrs []string
	NodeAddr       string
	AdvertiseAddr  string
	NodeID         string
	DataPath       string

	nickMu   sync.RWMutex
	nodeNick string

	chatMu  sync.Mutex
	chatLog []pb.ChatEntry

	persistMu  sync.Mutex
	persistGen uint64
	persister  *Persister

	// dirty has capacity 1: it is a "something changed" doorbell, not a queue.
	dirty     chan struct{}
	closeOnce sync.Once
	stopFlush chan struct{}
	flushDone chan struct{}
}

func NewModel(cfg Config) *Model {
	nick := cfg.Nick
	if nick == "" {
		nick = "anon"
	}
	advertise := cfg.AdvertiseAddr
	if advertise == "" {
		advertise = cfg.NodeAddr
	}
	m := &Model{
		BootstrapAddrs: cfg.BootstrapAddrs,
		NodeAddr:       cfg.NodeAddr,
		AdvertiseAddr:  advertise,
		nodeNick:       nick,
		DataPath:       cfg.DataPath,
		Nodes:          &sync.Map{},
	}

	var state PersistState
	var found bool
	if cfg.DataPath != "" {
		m.persister = NewPersister(cfg.DataPath)
		var err error
		state, found, err = m.persister.Load()
		if err != nil {
			log.Errorf("load persisted state from %s: %s", cfg.DataPath, err)
			found = false
		}
	}

	// Identity is persisted, not derived from the listen address: a node that
	// moves to a different port is still the same writer, and its version
	// tiebreak has to stay stable or old and new writes from it sort oddly
	// against each other.
	m.NodeID = state.NodeID
	if m.NodeID == "" {
		m.NodeID = uuid.New().String()
	}

	m.Store = NewKVStore(m.NodeID)
	if found {
		m.Store.LoadState(state)
		m.chatLog = append(m.chatLog, state.Chat...)
		log.Debugf("loaded %d entries (clock=%d) and %d chat lines from %s",
			len(state.Entries), state.Clock, len(state.Chat), cfg.DataPath)
	}
	if m.persister != nil {
		m.dirty = make(chan struct{}, 1)
		m.stopFlush = make(chan struct{})
		m.flushDone = make(chan struct{})
		go m.flushLoop()
		m.Store.SetOnChange(m.Persist)
		// A freshly generated identity has to reach disk even before the first
		// write, or a restart before any write would mint another one. This one
		// is synchronous: there may be no second write to coalesce it with.
		if !found || state.NodeID != m.NodeID {
			m.Flush()
		}
	}
	return m
}

// flushLoop rewrites the state file at most once per flushInterval, however
// many mutations arrived in between.
func (bn *Model) flushLoop() {
	defer close(bn.flushDone)
	timer := time.NewTimer(flushInterval)
	if !timer.Stop() {
		<-timer.C
	}
	pending := false
	for {
		select {
		case <-bn.stopFlush:
			return
		case <-bn.dirty:
			if !pending {
				pending = true
				timer.Reset(flushInterval)
			}

		case <-timer.C:
			pending = false
			bn.writeState()
		}
	}
}

// Close flushes anything outstanding and stops the background writer. It is
// safe to call more than once, and a Model with no persistence ignores it.
func (bn *Model) Close() {
	if bn.persister == nil {
		return
	}
	bn.closeOnce.Do(func() {
		close(bn.stopFlush)
		<-bn.flushDone
		// Unconditional: a doorbell sitting in `dirty` races the stop signal in
		// the loop's select, so "was anything pending?" is not answerable here.
		// One write on shutdown is cheaper than reasoning about that race.
		bn.writeState()
	})
}

// Flush writes the state file now and waits for it, which is what a clean
// shutdown and a test that restarts a node both need.
func (bn *Model) Flush() {
	if bn.persister == nil {
		return
	}
	bn.writeState()
}

// Nick returns this node's current nickname.
func (bn *Model) Nick() string {
	bn.nickMu.RLock()
	defer bn.nickMu.RUnlock()
	return bn.nodeNick
}

// SetOwnNick renames this node at runtime and reports the previous nickname.
func (bn *Model) SetOwnNick(nick string) string {
	bn.nickMu.Lock()
	defer bn.nickMu.Unlock()
	prev := bn.nodeNick
	bn.nodeNick = nick
	return prev
}

// Persist marks the state dirty. The write itself happens on the flush loop, so
// a burst of mutations — a repair batch, an import, a state sync — costs one
// serialisation of the store rather than one per entry.
func (bn *Model) Persist() {
	if bn.persister == nil {
		return
	}
	select {
	case bn.dirty <- struct{}{}:
	default: // already rung
	}
}

// writeState builds the snapshot and hands it to the persister.
//
// The generation is taken while the snapshot is built, under one lock, so a
// slow write can never land after a newer one: the persister drops any
// generation it has already passed.
func (bn *Model) writeState() {
	bn.persistMu.Lock()
	bn.persistGen++
	gen := bn.persistGen
	state := bn.Store.State()
	state.NodeID = bn.NodeID
	state.Chat = bn.ChatLog()
	bn.persistMu.Unlock()
	bn.persister.Save(gen, state)
}

func (bn *Model) GetNodes() []string {
	var res []string
	bn.Nodes.Range(func(key, value interface{}) bool {
		res = append(res, key.(string))
		return true
	})
	return res
}

// ValidPeerAddr reports whether addr is a usable host:port. Gossip is
// unauthenticated at the application level — anything a peer says about a third
// party is hearsay — so malformed entries are rejected at the door instead of
// lingering in the node list until eviction.
func ValidPeerAddr(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return false
	}
	// An empty host (":3137") is allowed: it is what a node bound to the
	// wildcard address advertises, and it resolves to the loopback interface —
	// which is exactly the local multi-node setup this PoC is usually run in.
	return !strings.ContainsAny(host, " \t\r\n")
}

// NormalizeAddr canonicalises an address for identity comparison, so a node
// listening on ":3137" recognises "127.0.0.1:3137" and "localhost:3137" as
// itself instead of gossiping itself into its own peer list. It is also what
// keeps the topology graph from drawing one peer as two.
func NormalizeAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "localhost", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// IsSelf reports whether addr refers to this node, under either the address it
// binds or the one it advertises. Recognising only the bind address would make
// a node with -advertise add itself as a peer the first time gossip echoed its
// own advertised address back at it.
func (bn *Model) IsSelf(addr string) bool {
	if addr == bn.NodeAddr || addr == bn.AdvertiseAddr {
		return true
	}
	norm := NormalizeAddr(addr)
	return norm == NormalizeAddr(bn.NodeAddr) || norm == NormalizeAddr(bn.AdvertiseAddr)
}

// AddPeer returns true if peer was newly learned.
func (bn *Model) AddPeer(addr string) bool {
	if addr == "" || bn.IsSelf(addr) || !ValidPeerAddr(addr) {
		return false
	}
	bn.lastSeen.Store(addr, time.Now())
	bn.normalized.Store(NormalizeAddr(addr), addr)
	_, loaded := bn.Nodes.LoadOrStore(addr, true)
	return !loaded
}

// HasPeerNormalized reports whether addr — in any spelling — is a known peer.
// It exists for the packet path, which sees the source address the transport
// observed rather than the one the peer advertises.
func (bn *Model) HasPeerNormalized(addr string) bool {
	if _, ok := bn.Nodes.Load(addr); ok {
		return true
	}
	_, ok := bn.normalized.Load(NormalizeAddr(addr))
	return ok
}

func (bn *Model) HasPeer(addr string) bool {
	_, ok := bn.Nodes.Load(addr)
	return ok
}

// TouchPeer records a fresh contact from addr without changing membership.
func (bn *Model) TouchPeer(addr string) {
	if addr == "" || bn.IsSelf(addr) {
		return
	}
	bn.lastSeen.Store(addr, time.Now())
}

// RemovePeer drops a peer entirely.
func (bn *Model) RemovePeer(addr string) {
	bn.Nodes.Delete(addr)
	bn.lastSeen.Delete(addr)
	bn.nicks.Delete(addr)
	bn.views.Delete(addr)
	bn.normalized.Delete(NormalizeAddr(addr))
	bn.ids.Range(func(key, value interface{}) bool {
		if value.(string) == addr {
			bn.ids.Delete(key)
		}
		return true
	})
}

// PeerView is the peer list one peer last gossiped, and when.
//
// Keeping it is what makes a cluster graph possible without a byte of extra
// protocol: gossip already carries each node's own view, and the difference
// between "both ends claim this link" and "only one does" is the whole
// diagnostic.
type PeerView struct {
	Peers []string
	At    time.Time
}

// RecordPeerView stores what a peer said its own peer list was.
func (bn *Model) RecordPeerView(from string, peers []string) {
	if from == "" {
		return
	}
	cp := make([]string, len(peers))
	copy(cp, peers)
	bn.views.Store(from, PeerView{Peers: cp, At: time.Now()})
}

// PeerViews returns every gossiped peer list this node has heard.
func (bn *Model) PeerViews() map[string]PeerView {
	out := make(map[string]PeerView)
	bn.views.Range(func(key, value interface{}) bool {
		out[key.(string)] = value.(PeerView)
		return true
	})
	return out
}

// SetNick records the nickname reported by a peer.
func (bn *Model) SetNick(addr, nick string) {
	if addr == "" || nick == "" {
		return
	}
	bn.nicks.Store(addr, nick)
}

// NickOf returns the last-known nick for addr, or "" if unknown.
func (bn *Model) NickOf(addr string) string {
	if v, ok := bn.nicks.Load(addr); ok {
		return v.(string)
	}
	return ""
}

// SetNodeID maps a peer's stable id to its address, so a version stamped with
// that id can be attributed to a nickname.
func (bn *Model) SetNodeID(id, addr string) {
	if id == "" || addr == "" {
		return
	}
	bn.ids.Store(id, addr)
}

// AddrOfID resolves a version's node id back to a peer address.
func (bn *Model) AddrOfID(id string) string {
	if v, ok := bn.ids.Load(id); ok {
		return v.(string)
	}
	return ""
}

// WriterName renders a version's origin as something a human can read: "you"
// for local writes, the peer's nickname when known, and the raw id otherwise.
func (bn *Model) WriterName(id string) string {
	switch {
	case id == "":
		return "?"
	case id == bn.NodeID:
		return "you"
	}
	addr := bn.AddrOfID(id)
	if addr == "" {
		return short(id)
	}
	if nick := bn.NickOf(addr); nick != "" {
		return nick
	}
	return addr
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// EvictedPeer is the (addr, last-known-nick) pair for a removed peer.
type EvictedPeer struct {
	Addr string
	Nick string
}

// EvictStalePeers removes peers whose last-seen time is older than threshold
// and returns the (addr, nick) of every removed peer.
func (bn *Model) EvictStalePeers(threshold time.Duration) []EvictedPeer {
	now := time.Now()
	var evicted []EvictedPeer
	bn.lastSeen.Range(func(key, value interface{}) bool {
		addr := key.(string)
		ts := value.(time.Time)
		if now.Sub(ts) > threshold {
			nick := bn.NickOf(addr)
			bn.RemovePeer(addr)
			evicted = append(evicted, EvictedPeer{Addr: addr, Nick: nick})
		}
		return true
	})
	return evicted
}

// LastSeen reports when a packet last arrived from addr.
func (bn *Model) LastSeen(addr string) (time.Time, bool) {
	if v, ok := bn.lastSeen.Load(addr); ok {
		return v.(time.Time), true
	}
	return time.Time{}, false
}

func (bn *Model) AppendChat(entry pb.ChatEntry) {
	bn.chatMu.Lock()
	bn.chatLog = append(bn.chatLog, entry)
	if len(bn.chatLog) > chatRing {
		bn.chatLog = bn.chatLog[len(bn.chatLog)-chatRing:]
	}
	bn.chatMu.Unlock()
	bn.Persist()
}

func (bn *Model) ChatLog() []pb.ChatEntry {
	bn.chatMu.Lock()
	defer bn.chatMu.Unlock()
	out := make([]pb.ChatEntry, len(bn.chatLog))
	copy(out, bn.chatLog)
	return out
}

// PrependChat inserts synced history ahead of the lines already in the log,
// keeping the newest. Used when a joining node receives a peer's chat history:
// the peer's lines come first, then whatever this node logged locally (its own
// "joined" observations) while the state sync was in flight.
func (bn *Model) PrependChat(history []pb.ChatEntry) {
	if len(history) == 0 {
		return
	}
	bn.chatMu.Lock()
	// A snapshot repeats history this node may already hold — a joiner that syncs
	// from two peers is handed the same conversation twice, and asking one peer
	// again is enough on its own. Appending it wholesale showed every line as many
	// times as it had been synced, so merge on the entry itself and put the result
	// back in time order rather than remote-then-local.
	seen := make(map[pb.ChatEntry]struct{}, len(bn.chatLog)+len(history))
	merged := make([]pb.ChatEntry, 0, len(bn.chatLog)+len(history))
	for _, e := range append(append([]pb.ChatEntry{}, bn.chatLog...), history...) {
		if _, dup := seen[e]; dup {
			continue
		}
		seen[e] = struct{}{}
		merged = append(merged, e)
	}
	if len(merged) == len(bn.chatLog) {
		bn.chatMu.Unlock()
		return
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].TS < merged[j].TS })
	if len(merged) > chatRing {
		merged = merged[len(merged)-chatRing:]
	}
	bn.chatLog = merged
	bn.chatMu.Unlock()
	bn.Persist()
}
