package main

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	broadcastPort = 9999
	nodeTimeout   = 10000 * time.Second
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
		if now.Sub(timestamp) > nodeTimeout {
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

func handleBootstrap(bn *BootstrapNode, conn *net.UDPConn) {
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

func main() {
	addr := net.UDPAddr{
		Port: broadcastPort,
		IP:   net.ParseIP("0.0.0.0"),
	}
	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		fmt.Println("Error starting UDP server:", err)
		return
	}
	defer conn.Close()

	bn := NewBootstrapNode()
	go func() {
		for {
			bn.RemoveStaleNodes()
			time.Sleep(nodeTimeout / 2)
		}
	}()

	fmt.Printf("Bootstrap node is listening on port %d\n", broadcastPort)
	handleBootstrap(bn, conn)
}
