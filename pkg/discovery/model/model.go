package model

import (
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
	Store *KVStore
	Nodes *sync.Map
}

func NewModel() *Model {
	controller := Model{
		Store: NewKVStore(),
		Nodes: &sync.Map{},
	}
	return &controller
}
