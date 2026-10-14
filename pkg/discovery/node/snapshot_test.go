package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/transport"
)

func snapshotNode(t *testing.T, dir string, keep int) *Node {
	t.Helper()
	net := transport.NewMemNet(1)
	n, err := New(Config{
		NodeAddr:          "10.0.0.1:3137",
		Nick:              "alice",
		Transport:         net.Node("10.0.0.1:3137"),
		GossipInterval:    idleInterval,
		HeartbeatInterval: idleInterval,
		EvictThreshold:    idleInterval,
		SweepInterval:     idleInterval,
		Snapshot:          SnapshotConfig{Dir: dir, Interval: idleInterval, Keep: keep},
	}, nil)
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	n.Start()
	t.Cleanup(n.Stop)
	return n
}

func TestSnapshotWritesAnImportableExport(t *testing.T) {
	dir := t.TempDir()
	n := snapshotNode(t, dir, 24)
	if _, err := n.Set("colour", "blue", 0); err != nil {
		t.Fatalf("set: %v", err)
	}

	path, err := n.WriteSnapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var f ExportFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("snapshot is not an export: %v", err)
	}
	if len(f.Entries) != 1 || f.Entries[0].Key != "colour" {
		t.Fatalf("snapshot entries = %+v, want the one key", f.Entries)
	}
	// Versions have to survive, or a restore forces stale data back over
	// whatever the cluster moved on to.
	if f.Entries[0].Version.Zero() {
		t.Error("snapshot carries no version")
	}

	// And it restores: a fresh node imports it and holds the same value.
	other := snapshotNode(t, t.TempDir(), 24)
	applied, err := other.Import(data, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if applied != 1 {
		t.Fatalf("imported %d entries, want 1", applied)
	}
	if v, ok := other.Get("colour"); !ok || v != "blue" {
		t.Errorf("restored value = %q (%v), want blue", v, ok)
	}
}

func TestSnapshotRotationKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	n := snapshotNode(t, dir, 3)

	// The filename is stamped to the second, so writes in one test would
	// collide; the names are planted directly to drive rotation alone.
	for _, stamp := range []string{"20260101-000001", "20260101-000002", "20260101-000003", "20260101-000004"} {
		name := snapshotPrefix + stamp + snapshotSuffix
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatalf("plant %s: %v", name, err)
		}
	}
	if err := pruneSnapshots(dir, 3); err != nil {
		t.Fatalf("prune: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if len(left) != 3 {
		t.Fatalf("%d snapshots left, want 3: %v", len(left), left)
	}
	for _, name := range left {
		if strings.Contains(name, "000001") {
			t.Errorf("rotation kept the oldest: %v", left)
		}
	}
	_ = n
}

// A directory shared with anything else must lose nothing to rotation.
func TestSnapshotRotationIgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "important.json")
	if err := os.WriteFile(foreign, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("plant: %v", err)
	}
	for i := 1; i <= 4; i++ {
		name := snapshotPrefix + "2026010" + string(rune('0'+i)) + "-000000" + snapshotSuffix
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatalf("plant: %v", err)
		}
	}
	if err := pruneSnapshots(dir, 1); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("rotation removed a file it did not write: %v", err)
	}
}

func TestSnapshotRefusedWithoutADirectory(t *testing.T) {
	net := transport.NewMemNet(1)
	n, err := New(Config{
		NodeAddr:          "10.0.0.1:3137",
		Transport:         net.Node("10.0.0.1:3137"),
		GossipInterval:    idleInterval,
		HeartbeatInterval: idleInterval,
		EvictThreshold:    idleInterval,
		SweepInterval:     idleInterval,
	}, nil)
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	n.Start()
	defer n.Stop()

	if _, err := n.WriteSnapshot(); err == nil {
		t.Error("snapshot succeeded with no directory configured")
	}
}

func TestSnapshotDefaultsApplyOnlyWhenEnabled(t *testing.T) {
	off := Config{}
	off.applyDefaults()
	if off.Snapshot.Interval != 0 || off.Snapshot.Keep != 0 {
		t.Errorf("snapshot defaults applied with no directory: %+v", off.Snapshot)
	}

	on := Config{Snapshot: SnapshotConfig{Dir: "/tmp/x"}}
	on.applyDefaults()
	if on.Snapshot.Interval != time.Hour {
		t.Errorf("interval = %s, want 1h", on.Snapshot.Interval)
	}
	if on.Snapshot.Keep != 24 {
		t.Errorf("keep = %d, want 24", on.Snapshot.Keep)
	}
}
