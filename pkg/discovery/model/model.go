package model

import (
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/google/uuid"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	log "github.com/sirupsen/logrus"
	"net"
	"strings"
	"sync"
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

type Model struct {
	Store         *KVStore
	Nodes         *sync.Map
	BootstrapAddr string
	NodeAddr      string
	NodeUUID      string
}

func NewModel(bootstrapAddr, nodeAddr string) *Model {
	controller := Model{
		NodeUUID:      uuid.New().String(),
		BootstrapAddr: bootstrapAddr,
		NodeAddr:      nodeAddr,
		Store:         NewKVStore(),
		Nodes:         &sync.Map{},
	}
	return &controller
}

func (bn *Model) RegisterNode() {
	conn, err := net.Dial("udp", bn.BootstrapAddr)
	if err != nil {
		log.Errorf("Error connecting to bootstrap node: %s", err)
		return
	}
	defer conn.Close()
	msg := pb.BootstrapMessage{
		Action: pb.BootstrapAction_REGISTER,
		Host:   &pb.Host{Host: bn.NodeAddr},
	}
	toSend, err := proto.Marshal(&msg)
	_, err = conn.Write(toSend)
	if err != nil {
		log.Errorf("Error sending REGISTER message: %s", err)
	}
}

func (bn *Model) GetNodes() []string {
	var res []string

	bn.Nodes.Range(func(key, value interface{}) bool {
		res = append(res, key.(string))
		return true
	})
	return res
}

func (bn *Model) GetStore() map[string]string {
	return bn.Store.store
}

func (bn *Model) DiscoverNodes() []string {
	conn, err := net.Dial("udp", bn.BootstrapAddr)
	if err != nil {
		log.Errorf("Error connecting to bootstrap node: %s", err)
		return nil
	}
	defer conn.Close()

	dm := pb.BootstrapMessage{Action: pb.BootstrapAction_DISCOVER}
	data, err := proto.Marshal(&dm)
	if err != nil {
		log.Errorf("Error marshalling DISCOVER message: %s", err)
		return nil
	}
	_, err = conn.Write(data)
	if err != nil {
		log.Errorf("Error sending DISCOVER message: %s", err)
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
