package model

import (
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type Model struct {
	BroadcastPort int
	NodeTimeout   time.Duration
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
		nodes:         make(map[string]time.Time),
	}
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
