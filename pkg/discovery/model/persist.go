package model

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

// Persister writes the KV store to a JSON file. Writes are serialized and
// gated on a generation number so a slow, out-of-order save can never
// overwrite the file with a stale snapshot.
type Persister struct {
	path    string
	mu      sync.Mutex
	lastGen uint64
}

func NewPersister(path string) *Persister {
	return &Persister{path: path}
}

// Save persists state if gen is newer than the last write. It matches the
// KVStore.SetOnChange callback signature and logs (rather than returns)
// errors, since it is called fire-and-forget from mutation sites.
func (p *Persister) Save(gen uint64, state PersistState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen != 0 && gen <= p.lastGen {
		return
	}
	if err := p.writeAtomic(state); err != nil {
		log.Errorf("persist store to %s: %s", p.path, err)
		return
	}
	p.lastGen = gen
}

func (p *Persister) writeAtomic(state PersistState) error {
	if dir := filepath.Dir(p.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// Compact, not indented: this file is rewritten on every flush and is read
	// by the node, not by a person. Indenting a large store roughly doubles the
	// bytes written for nothing.
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	// Rename is atomic on the same filesystem: a reader sees either the old
	// file or the fully-written new one, never a truncated write.
	return os.Rename(tmp, p.path)
}

// Load reads the persisted state. The returned bool is false when the file
// does not exist yet (a fresh node), in which case the caller starts empty.
func (p *Persister) Load() (PersistState, bool, error) {
	data, err := os.ReadFile(p.path)
	if errors.Is(err, os.ErrNotExist) {
		return PersistState{}, false, nil
	}
	if err != nil {
		return PersistState{}, false, err
	}
	var state PersistState
	if err := json.Unmarshal(data, &state); err != nil {
		return PersistState{}, false, err
	}
	return state, true, nil
}

// DefaultDataPath is where a node persists its store when no explicit path is
// given. It is derived from the node address so several nodes on one machine
// do not clobber each other's file.
func DefaultDataPath(nodeAddr string) string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = "." // fall back to the working directory
	}
	return filepath.Join(base, "rezoagwe", sanitizeAddr(nodeAddr)+".json")
}

// sanitizeAddr turns a node address into a filesystem-safe basename,
// e.g. ":3137" -> "_3137", "127.0.0.1:3137" -> "127.0.0.1_3137".
func sanitizeAddr(addr string) string {
	s := strings.NewReplacer(":", "_", "/", "_", "\\", "_", " ", "_").Replace(addr)
	if s == "" {
		return "node"
	}
	return s
}
