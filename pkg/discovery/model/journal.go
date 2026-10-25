package model

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Journal records contain only changed keys plus node/chat metadata. Each line
// is checksummed; only an unterminated final line may be discarded on recovery.
type journalDelta struct {
	NodeID  string             `json:"node_id"`
	Clock   uint64             `json:"clock"`
	Chat    json.RawMessage    `json:"chat"`
	ChatSeq uint64             `json:"chat_seq"`
	Set     map[string]kvEntry `json:"set"`
	Remove  []string           `json:"remove"`
}
type journalRecord struct {
	Data   string `json:"data"`
	SHA256 string `json:"sha256"`
}

func (p *Persister) SetDurability(mode string) error {
	if mode != "snapshot" && mode != "journal" && mode != "journal-sync" {
		return fmt.Errorf("durability must be snapshot, journal, or journal-sync")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = mode
	p.healthMu.Lock()
	p.health.Mode = mode
	p.healthMu.Unlock()
	return nil
}
func (p *Persister) writeState(state PersistState) error {
	if p.mode == "" || p.mode == "snapshot" {
		// Before retiring a journal, include this save in it. A crash between
		// checkpoint rename and journal removal must not replay older state.
		if _, err := os.Stat(p.path + ".wal"); err == nil {
			oldMode := p.mode
			p.mode = "journal"
			err = p.writeState(state)
			p.mode = oldMode
			if err != nil {
				return err
			}
		}
		if err := p.writeAtomic(state); err != nil {
			return err
		}
		if err := os.Remove(p.path + ".wal"); err != nil && !os.IsNotExist(err) {
			return err
		}
		p.previous = state
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0700); err != nil {
		return err
	}
	// Create the compatibility snapshot before the first delta, never after it.
	if _, err := os.Stat(p.path); os.IsNotExist(err) {
		if err := p.writeAtomic(PersistState{Entries: map[string]kvEntry{}}); err != nil {
			return err
		}
	}
	delta := journalDelta{NodeID: state.NodeID, Clock: state.Clock, ChatSeq: state.ChatSeq, Set: map[string]kvEntry{}, Remove: []string{}}
	delta.Chat, _ = json.Marshal(state.Chat)
	for k, e := range state.Entries {
		if old, ok := p.previous.Entries[k]; !ok || old != e {
			delta.Set[k] = e
		}
	}
	for k := range p.previous.Entries {
		if _, ok := state.Entries[k]; !ok {
			delta.Remove = append(delta.Remove, k)
		}
	}
	data, err := json.Marshal(delta)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	line, err := json.Marshal(journalRecord{Data: string(data), SHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(p.path+".wal", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	n, err := f.Write(line)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if err == nil && p.mode == "journal-sync" {
		err = f.Sync()
	}
	if err != nil {
		f.Truncate(info.Size())
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if p.mode == "journal-sync" {
		if err = syncDirectory(filepath.Dir(p.path)); err != nil {
			return err
		}
	}
	p.previous = state
	p.journalRecords++
	if p.journalRecords >= 64 || info.Size()+int64(len(line)) > 16<<20 {
		if err := p.writeAtomic(state); err != nil {
			return err
		}
		f, err := os.OpenFile(p.path+".wal", os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return err
		}
		if p.mode == "journal-sync" {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		p.journalRecords = 0
	}
	return nil
}
func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (p *Persister) replay(state PersistState) (PersistState, bool, error) {
	f, err := os.OpenFile(p.path+".wal", os.O_RDWR, 0600)
	if os.IsNotExist(err) {
		p.previous = state
		return state, false, nil
	}
	if err != nil {
		return state, false, err
	}
	defer f.Close()
	if _, err := os.Stat(p.path); err != nil {
		return state, false, fmt.Errorf("journal checkpoint unavailable; recovery blocked: %w", err)
	}
	if state.Entries == nil {
		state.Entries = map[string]kvEntry{}
	}
	reader := bufio.NewReader(f)
	var offset int64
	found := false
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr == io.EOF {
			if len(line) > 0 {
				if err := f.Truncate(offset); err != nil {
					return state, found, err
				}
			}
			break
		}
		if readErr != nil {
			return state, found, readErr
		}
		var rec journalRecord
		if err := json.Unmarshal(bytes.TrimSpace(line), &rec); err != nil {
			return state, found, fmt.Errorf("journal record at %d: %w", offset, err)
		}
		sum := sha256.Sum256([]byte(rec.Data))
		if hex.EncodeToString(sum[:]) != rec.SHA256 {
			return state, found, fmt.Errorf("journal checksum mismatch at %d", offset)
		}
		var d journalDelta
		if err := json.Unmarshal([]byte(rec.Data), &d); err != nil {
			return state, found, err
		}
		state.NodeID = d.NodeID
		state.Clock = d.Clock
		state.ChatSeq = d.ChatSeq
		if err := json.Unmarshal(d.Chat, &state.Chat); err != nil {
			return state, found, err
		}
		for k, e := range d.Set {
			state.Entries[k] = e
		}
		for _, k := range d.Remove {
			delete(state.Entries, k)
		}
		found = true
		offset += int64(len(line))
		p.journalRecords++
	}
	p.previous = state
	return state, found, nil
}
