package node_bootstrap

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	BroadcastPort = 9999
	NodeTimeout   = 10000 * time.Second
)

type BootstrapNode struct {
	mu    sync.Mutex
	nodes map[string]time.Time
}

func NewBootstrapNode() *BootstrapNode {
	return &BootstrapNode{
		nodes: make(map[string]time.Time),
	}
}

func (bn *BootstrapNode) RegisterNode(address string) {
	bn.mu.Lock()
	defer bn.mu.Unlock()
	bn.nodes[address] = time.Now()
	fmt.Printf("Registred node: %s\n", address)
	fmt.Printf("nodes: %s\n", bn.nodes)
}

func (bn *BootstrapNode) RemoveStaleNodes() {
	bn.mu.Lock()
	defer bn.mu.Unlock()
	now := time.Now()
	for address, timestamp := range bn.nodes {
		if now.Sub(timestamp) > NodeTimeout {
			delete(bn.nodes, address)
		}
	}
}

func (bn *BootstrapNode) GetNodes() []string {
	bn.mu.Lock()
	defer bn.mu.Unlock()
	nodes := []string{}
	fmt.Printf("Cur nodes: %s\n", bn.nodes)
	for address := range bn.nodes {
		nodes = append(nodes, address)
	}
	return nodes
}

func HandleBootstrap(bn *BootstrapNode, conn *net.UDPConn) {
	buf := make([]byte, 1024)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Println("Error reading from UDP:", err)
			continue
		}

		message := strings.TrimSpace(string(buf[:n]))
		if message == "DISCOVER" {
			nodes := bn.GetNodes()
			response := strings.Join(nodes, ",")
			conn.WriteToUDP([]byte(response), addr)
		} else if strings.HasPrefix(message, "REGISTER:") {
			nodeAddress := strings.TrimPrefix(message, "REGISTER:")
			bn.RegisterNode(nodeAddress)
			conn.WriteToUDP([]byte("REGISTERED"), addr)
			fmt.Printf("Nodes: %s\n", bn.GetNodes())
		}
	}
}
