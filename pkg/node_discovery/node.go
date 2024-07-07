package node_discovery

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

const (
	broadcastPort     = 9999
	discoveryInterval = 5 * time.Second
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

func HandleConnection(conn *net.UDPConn, kv *KVStore, nodes *sync.Map) {
	buf := make([]byte, 1024)

	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Println("Error reading from UDP:", err)
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
			kv.Set(key, value)
			propagate(nodes, message)
			conn.WriteToUDP([]byte("OK"), addr)
		case "GET":
			value, ok := kv.Get(key)
			if ok {
				conn.WriteToUDP([]byte(value), addr)
			} else {
				conn.WriteToUDP([]byte("Key not found"), addr)
			}
		case "DELETE":
			kv.Delete(key)
			propagate(nodes, message)
			conn.WriteToUDP([]byte("OK"), addr)
		default:
			conn.WriteToUDP([]byte("Unknown command"), addr)
		}
	}
}

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
