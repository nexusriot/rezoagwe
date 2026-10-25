package model

import (
	"encoding/json"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"os"
	"reflect"
	"testing"
)

func TestSharedBoundaryCorpus(t *testing.T) {
	data, err := os.ReadFile("../../../testdata/boundaries.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Entries []struct {
			Key, Value, Node string
			EscapedBytes     map[string]int `json:"escaped_bytes"`
			Counter          uint64
			ExpiresAt        int64 `json:"expires_at"`
		}
		SortedKeys []string `json:"sorted_keys"`
		Buckets    []string
	}
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	s := NewKVStore("test")
	for _, e := range c.Entries {
		if pb.EscapedLen(e.Value) != e.EscapedBytes["go"] {
			t.Fatalf("wire byte budget for %q: %d != %d", e.Key, pb.EscapedLen(e.Value), e.EscapedBytes["go"])
		}
		u := pb.KVUpdate{Action: pb.KVSet, Key: e.Key, Value: e.Value, Version: pb.Version{Counter: e.Counter, Node: e.Node}, ExpiresAt: e.ExpiresAt}
		if !s.Apply(u) {
			t.Fatalf("refused %q", e.Key)
		}
	}
	keys := []string{}
	for _, u := range s.Updates() {
		keys = append(keys, u.Key)
	}
	if !reflect.DeepEqual(keys, c.SortedKeys) {
		t.Fatalf("key order: %q", keys)
	}
	if !reflect.DeepEqual(s.FingerprintVersion(16, 2).Buckets, c.Buckets) {
		t.Fatal("digest v2 differs from shared corpus")
	}
	raw, err := json.Marshal(s.State())
	if err != nil {
		t.Fatal(err)
	}
	var restored PersistState
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	copy := NewKVStore("test")
	copy.LoadState(restored)
	if !reflect.DeepEqual(copy.Updates(), s.Updates()) {
		t.Fatal("state round trip changed data")
	}
	next, err := copy.Write("next", "v", WriteOptions{})
	if err != nil || next.Version.Counter != 9007199254740991 {
		t.Fatalf("safe counter boundary: %+v %v", next, err)
	}
}
