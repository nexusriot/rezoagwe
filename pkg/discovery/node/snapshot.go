package node

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// snapshotPrefix and snapshotSuffix bracket a snapshot's filename. Rotation
// only ever considers files that match both, so a directory shared with
// anything else loses nothing.
const (
	snapshotPrefix = "rezoagwe-"
	snapshotSuffix = ".json"
	// snapshotStamp sorts lexicographically in time order, which is what lets
	// rotation pick the oldest without stat-ing anything.
	snapshotStamp = "20060102-150405"
)

// SnapshotConfig describes periodic export to disk.
//
// The state file is a replica, not a backup: it is rewritten in place on every
// flush, so a key deleted by mistake is gone from it within 250 ms and a
// corrupted store is faithfully preserved. Export existed but only as a pull —
// a node that lost its disk took whatever only it held with it unless somebody
// had remembered to curl it.
type SnapshotConfig struct {
	// Dir is where snapshots are written. Empty disables them.
	Dir string
	// Interval is how often. Zero with a Dir set means hourly.
	Interval time.Duration
	// Keep is how many to retain; older ones are removed after each write.
	// Zero keeps 24 — a day of hourly snapshots.
	Keep int
}

func (c *SnapshotConfig) applyDefaults() {
	if c.Interval == 0 {
		c.Interval = time.Hour
	}
	if c.Keep == 0 {
		c.Keep = 24
	}
}

// snapshotLoop writes a snapshot on a timer until the node stops.
func (n *Node) snapshotLoop() {
	cfg := n.cfg.Snapshot
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-t.C:
			if path, err := n.WriteSnapshot(); err != nil {
				// A failed backup that says nothing is a backup you find out
				// about when you need it.
				log.Errorf("snapshot: %s", err)
				n.logActivity("snapshot failed: %s", err)
			} else {
				log.Debugf("snapshot written to %s", path)
			}
		}
	}
}

// WriteSnapshot exports the store to the configured directory and prunes older
// snapshots, returning the path written.
//
// The export carries versions, so restoring one through POST /import (without
// mode=seed) merges it under the usual last-write-wins rule rather than
// forcing stale data back over what the cluster has moved on to.
func (n *Node) WriteSnapshot() (string, error) {
	cfg := n.cfg.Snapshot
	if cfg.Dir == "" {
		return "", fmt.Errorf("no snapshot directory configured")
	}
	data, err := n.Export()
	if err != nil {
		return "", fmt.Errorf("export: %w", err)
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return "", err
	}
	name := snapshotPrefix + time.Now().UTC().Format(snapshotStamp) + snapshotSuffix
	path := filepath.Join(cfg.Dir, name)

	// Written beside the target and renamed, so a reader never sees a half
	// -written snapshot and a crash mid-write leaves no file claiming to be one.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	n.logActivity("snapshot written to %s (%d bytes)", name, len(data))
	if err := pruneSnapshots(cfg.Dir, cfg.Keep); err != nil {
		// The snapshot itself landed; a failed prune is worth reporting but
		// not worth calling the write a failure.
		log.Debugf("prune snapshots in %s: %s", cfg.Dir, err)
	}
	return path, nil
}

// pruneSnapshots removes all but the newest keep snapshots in dir.
func pruneSnapshots(dir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, snapshotPrefix) || !strings.HasSuffix(name, snapshotSuffix) {
			continue
		}
		names = append(names, name)
	}
	if len(names) <= keep {
		return nil
	}
	// The timestamp is fixed-width and zero-padded, so name order is time
	// order — no need to stat every file to find the oldest.
	sort.Strings(names)
	var firstErr error
	for _, name := range names[:len(names)-keep] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
