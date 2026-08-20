package model

import (
	"net"
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

// Config is everything a node needs to know about itself before it talks to
// anyone.
type Config struct {
	BootstrapAddrs []string
	NodeAddr       string
	Nick           string
	DataPath       string // "" disables persistence
}

// Model owns a node's local state: its identity, its KV replica, who its peers
// are and what has been said in chat. It knows nothing about transports — the
// node engine does the talking.
type Model struct {
	Store          *KVStore
	Nodes          *sync.Map // address -> bool
	lastSeen       sync.Map  // address -> time.Time (last packet observed)
	nicks          sync.Map  // address -> string (peer-reported nickname)
	ids            sync.Map  // node id -> address (attributes a version to a peer)
	BootstrapAddrs []string
	NodeAddr       string
	NodeID         string
	DataPath       string

	nickMu   sync.RWMutex
	nodeNick string

	chatMu  sync.Mutex
	chatLog []pb.ChatEntry

	persistMu  sync.Mutex
	persistGen uint64
	persister  *Persister
}

func NewModel(cfg Config) *Model {
	nick := cfg.Nick
	if nick == "" {
		nick = "anon"
	}
	m := &Model{
		BootstrapAddrs: cfg.BootstrapAddrs,
		NodeAddr:       cfg.NodeAddr,
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
		m.Store.SetOnChange(m.Persist)
		// A freshly generated identity has to reach disk even before the first
		// write, or a restart before any write would mint another one.
		if !found || state.NodeID != m.NodeID {
			m.Persist()
		}
	}
	return m
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

// Persist writes the full node state (identity, KV, chat) to disk.
//
// The generation is taken while the snapshot is built, under one lock, so a
// slow write can never land after a newer one: the persister drops any
// generation it has already passed.
func (bn *Model) Persist() {
	if bn.persister == nil {
		return
	}
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

// normalizeAddr canonicalises an address for identity comparison, so a node
// listening on ":3137" recognises "127.0.0.1:3137" and "localhost:3137" as
// itself instead of gossiping itself into its own peer list.
func normalizeAddr(addr string) string {
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

// IsSelf reports whether addr refers to this node.
func (bn *Model) IsSelf(addr string) bool {
	return addr == bn.NodeAddr || normalizeAddr(addr) == normalizeAddr(bn.NodeAddr)
}

// AddPeer returns true if peer was newly learned.
func (bn *Model) AddPeer(addr string) bool {
	if addr == "" || bn.IsSelf(addr) || !ValidPeerAddr(addr) {
		return false
	}
	bn.lastSeen.Store(addr, time.Now())
	_, loaded := bn.Nodes.LoadOrStore(addr, true)
	return !loaded
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
	bn.ids.Range(func(key, value interface{}) bool {
		if value.(string) == addr {
			bn.ids.Delete(key)
		}
		return true
	})
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
	merged := make([]pb.ChatEntry, 0, len(history)+len(bn.chatLog))
	merged = append(merged, history...)
	merged = append(merged, bn.chatLog...)
	if len(merged) > chatRing {
		merged = merged[len(merged)-chatRing:]
	}
	bn.chatLog = merged
	bn.chatMu.Unlock()
	bn.Persist()
}
