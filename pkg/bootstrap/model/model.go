package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// NodeInfo is a registered node: where it is, what it calls itself, and when it
// last said anything.
type NodeInfo struct {
	Addr     string    `json:"addr"`
	Nick     string    `json:"nick,omitempty"`
	LastSeen time.Time `json:"last_seen"`
}

// rosterFile is the persisted form of the roster.
type rosterFile struct {
	Nodes []NodeInfo `json:"nodes"`
}

type Model struct {
	BroadcastPort int
	NodeTimeout   time.Duration
	StartedAt     time.Time
	DataPath      string

	mu    sync.Mutex
	nodes map[string]NodeInfo

	saveMu sync.Mutex
}

func NewModel(broadcastPort int, nodeTimeout time.Duration, dataPath string) *Model {
	m := &Model{
		BroadcastPort: broadcastPort,
		NodeTimeout:   nodeTimeout,
		StartedAt:     time.Now(),
		DataPath:      dataPath,
		nodes:         make(map[string]NodeInfo),
	}
	m.load()
	return m
}

// DefaultDataPath is where a bootstrap persists its roster. It is derived from
// the port so several rendezvous services on one machine do not collide.
func DefaultDataPath(port int) string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = "."
	}
	return filepath.Join(base, "rezoagwe", fmt.Sprintf("bootstrap-%d.json", port))
}

// load restores the roster written by a previous run.
//
// Every restored node's last-seen time is reset to now rather than kept: the
// service was down, so nobody could have been heard from, and expiring the whole
// roster the instant it loads would make persistence pointless. A node that is
// genuinely gone is dropped after one timeout anyway.
func (bn *Model) load() {
	if bn.DataPath == "" {
		return
	}
	data, err := os.ReadFile(bn.DataPath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Errorf("load roster from %s: %s", bn.DataPath, err)
		return
	}
	var file rosterFile
	if err := json.Unmarshal(data, &file); err != nil {
		log.Errorf("parse roster %s: %s", bn.DataPath, err)
		return
	}
	now := time.Now()
	bn.mu.Lock()
	for _, n := range file.Nodes {
		if n.Addr == "" {
			continue
		}
		n.LastSeen = now
		bn.nodes[n.Addr] = n
	}
	count := len(bn.nodes)
	bn.mu.Unlock()
	log.Debugf("restored %d nodes from %s", count, bn.DataPath)
}

// save writes the roster atomically.
func (bn *Model) save() {
	if bn.DataPath == "" {
		return
	}
	infos := bn.GetNodeInfos()
	bn.saveMu.Lock()
	defer bn.saveMu.Unlock()
	if dir := filepath.Dir(bn.DataPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Errorf("create roster dir: %s", err)
			return
		}
	}
	data, err := json.MarshalIndent(rosterFile{Nodes: infos}, "", "  ")
	if err != nil {
		log.Errorf("marshal roster: %s", err)
		return
	}
	tmp := bn.DataPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Errorf("write roster: %s", err)
		return
	}
	if err := os.Rename(tmp, bn.DataPath); err != nil {
		log.Errorf("replace roster: %s", err)
	}
}

// GetNodeInfos returns every known node, sorted by address for a stable UI
// ordering.
func (bn *Model) GetNodeInfos() []NodeInfo {
	bn.mu.Lock()
	defer bn.mu.Unlock()
	out := make([]NodeInfo, 0, len(bn.nodes))
	for _, n := range bn.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// RegisterNode records (or refreshes) a node. It reports whether this was a
// new arrival, so the caller can avoid rewriting the roster on every heartbeat.
func (bn *Model) RegisterNode(address, nick string) bool {
	if address == "" {
		return false
	}
	bn.mu.Lock()
	prev, existed := bn.nodes[address]
	info := NodeInfo{Addr: address, Nick: nick, LastSeen: time.Now()}
	if nick == "" {
		info.Nick = prev.Nick
	}
	bn.nodes[address] = info
	bn.mu.Unlock()

	changed := !existed || prev.Nick != info.Nick
	if changed {
		bn.save()
	}
	return !existed
}

// RemoveStaleNodes drops nodes that stopped registering and returns them.
func (bn *Model) RemoveStaleNodes() []NodeInfo {
	bn.mu.Lock()
	now := time.Now()
	var removed []NodeInfo
	for address, info := range bn.nodes {
		if now.Sub(info.LastSeen) > bn.NodeTimeout {
			delete(bn.nodes, address)
			removed = append(removed, info)
		}
	}
	bn.mu.Unlock()
	if len(removed) > 0 {
		bn.save()
	}
	return removed
}

func (bn *Model) GetNodes() []string {
	infos := bn.GetNodeInfos()
	out := make([]string, 0, len(infos))
	for _, n := range infos {
		out = append(out, n.Addr)
	}
	return out
}

// Roster is the wire form of the node list, excluding the requester itself —
// a node has no use for its own address and would otherwise gossip with itself.
func (bn *Model) Roster(exclude string) pb.BootstrapRoster {
	infos := bn.GetNodeInfos()
	roster := pb.BootstrapRoster{Peers: make([]pb.BootstrapPeer, 0, len(infos))}
	for _, n := range infos {
		if n.Addr == exclude {
			continue
		}
		roster.Peers = append(roster.Peers, pb.BootstrapPeer{
			Addr:     n.Addr,
			Nick:     n.Nick,
			LastSeen: n.LastSeen.Unix(),
		})
	}
	return roster
}
