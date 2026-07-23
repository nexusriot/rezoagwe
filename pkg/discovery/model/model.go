package model

import (
	"net"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// defaultDiscoverTimeout bounds the bootstrap DISCOVER reply read so an
// unreachable bootstrap — or a single lost UDP packet — degrades to "no peers
// learned" instead of hanging node startup forever.
const defaultDiscoverTimeout = 2 * time.Second

// kvEntry is a versioned value. Deleted keys are kept as tombstones (with the
// version of the delete) so a stale set can't resurrect a concurrently-deleted
// key.
type kvEntry struct {
	Value   string     `json:"value"`
	Version pb.Version `json:"version"`
	Deleted bool       `json:"deleted,omitempty"`
}

// PersistState is the on-disk form of the store: the Lamport clock plus every
// entry (tombstones included), so versions survive a restart and this node's
// future writes keep sorting after everything it had already seen.
type PersistState struct {
	Clock   uint64             `json:"clock"`
	Entries map[string]kvEntry `json:"entries"`
}

// KVStore is a version-aware KV store with last-write-wins merge. Every local
// mutation stamps a per-key pb.Version (a Lamport counter with this node's id
// as tiebreak); an incoming update is applied only when its version is newer
// than the one held.
type KVStore struct {
	mu       sync.RWMutex
	node     string // this node's id, used as the version tiebreaker
	clock    uint64 // Lamport counter
	store    map[string]kvEntry
	gen      uint64
	onChange func(gen uint64, state PersistState)
}

func NewKVStore(node string) *KVStore {
	return &KVStore{
		node:  node,
		store: make(map[string]kvEntry),
	}
}

// SetOnChange registers a callback invoked (outside the lock) after every
// mutation, carrying a monotonically increasing generation and the full
// persistable state. The generation lets a persister ignore an out-of-order
// write so the on-disk copy never goes backwards.
func (kv *KVStore) SetOnChange(f func(gen uint64, state PersistState)) {
	kv.mu.Lock()
	kv.onChange = f
	kv.mu.Unlock()
}

// loadState installs a persisted state without firing onChange (used at
// startup, before the persister is wired).
func (kv *KVStore) loadState(state PersistState) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.clock = state.Clock
	kv.store = make(map[string]kvEntry, len(state.Entries))
	for k, e := range state.Entries {
		kv.store[k] = e
	}
}

// mutated bumps the generation and returns what onChange needs. Must be called
// while holding the write lock; the callback runs after the lock is released
// so disk I/O never blocks other ops.
func (kv *KVStore) mutated() (uint64, PersistState, func(uint64, PersistState)) {
	kv.gen++
	if kv.onChange == nil {
		return 0, PersistState{}, nil
	}
	return kv.gen, kv.stateLocked(), kv.onChange
}

func (kv *KVStore) stateLocked() PersistState {
	entries := make(map[string]kvEntry, len(kv.store))
	for k, e := range kv.store {
		entries[k] = e
	}
	return PersistState{Clock: kv.clock, Entries: entries}
}

// Set records a local write and returns the versioned update to replicate.
func (kv *KVStore) Set(key, value string) pb.KVUpdate {
	kv.mu.Lock()
	kv.clock++
	ver := pb.Version{Counter: kv.clock, Node: kv.node}
	kv.store[key] = kvEntry{Value: value, Version: ver}
	gen, state, cb := kv.mutated()
	kv.mu.Unlock()
	if cb != nil {
		cb(gen, state)
	}
	return pb.KVUpdate{Action: pb.KVSet, Key: key, Value: value, Version: ver}
}

// Delete records a local tombstone and returns the versioned update to
// replicate.
func (kv *KVStore) Delete(key string) pb.KVUpdate {
	kv.mu.Lock()
	kv.clock++
	ver := pb.Version{Counter: kv.clock, Node: kv.node}
	kv.store[key] = kvEntry{Version: ver, Deleted: true}
	gen, state, cb := kv.mutated()
	kv.mu.Unlock()
	if cb != nil {
		cb(gen, state)
	}
	return pb.KVUpdate{Action: pb.KVDelete, Key: key, Version: ver}
}

// Apply merges a remote update under last-write-wins and reports whether it
// changed local state. The Lamport clock is advanced past any counter seen so
// this node's later writes sort after it.
func (kv *KVStore) Apply(u pb.KVUpdate) bool {
	kv.mu.Lock()
	if u.Version.Counter > kv.clock {
		kv.clock = u.Version.Counter
	}
	if cur, ok := kv.store[u.Key]; ok && !u.Version.Newer(cur.Version) {
		kv.mu.Unlock()
		return false
	}
	kv.store[u.Key] = kvEntry{
		Value:   u.Value,
		Version: u.Version,
		Deleted: u.Action == pb.KVDelete,
	}
	gen, state, cb := kv.mutated()
	kv.mu.Unlock()
	if cb != nil {
		cb(gen, state)
	}
	return true
}

func (kv *KVStore) Get(key string) (string, bool) {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	e, ok := kv.store[key]
	if !ok || e.Deleted {
		return "", false
	}
	return e.Value, true
}

// Snapshot returns a copy of the live (non-tombstoned) values for UI iteration.
func (kv *KVStore) Snapshot() map[string]string {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	out := make(map[string]string, len(kv.store))
	for k, e := range kv.store {
		if !e.Deleted {
			out[k] = e.Value
		}
	}
	return out
}

// Updates returns every entry (tombstones included) as versioned updates, for
// shipping a full snapshot to a joining peer to merge.
func (kv *KVStore) Updates() []pb.KVUpdate {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	out := make([]pb.KVUpdate, 0, len(kv.store))
	for k, e := range kv.store {
		action := pb.KVSet
		if e.Deleted {
			action = pb.KVDelete
		}
		out = append(out, pb.KVUpdate{Action: action, Key: k, Value: e.Value, Version: e.Version})
	}
	return out
}

type Model struct {
	Store         *KVStore
	Nodes         *sync.Map // address -> bool
	lastSeen      sync.Map  // address -> time.Time (last packet observed)
	nicks         sync.Map  // address -> string (peer-reported nickname)
	BootstrapAddr string
	NodeAddr      string
	NodeUUID      string
	NodeNick      string
	DataPath      string // KV persistence file; "" disables persistence

	// DiscoverTimeout bounds the bootstrap roster read; 0 means the default.
	DiscoverTimeout time.Duration

	chatMu  sync.Mutex
	chatLog []string
}

func NewModel(bootstrapAddr, nodeAddr, nick, dataPath string) *Model {
	if nick == "" {
		nick = "anon"
	}
	// The node address is the version tiebreaker: stable across restarts and
	// unique per node in the cluster.
	store := NewKVStore(nodeAddr)
	m := &Model{
		NodeUUID:        uuid.New().String(),
		BootstrapAddr:   bootstrapAddr,
		NodeAddr:        nodeAddr,
		NodeNick:        nick,
		DataPath:        dataPath,
		DiscoverTimeout: defaultDiscoverTimeout,
		Store:           store,
		Nodes:           &sync.Map{},
	}
	if dataPath != "" {
		p := NewPersister(dataPath)
		if state, found, err := p.Load(); err != nil {
			log.Errorf("load persisted store from %s: %s", dataPath, err)
		} else if found {
			// onChange is not wired yet, so this reload does not re-persist.
			store.loadState(state)
			log.Debugf("loaded %d entries (clock=%d) from %s", len(state.Entries), state.Clock, dataPath)
		}
		store.SetOnChange(p.Save)
	}
	return m
}

func (bn *Model) RegisterNode() {
	conn, err := net.Dial("udp", bn.BootstrapAddr)
	if err != nil {
		log.Errorf("Error connecting to bootstrap node: %s", err)
		return
	}
	defer conn.Close()
	msg := pb.BootstrapMessage{
		Action: pb.BootstrapAction_REGISTER,
		Host:   &pb.Host{Host: bn.NodeAddr},
	}
	toSend, err := proto.Marshal(&msg)
	if err != nil {
		log.Errorf("Error marshalling REGISTER message: %s", err)
		return
	}
	if _, err = conn.Write(toSend); err != nil {
		log.Errorf("Error sending REGISTER message: %s", err)
	}
}

func (bn *Model) GetNodes() []string {
	var res []string
	bn.Nodes.Range(func(key, value interface{}) bool {
		res = append(res, key.(string))
		return true
	})
	return res
}

// AddPeer returns true if peer was newly learned.
func (bn *Model) AddPeer(addr string) bool {
	if addr == "" || addr == bn.NodeAddr {
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
	if addr == "" || addr == bn.NodeAddr {
		return
	}
	bn.lastSeen.Store(addr, time.Now())
}

// RemovePeer drops a peer entirely.
func (bn *Model) RemovePeer(addr string) {
	bn.Nodes.Delete(addr)
	bn.lastSeen.Delete(addr)
	bn.nicks.Delete(addr)
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
			bn.Nodes.Delete(addr)
			bn.lastSeen.Delete(addr)
			bn.nicks.Delete(addr)
			evicted = append(evicted, EvictedPeer{Addr: addr, Nick: nick})
		}
		return true
	})
	return evicted
}

func (bn *Model) DiscoverNodes() []string {
	conn, err := net.Dial("udp", bn.BootstrapAddr)
	if err != nil {
		log.Errorf("Error connecting to bootstrap node: %s", err)
		return nil
	}
	defer conn.Close()

	dm := pb.BootstrapMessage{Action: pb.BootstrapAction_DISCOVER}
	data, err := proto.Marshal(&dm)
	if err != nil {
		log.Errorf("Error marshalling DISCOVER message: %s", err)
		return nil
	}
	if _, err = conn.Write(data); err != nil {
		log.Errorf("Error sending DISCOVER message: %s", err)
		return nil
	}

	timeout := bn.DiscoverTimeout
	if timeout <= 0 {
		timeout = defaultDiscoverTimeout
	}
	if err = conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		log.Errorf("Error setting read deadline: %s", err)
		return nil
	}

	// Larger than the bootstrap side needs today, so a big roster isn't
	// silently truncated.
	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		// Timeout or refused: no roster this attempt. Startup proceeds and the
		// heartbeat loop retries while we remain alone.
		log.Errorf("Error reading roster from bootstrap: %s", err)
		return nil
	}

	response := strings.TrimSpace(string(buf[:n]))
	if response == "" {
		return nil
	}
	return strings.Split(response, ",")
}

func (bn *Model) AppendChat(line string) {
	bn.chatMu.Lock()
	defer bn.chatMu.Unlock()
	bn.chatLog = append(bn.chatLog, line)
	if len(bn.chatLog) > 500 {
		bn.chatLog = bn.chatLog[len(bn.chatLog)-500:]
	}
}

func (bn *Model) ChatLog() []string {
	bn.chatMu.Lock()
	defer bn.chatMu.Unlock()
	out := make([]string, len(bn.chatLog))
	copy(out, bn.chatLog)
	return out
}

// PrependChat inserts synced history ahead of the lines already in the log,
// keeping the newest 500. Used when a joining node receives a peer's chat
// history: the peer's lines come first, then whatever this node logged locally
// (its own "joined" observations) while the state sync was in flight.
func (bn *Model) PrependChat(history []string) {
	if len(history) == 0 {
		return
	}
	bn.chatMu.Lock()
	defer bn.chatMu.Unlock()
	merged := make([]string, 0, len(history)+len(bn.chatLog))
	merged = append(merged, history...)
	merged = append(merged, bn.chatLog...)
	if len(merged) > 500 {
		merged = merged[len(merged)-500:]
	}
	bn.chatLog = merged
}
