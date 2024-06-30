package main

import (
	"fmt"
	"net"
	"os"
	"sync"

	nd "github.com/nexusriot/rezoagwe/pkg/node_discovery"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: kv_node_discovery <address> <bootstrap_address>")
		return
	}

	address := os.Args[1]
	bootstrapAddress := os.Args[2]
	nodes := &sync.Map{}

	nd.RegisterNode(bootstrapAddress, address)

	discoveredNodes := nd.DiscoverNodes(bootstrapAddress)
	for _, node := range discoveredNodes {
		if node != "" && node != address {
			nodes.Store(node, true)
		}
	}

	addr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		fmt.Println("Error resolving address:", err)
		return
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Println("Error starting UDP server:", err)
		return
	}
	defer conn.Close()

	kv := nd.NewKVStore()
	fmt.Printf("UDP node is listening on %s\n", address)

	nd.HandleConnection(conn, kv, nodes)
}
