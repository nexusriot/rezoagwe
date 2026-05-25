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

type KVStore struct {
	mu    sync.RWMutex
	store map[string]string
}

func NewKVStore() *KVStore {
	return &KVStore{
		store: make(map[string]string),
	}
}

func (kv *KVStore) Set(key, value string) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.store[key] = value
}

func (kv *KVStore) Get(key string) (string, bool) {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	val, ok := kv.store[key]
	return val, ok
}

func (kv *KVStore) Delete(key string) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	delete(kv.store, key)
}

// Snapshot returns a copy of the store for safe iteration outside the lock.
func (kv *KVStore) Snapshot() map[string]string {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	out := make(map[string]string, len(kv.store))
	for k, v := range kv.store {
		out[k] = v
	}
	return out
}

// Replace overwrites the store with the given snapshot.
func (kv *KVStore) Replace(snapshot map[string]string) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.store = make(map[string]string, len(snapshot))
	for k, v := range snapshot {
		kv.store[k] = v
	}
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

	chatMu  sync.Mutex
	chatLog []string
}

func NewModel(bootstrapAddr, nodeAddr, nick string) *Model {
	if nick == "" {
		nick = "anon"
	}
	return &Model{
		NodeUUID:      uuid.New().String(),
		BootstrapAddr: bootstrapAddr,
		NodeAddr:      nodeAddr,
		NodeNick:      nick,
		Store:         NewKVStore(),
		Nodes:         &sync.Map{},
	}
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

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		log.Errorf("Error reading response: %s", err)
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
