package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// Persister writes the KV store to a JSON file. Writes are serialized and
// gated on a generation number so a slow, out-of-order save can never
// overwrite the file with a stale snapshot.
type Persister struct {
	mode           string
	previous       PersistState
	journalRecords int
	healthMu       sync.Mutex
	write          func(PersistState) error
	path           string
	mu             sync.Mutex
	lastGen        uint64
	epoch          uint64
	health         PersistenceHealth
	blocked        bool
}

func NewPersister(path string) *Persister {
	return &Persister{path: path, mode: "snapshot", health: PersistenceHealth{Enabled: true, Mode: "snapshot"}}
}

// Save persists state if gen is newer than the last write. It matches the
// KVStore.SetOnChange callback signature and logs (rather than returns)
// errors, since it is called fire-and-forget from mutation sites.
func (p *Persister) Save(gen uint64, state PersistState) {
	p.SaveAt(gen, state, p.Epoch())
}
func (p *Persister) SaveAt(gen uint64, state PersistState, epoch uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen != 0 && gen <= p.lastGen {
		return
	}
	p.healthMu.Lock()
	p.health.Pending = true
	p.health.Durable = false
	p.healthMu.Unlock()
	if p.blocked {
		return
	}
	write := p.write
	if write == nil {
		write = p.writeState
	}
	if err := write(state); err != nil {
		p.healthMu.Lock()
		p.health.Error = err.Error()
		p.healthMu.Unlock()
		log.Errorf("persist store to %s: %s", p.path, err)
		return
	}
	p.lastGen = gen
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	p.health.LastSuccess = time.Now().UnixMilli()
	p.health.Durable = p.mode == "journal-sync" && p.epoch == epoch
	if p.health.Durable {
		p.health.LastDurable = p.health.LastSuccess
	}
	p.health.Error = ""
	p.health.Pending = p.epoch != epoch
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
	if p.mode == "journal-sync" {
		f, err := os.OpenFile(tmp, os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return err
	}
	if p.mode == "journal-sync" {
		return syncDirectory(filepath.Dir(p.path))
	}
	return nil
}

// Load reads the persisted state. The returned bool is false when the file
// does not exist yet (a fresh node), in which case the caller starts empty.
func (p *Persister) Load() (PersistState, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	data, err := os.ReadFile(p.path)
	if errors.Is(err, os.ErrNotExist) {
		state, found, err := p.replay(PersistState{})
		if err != nil {
			p.blocked = true
			p.healthMu.Lock()
			p.health.Error = err.Error()
			p.healthMu.Unlock()
		}
		return state, found, err
	}
	if err != nil {
		p.protect(err)
		return PersistState{}, false, err
	}
	var state PersistState
	if err := json.Unmarshal(data, &state); err != nil {
		p.protect(err)
		return PersistState{}, false, err
	}
	recovered, _, err := p.replay(state)
	if err != nil {
		p.blocked = true
		p.healthMu.Lock()
		p.health.Error = err.Error()
		p.healthMu.Unlock()
		return PersistState{}, false, err
	}
	return recovered, true, nil
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

// PersistenceHealth distinguishes in-memory success from a completed disk save.
type PersistenceHealth struct {
	Mode         string `json:"mode"`
	Durable      bool   `json:"durable"`
	LastDurable  int64  `json:"last_durable_ms"`
	Enabled      bool   `json:"enabled"`
	Pending      bool   `json:"pending"`
	LastSuccess  int64  `json:"last_success_ms"`
	Error        string `json:"error,omitempty"`
	RecoveryFile string `json:"recovery_file,omitempty"`
}

func (p *Persister) Health() PersistenceHealth {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	return p.health
}
func (p *Persister) MarkDirty() {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	p.epoch++
	p.health.Durable = false
	p.health.Pending = true
}
func (p *Persister) Epoch() uint64 { p.healthMu.Lock(); defer p.healthMu.Unlock(); return p.epoch }

// Refuse subsequent saves if the original cannot be retained.
func (p *Persister) protect(cause error) {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	p.health.Error = cause.Error()
	backup := fmt.Sprintf("%s.corrupt-%d", p.path, time.Now().UnixNano())
	info, err := os.Stat(p.path)
	if err == nil && info.Mode().IsRegular() {
		err = os.Rename(p.path, backup)
	} else if err == nil {
		err = fmt.Errorf("state is not a regular file")
	}
	if err != nil {
		p.blocked = true
		p.health.Error += "; recovery blocked: " + err.Error()
		return
	}
	p.health.RecoveryFile = backup
	if _, err := os.Stat(p.path + ".wal"); err == nil {
		if err := os.Rename(p.path+".wal", backup+".wal"); err != nil {
			p.blocked = true
			p.health.Error += "; journal retention failed: " + err.Error()
		}
	}
}
