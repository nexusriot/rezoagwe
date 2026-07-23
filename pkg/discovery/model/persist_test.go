package model

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

func sampleState() PersistState {
	return PersistState{
		Clock: 7,
		Entries: map[string]kvEntry{
			"a": {Value: "1", Version: pb.Version{Counter: 5, Node: ":3137"}},
			"b": {Value: "two", Version: pb.Version{Counter: 6, Node: ":3137"}},
			"c": {Version: pb.Version{Counter: 7, Node: ":3138"}, Deleted: true},
		},
	}
}

func TestPersisterRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.json")
	p := NewPersister(path)

	want := sampleState()
	p.Save(1, want)

	got, found, err := p.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("Load: found = false after Save")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip = %+v, want %+v", got, want)
	}
}

// An out-of-order Save with an older generation must not overwrite a newer
// on-disk state.
func TestPersisterGenerationGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.json")
	p := NewPersister(path)

	newer := PersistState{Clock: 2, Entries: map[string]kvEntry{"k": {Value: "new", Version: pb.Version{Counter: 2, Node: "n"}}}}
	stale := PersistState{Clock: 1, Entries: map[string]kvEntry{"k": {Value: "stale", Version: pb.Version{Counter: 1, Node: "n"}}}}

	p.Save(2, newer)
	p.Save(1, stale) // older gen: must be ignored

	got, _, err := p.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Entries["k"].Value != "new" {
		t.Fatalf("k = %q, want %q (stale write leaked through)", got.Entries["k"].Value, "new")
	}
}

func TestPersisterLoadMissingIsNotFound(t *testing.T) {
	p := NewPersister(filepath.Join(t.TempDir(), "does-not-exist.json"))
	got, found, err := p.Load()
	if err != nil {
		t.Fatalf("Load of missing file returned error: %v", err)
	}
	if found {
		t.Fatal("Load of missing file: found = true")
	}
	if len(got.Entries) != 0 {
		t.Fatalf("Load of missing file returned entries: %v", got.Entries)
	}
}

func TestPersisterLoadCorruptErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	if _, _, err := NewPersister(path).Load(); err == nil {
		t.Fatal("Load of corrupt file: expected error, got nil")
	}
}

// The atomic write must leave no stray .tmp file behind on success.
func TestPersisterNoTmpLeftover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.json")
	NewPersister(path).Save(1, sampleState())
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("stray .tmp file left after Save (stat err = %v)", err)
	}
}

func TestDefaultDataPath(t *testing.T) {
	p := DefaultDataPath(":3137")
	if !strings.HasSuffix(p, ".json") {
		t.Fatalf("DefaultDataPath = %q, want a .json file", p)
	}
	if strings.Contains(filepath.Base(p), ":") {
		t.Fatalf("basename %q still contains ':' — not filesystem-safe", filepath.Base(p))
	}
	if filepath.Base(p) != "_3137.json" {
		t.Fatalf("basename = %q, want %q", filepath.Base(p), "_3137.json")
	}
	if a, b := DefaultDataPath(":3137"), DefaultDataPath(":3138"); a == b {
		t.Fatal("distinct node addresses produced the same data path")
	}
}
