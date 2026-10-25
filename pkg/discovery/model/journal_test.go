package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestJournalRecoveryAndCheckpoint(t *testing.T) {
	for _, mode := range []string{"journal", "journal-sync"} {
		t.Run(mode, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "state.json")
			p := NewPersister(file)
			p.SetDurability(mode)
			for i := 1; i <= 65; i++ {
				p.Save(uint64(i), PersistState{Clock: uint64(i), Entries: map[string]kvEntry{"k": {Value: fmt.Sprint(i)}}})
				if p.Health().Error != "" {
					t.Fatal(p.Health())
				}
			}
			f, _ := os.OpenFile(file+".wal", os.O_APPEND|os.O_WRONLY, 0600)
			f.WriteString(`{"data":"torn`)
			f.Close()
			recovered := NewPersister(file)
			got, found, err := recovered.Load()
			if err != nil || !found || got.Clock != 65 || got.Entries["k"].Value != "65" {
				t.Fatalf("%+v %v %v", got, found, err)
			}
			recovered.SetDurability(mode)
			recovered.Save(66, PersistState{Clock: 66, Entries: map[string]kvEntry{}})
			got, _, err = NewPersister(file).Load()
			if err != nil || len(got.Entries) != 0 {
				t.Fatal("removed keys returned", got, err)
			}
			if p.Health().Durable != (mode == "journal-sync") {
				t.Fatal("incorrect durability", p.Health())
			}
		})
	}
}
func TestJournalCorruptionBlocksOverwrite(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	p := NewPersister(file)
	p.SetDurability("journal")
	p.Save(1, PersistState{Entries: map[string]kvEntry{"k": {Value: "keep"}}})
	f, _ := os.OpenFile(file+".wal", os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString("{bad}\n")
	f.Close()
	before, _ := os.ReadFile(file + ".wal")
	q := NewPersister(file)
	if _, _, err := q.Load(); err == nil {
		t.Fatal("corruption ignored")
	}
	q.Save(2, PersistState{})
	after, _ := os.ReadFile(file + ".wal")
	if string(before) != string(after) {
		t.Fatal("corrupt evidence overwritten")
	}
}
func TestJournalMigratesWithLegacyProfile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.json")
	p := NewPersister(file)
	p.SetDurability("journal")
	p.Save(1, PersistState{Clock: 1, Entries: map[string]kvEntry{"k": {Value: "journal-only"}}})
	migrated, err := ProfileDataPath(file, "test")
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := NewPersister(migrated).Load()
	if err != nil || state.Entries["k"].Value != "journal-only" {
		t.Fatal(state, err)
	}
}
func TestMissingJournalCheckpointBlocksRecovery(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	p := NewPersister(file)
	p.SetDurability("journal")
	for i := 1; i <= 65; i++ {
		p.Save(uint64(i), PersistState{Clock: uint64(i), Entries: map[string]kvEntry{"stable": {Value: "keep"}, "changed": {Value: fmt.Sprint(i)}}})
	}
	before, _ := os.ReadFile(file + ".wal")
	os.Remove(file)
	q := NewPersister(file)
	if _, _, err := q.Load(); err == nil {
		t.Fatal("partial journal accepted without its checkpoint")
	}
	q.Save(1, PersistState{})
	after, _ := os.ReadFile(file + ".wal")
	if string(before) != string(after) {
		t.Fatal("journal overwritten")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("missing checkpoint replaced")
	}
}
func TestSharedJournalCorpus(t *testing.T) {
	var corpus struct {
		Checkpoint json.RawMessage   `json:"checkpoint"`
		Records    []json.RawMessage `json:"records"`
	}
	data, err := os.ReadFile("../../../testdata/journal.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(file, corpus.Checkpoint, 0600)
	var wal []byte
	for _, record := range corpus.Records {
		var compact bytes.Buffer
		json.Compact(&compact, record)
		wal = append(wal, compact.Bytes()...)
		wal = append(wal, '\n')
	}
	os.WriteFile(file+".wal", wal, 0600)
	state, _, err := NewPersister(file).Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Clock != 5 || state.NodeID != "journal-fixture" || state.Entries["keep"].Value != "checkpoint value" || state.Entries["go"].Value != "Go <>& 😀\nrecord" || state.Entries["__proto__"].Value != "JS 😀 <>&" || !state.Entries["deleted"].Deleted {
		t.Fatal(state)
	}
	if _, exists := state.Entries["removed"]; exists {
		t.Fatal("removed entry returned")
	}
}
