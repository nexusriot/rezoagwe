package model

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

func TestPersisterRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p := NewPersister(path)

	state := PersistState{
		NodeID:  "id-1",
		Clock:   7,
		Entries: map[string]kvEntry{"k": {Value: "v", Version: pb.Version{Counter: 7, Node: "id-1"}}},
		Chat:    pb.ChatLog{{Text: "hello"}},
	}
	p.Save(1, state)

	got, found, err := p.Load()
	if err != nil || !found {
		t.Fatalf("Load = (found %v, err %v)", found, err)
	}
	if got.NodeID != "id-1" || got.Clock != 7 || got.Entries["k"].Value != "v" {
		t.Fatalf("state round trip lost data: %+v", got)
	}
	if len(got.Chat) != 1 || got.Chat[0].Text != "hello" {
		t.Fatalf("chat round trip lost data: %+v", got.Chat)
	}
}

// The generation guard is what stops a slow write from overwriting the file
// with an older snapshot.
func TestPersisterIgnoresOutOfOrderSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p := NewPersister(path)

	p.Save(5, PersistState{Clock: 5})
	p.Save(2, PersistState{Clock: 2})

	got, _, err := p.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Clock != 5 {
		t.Fatalf("clock = %d, want 5 (stale save won)", got.Clock)
	}
}

func TestPersisterLoadMissingFile(t *testing.T) {
	p := NewPersister(filepath.Join(t.TempDir(), "absent.json"))
	_, found, err := p.Load()
	if err != nil {
		t.Fatalf("load of a missing file returned %v", err)
	}
	if found {
		t.Fatal("reported a state file that does not exist")
	}
}

// A temp file must never be left behind, and the real file must be complete —
// that is the point of writing through a rename.
func TestPersisterWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	NewPersister(path).Save(1, PersistState{Clock: 1})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var state PersistState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}
}

// Upgrading from wire v1 must not throw away the KV store just because chat
// history used to be a list of strings.
func TestPersisterReadsLegacyChatFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{
	  "clock": 3,
	  "entries": {"k": {"value": "v", "version": {"counter": 3, "node": ":3137"}}},
	  "chat": ["[gray]12:00[-] bob: hi"]
	}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}

	state, found, err := NewPersister(path).Load()
	if err != nil || !found {
		t.Fatalf("Load = (found %v, err %v)", found, err)
	}
	if state.Entries["k"].Value != "v" {
		t.Fatalf("legacy KV lost: %+v", state.Entries)
	}
	if len(state.Chat) != 1 || state.Chat[0].Text != "12:00 bob: hi" {
		t.Fatalf("legacy chat not converted: %+v", state.Chat)
	}
}

func TestSanitizeAddr(t *testing.T) {
	cases := map[string]string{
		":3137":          "_3137",
		"127.0.0.1:3137": "127.0.0.1_3137",
		"":               "node",
		"a/b\\c:1":       "a_b_c_1",
	}
	for in, want := range cases {
		if got := sanitizeAddr(in); got != want {
			t.Errorf("sanitizeAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
