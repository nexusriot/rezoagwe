package model

import (
	"fmt"
	"go.uber.org/zap"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

const (
	BroadcastPort = 9999
	NodeTimeout   = 10000 * time.Second
)

type Model struct {
	mu    sync.Mutex
	nodes map[string]time.Time
}

type BootstrapNode struct {
	mu     sync.Mutex
	nodes  map[string]time.Time
	logger *zap.Logger
}

func NewBootstrapNode() *BootstrapNode {
	var loggerConfig = zap.NewProductionConfig()
	loggerConfig.Level.SetLevel(zap.DebugLevel)
	logger, _ := loggerConfig.Build()
	return &BootstrapNode{
		nodes:  make(map[string]time.Time),
		logger: logger,
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
		loaded := new(pb.BootstrapMessage)
		err = proto.Unmarshal(buf[:n], loaded)
		if err != nil {
			bn.logger.Error("Failed to unmarshal b message", zap.Error(err))
		}

		if loaded.Action == pb.BootstrapAction_DISCOVER {
			nodes := bn.GetNodes()
			response := strings.Join(nodes, ",")
			conn.WriteToUDP([]byte(response), addr)

		} else if loaded.Action == pb.BootstrapAction_REGISTER {
			nodeAddress := loaded.Host.GetHost()
			bn.RegisterNode(nodeAddress)
			conn.WriteToUDP([]byte("REGISTERED"), addr)
			fmt.Printf("Nodes: %s\n", bn.GetNodes())
		}
	}
}
