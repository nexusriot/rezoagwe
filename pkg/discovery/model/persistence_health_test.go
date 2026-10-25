package model

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestPersistenceHealthDiskFullAndRecovery(t *testing.T) {
	p := NewPersister(filepath.Join(t.TempDir(), "state.json"))
	p.MarkDirty()
	p.write = func(PersistState) error { return syscall.ENOSPC }
	p.Save(1, PersistState{})
	h := p.Health()
	if !h.Pending || h.Error == "" || h.LastSuccess != 0 {
		t.Fatalf("hidden disk failure: %+v", h)
	}
	p.write = nil
	p.Save(1, PersistState{})
	h = p.Health()
	if h.Pending || h.Error != "" || h.LastSuccess == 0 {
		t.Fatalf("save did not recover: %+v", h)
	}
}
func TestUnreadableStateRetainedBeforeRecovery(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(file, []byte("{broken"), 0600)
	p := NewPersister(file)
	if _, _, err := p.Load(); err == nil {
		t.Fatal("corruption accepted")
	}
	backup := p.Health().RecoveryFile
	data, err := os.ReadFile(backup)
	if err != nil || string(data) != "{broken" {
		t.Fatalf("original lost: %q %v", data, err)
	}
	p.Save(1, PersistState{NodeID: "new"})
	if _, ok, err := NewPersister(file).Load(); err != nil || !ok {
		t.Fatalf("recovery cannot reload: %v", err)
	}
	if data, _ := os.ReadFile(backup); string(data) != "{broken" {
		t.Fatal("recovery backup overwritten")
	}
}
func TestPendingNewMutationSurvivesOlderSave(t *testing.T) {
	p := NewPersister(filepath.Join(t.TempDir(), "state.json"))
	p.MarkDirty()
	epoch := p.Epoch()
	p.MarkDirty()
	p.SaveAt(1, PersistState{}, epoch)
	if !p.Health().Pending {
		t.Fatal("older snapshot marked newer mutation saved")
	}
}
func TestClusterProfilesPreserveLegacyAndSeparateIdentity(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "node.json")
	os.WriteFile(legacy, []byte("legacy"), 0600)
	a, err := ProfileDataPath(legacy, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ProfileDataPath(legacy, "../beta")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("profiles collide")
	}
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Fatal("legacy copied into different cluster")
	}
	os.WriteFile(a, []byte("updated"), 0600)
	again, _ := ProfileDataPath(legacy, "alpha")
	data, _ := os.ReadFile(again)
	if string(data) != "updated" {
		t.Fatal("switching back lost source state")
	}
}

func TestHealthRemainsReadableDuringDiskWrite(t *testing.T) {
	p := NewPersister(filepath.Join(t.TempDir(), "state.json"))
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	p.write = func(PersistState) error { close(started); <-release; return nil }
	go func() { p.Save(1, PersistState{}); close(done) }()
	<-started
	status := make(chan PersistenceHealth, 1)
	go func() { status <- p.Health() }()
	select {
	case h := <-status:
		if !h.Pending {
			t.Error("in-flight write not pending")
		}
	case <-time.After(time.Second):
		t.Error("health blocked on disk write")
	}
	close(release)
	<-done
}
