package controller

import (
	"fmt"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"net"
	"strings"
	"sync"

	"github.com/golang/protobuf/proto"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	"github.com/nexusriot/rezoagwe/pkg/discovery/view"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

type Controller struct {
	debug bool
	view  *view.View
	model *model.Model
}

func NewController(
	debug bool,
	broadcastPort int,
) *Controller {
	m := model.NewModel()
	v := view.NewView()
	v.Frame.AddText(fmt.Sprintf("Rezoagve Discovery Node v.0.0.1 PoC"), true, tview.AlignCenter, tcell.ColorGreen)
	controller := Controller{
		debug: debug,
		view:  v,
		model: m,
	}
	return &controller
}

func (c *Controller) HandleConnection(conn *net.UDPConn) {
	buf := make([]byte, 1024)

	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			//fmt.Println("Error reading from UDP:", err)
			continue
		}

		message := strings.TrimSpace(string(buf[:n]))
		parts := strings.SplitN(message, " ", 3)

		if len(parts) < 2 {
			conn.WriteToUDP([]byte("Invalid command"), addr)
			continue
		}

		command, key := parts[0], parts[1]
		switch command {
		case "SET":
			if len(parts) < 3 {
				conn.WriteToUDP([]byte("SET command requires a value"), addr)
				continue
			}
			value := parts[2]
			c.model.Store.Set(key, value)
			propagate(c.model.Nodes, message)
			conn.WriteToUDP([]byte("OK"), addr)
		case "GET":
			value, ok := c.model.Store.Get(key)
			if ok {
				conn.WriteToUDP([]byte(value), addr)
			} else {
				conn.WriteToUDP([]byte("Key not found"), addr)
			}
		case "DELETE":
			c.model.Store.Delete(key)
			propagate(c.model.Nodes, message)
			conn.WriteToUDP([]byte("OK"), addr)
		default:
			conn.WriteToUDP([]byte("Unknown command"), addr)
		}
	}
}

func (c *Controller) Start() error {
	return c.view.App.Run()
}

//package main
//
//import (
//"fmt"
//"net"
//"os"
//"sync"
//
//nd "github.com/nexusriot/rezoagwe/pkg/discovery"
//)

//func main() {
//	if len(os.Args) < 3 {
//		fmt.Println("Usage: kv_node_discovery <address> <bootstrap_address>")
//		return
//	}
//
//	address := os.Args[1]
//	bootstrapAddress := os.Args[2]
//	nodes := &sync.Map{}
//
//	nd.RegisterNode(bootstrapAddress, address)
//
//	discoveredNodes := nd.DiscoverNodes(bootstrapAddress)
//	for _, node := range discoveredNodes {
//		if node != "" && node != address {
//			nodes.Store(node, true)
//		}
//	}
//
//	addr, err := net.ResolveUDPAddr("udp", address)
//	if err != nil {
//		fmt.Println("Error resolving address:", err)
//		return
//	}
//	conn, err := net.ListenUDP("udp", addr)
//	if err != nil {
//		fmt.Println("Error starting UDP server:", err)
//		return
//	}
//	defer conn.Close()
//
//	kv := nd.NewKVStore()
//	fmt.Printf("UDP node is listening on %s\n", address)
//
//	nd.HandleConnection(conn, kv, nodes)
//}

func propagate(nodes *sync.Map, message string) {
	nodes.Range(func(key, value interface{}) bool {
		addr, err := net.ResolveUDPAddr("udp", key.(string))
		if err != nil {
			fmt.Println("Error resolving address:", err)
			return true
		}
		conn, err := net.DialUDP("udp", nil, addr)
		if err != nil {
			fmt.Println("Error connecting to node:", err)
			return true
		}
		defer conn.Close()
		conn.Write([]byte(message))
		return true
	})
}

func DiscoverNodes(bootstrapAddr string) []string {
	conn, err := net.Dial("udp", bootstrapAddr)
	if err != nil {
		fmt.Println("Error connecting to bootstrap node:", err)
		return nil
	}
	defer conn.Close()

	dm := pb.BootstrapMessage{Action: pb.BootstrapAction_DISCOVER}
	data, err := proto.Marshal(&dm)
	if err != nil {
		fmt.Println("Error marshalling DISCOVER message:", err)
		return nil
	}
	_, err = conn.Write(data)
	if err != nil {
		fmt.Println("Error sending DISCOVER message:", err)
		return nil
	}

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		fmt.Println("Error reading response:", err)
		return nil
	}

	response := strings.TrimSpace(string(buf[:n]))
	return strings.Split(response, ",")
}

func RegisterNode(bootstrapAddr, nodeAddr string) {
	conn, err := net.Dial("udp", bootstrapAddr)
	if err != nil {
		fmt.Println("Error connecting to bootstrap node:", err)
		return
	}
	defer conn.Close()
	msg := pb.BootstrapMessage{
		Action: pb.BootstrapAction_REGISTER,
		Host:   &pb.Host{Host: nodeAddr},
	}
	toSend, err := proto.Marshal(&msg)
	_, err = conn.Write(toSend)
	if err != nil {
		fmt.Println("Error sending REGISTER message:", err)
	}
}
