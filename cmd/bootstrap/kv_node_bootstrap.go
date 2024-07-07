package main

import (
	"fmt"
	"net"
	"time"

	nb "github.com/nexusriot/rezoagwe/pkg/node_bootstrap"
)

func main() {
	addr := net.UDPAddr{
		Port: nb.BroadcastPort,
		IP:   net.ParseIP("0.0.0.0"),
	}
	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		fmt.Println("Error starting UDP server:", err)
		return
	}
	defer conn.Close()

	bn := nb.NewBootstrapNode()
	go func() {
		for {
			bn.RemoveStaleNodes()
			time.Sleep(nb.NodeTimeout / 2)
		}
	}()

	fmt.Printf("Bootstrap node is listening on port %d\n", nb.BroadcastPort)
	nb.HandleBootstrap(bn, conn)
}
