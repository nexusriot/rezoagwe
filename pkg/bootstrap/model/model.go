package model

import (
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type Model struct {
	BroadcastPort int
	NodeTimeout   time.Duration
	StartedAt     time.Time
	mu            sync.Mutex
	nodes         map[string]time.Time
}

type BootstrapNode struct {
	mu    sync.Mutex
	nodes map[string]time.Time
}

func NewModel(broadcastPort int, nodeTimout time.Duration) *Model {
	return &Model{
		BroadcastPort: broadcastPort,
		NodeTimeout:   nodeTimout,
		StartedAt:     time.Now(),
		nodes:         make(map[string]time.Time),
	}
}

// NodeInfo is a node address paired with the time it last REGISTERed.
type NodeInfo struct {
	Addr     string
	LastSeen time.Time
}

// GetNodeInfos returns every known node with its last-seen timestamp,
// sorted by address for a stable UI ordering.
func (bn *Model) GetNodeInfos() []NodeInfo {
	bn.mu.Lock()
	defer bn.mu.Unlock()
	out := make([]NodeInfo, 0, len(bn.nodes))
	for addr, ts := range bn.nodes {
		out = append(out, NodeInfo{Addr: addr, LastSeen: ts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

func (bn *Model) RegisterNode(address string) {
	bn.mu.Lock()
	defer bn.mu.Unlock()
	bn.nodes[address] = time.Now()
	log.Debugf("Registred node: %s", address)
	log.Debugf("nodes: %s", bn.nodes)
}

func (bn *Model) RemoveStaleNodes() {
	bn.mu.Lock()
	defer bn.mu.Unlock()
	now := time.Now()
	for address, timestamp := range bn.nodes {
		if now.Sub(timestamp) > bn.NodeTimeout {
			delete(bn.nodes, address)
		}
	}
}

func (bn *Model) GetNodes() []string {
	bn.mu.Lock()
	defer bn.mu.Unlock()
	var nodes []string
	log.Debugf("Get nodes: %s", bn.nodes)
	for address := range bn.nodes {
		nodes = append(nodes, address)
	}
	return nodes
}
