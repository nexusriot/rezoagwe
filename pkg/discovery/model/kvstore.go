package model

import (
	"sort"
	"sync"
	"time"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// historyPerKey bounds the in-memory version history kept for each key. History
// is a debugging aid, not replicated state, so it is neither persisted nor
// synced.
const historyPerKey = 20

// kvEntry is a versioned value. Deleted keys are kept as tombstones (with the
// version of the delete) so a stale set can't resurrect a concurrently-deleted
// key.
type kvEntry struct {
	Value   string     `json:"value"`
	Version pb.Version `json:"version"`
	Deleted bool       `json:"deleted,omitempty"`
	// ExpiresAt is a unix second at which this entry becomes invisible on every
	// replica. Expiry is deterministic, so it needs no replication of its own.
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// DeletedAt is when the tombstone appeared, used only by tombstone GC.
	DeletedAt int64 `json:"deleted_at,omitempty"`
}

func (e kvEntry) expired(now int64) bool {
	return e.ExpiresAt != 0 && now >= e.ExpiresAt
}

// visible reports whether the entry should be readable at now.
func (e kvEntry) visible(now int64) bool {
	return !e.Deleted && !e.expired(now)
}

// PersistState is the on-disk form of a node: its identity, the Lamport clock,
// every entry (tombstones included) and the chat ring, so versions, write
// ordering and conversation all survive a restart.
type PersistState struct {
	NodeID  string             `json:"node_id,omitempty"`
	Clock   uint64             `json:"clock"`
	Entries map[string]kvEntry `json:"entries"`
	Chat    pb.ChatLog         `json:"chat,omitempty"`
}

// Entry is the exported view of a stored key.
type Entry struct {
	Key       string
	Value     string
	Version   pb.Version
	Deleted   bool
	ExpiresAt int64
}

// HistoryEntry is one observed version of a key.
type HistoryEntry struct {
	Version pb.Version
	Value   string
	Deleted bool
	// Local distinguishes a write made on this node from one merged in from a
	// peer, which is the first thing you want to know when a value surprises
	// you.
	Local bool
	At    time.Time
}

// WriteOptions modifies a write. The zero value is an unconditional write with
// no expiry.
type WriteOptions struct {
	// ExpiresAt is an absolute unix second; 0 means the key never expires.
	ExpiresAt int64
	// Expect turns the write into a compare-and-swap: it only lands if the
	// current version matches. A zero Version requires the key to be absent
	// (or tombstoned/expired), which is what makes "claim this lock" work.
	Expect *pb.Version
}

// KVStore is a version-aware KV store with last-write-wins merge. Every local
// mutation stamps a per-key pb.Version (a Lamport counter with this node's id
// as tiebreak); an incoming update is applied only when its version is newer
// than the one held.
type KVStore struct {
	mu       sync.RWMutex
	node     string // this node's stable id, used as the version tiebreaker
	clock    uint64 // Lamport counter
	store    map[string]kvEntry
	history  map[string][]HistoryEntry
	onChange func()

	// now is overridable so expiry and GC can be tested without sleeping.
	now func() time.Time
}

func NewKVStore(node string) *KVStore {
	return &KVStore{
		node:    node,
		store:   make(map[string]kvEntry),
		history: make(map[string][]HistoryEntry),
		now:     time.Now,
	}
}

// SetClock replaces the store's time source. Tests use it to drive TTL expiry
// and tombstone GC deterministically.
func (kv *KVStore) SetClock(f func() time.Time) {
	kv.mu.Lock()
	kv.now = f
	kv.mu.Unlock()
}

// SetOnChange registers a callback invoked (outside the lock) after every
// mutation. The callback is what drives persistence; it pulls the state itself
// so the store does not have to know what persistence looks like.
func (kv *KVStore) SetOnChange(f func()) {
	kv.mu.Lock()
	kv.onChange = f
	kv.mu.Unlock()
}

func (kv *KVStore) NodeID() string {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	return kv.node
}

// LoadState installs a persisted state without firing onChange (used at
// startup, before the persister is wired).
func (kv *KVStore) LoadState(state PersistState) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.clock = state.Clock
	kv.store = make(map[string]kvEntry, len(state.Entries))
	for k, e := range state.Entries {
		kv.store[k] = e
	}
}

// State returns the persistable snapshot of the store.
func (kv *KVStore) State() PersistState {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	return kv.stateLocked()
}

func (kv *KVStore) stateLocked() PersistState {
	entries := make(map[string]kvEntry, len(kv.store))
	for k, e := range kv.store {
		entries[k] = e
	}
	return PersistState{Clock: kv.clock, Entries: entries}
}

// changed captures the callback while holding the lock; callers invoke the
// returned function after unlocking so persistence I/O never blocks writers.
func (kv *KVStore) changed() func() {
	return kv.onChange
}

func (kv *KVStore) recordLocked(key string, e kvEntry, local bool) {
	h := append(kv.history[key], HistoryEntry{
		Version: e.Version,
		Value:   e.Value,
		Deleted: e.Deleted,
		Local:   local,
		At:      kv.now(),
	})
	if len(h) > historyPerKey {
		h = h[len(h)-historyPerKey:]
	}
	kv.history[key] = h
}

// Write records a local set and returns the versioned update to replicate.
// The bool is false only when a compare-and-swap precondition failed.
func (kv *KVStore) Write(key, value string, opt WriteOptions) (pb.KVUpdate, bool) {
	return kv.mutate(key, value, false, opt)
}

// Remove records a local tombstone and returns the versioned update to
// replicate.
func (kv *KVStore) Remove(key string, opt WriteOptions) (pb.KVUpdate, bool) {
	return kv.mutate(key, "", true, opt)
}

// Set is an unconditional write with no expiry.
func (kv *KVStore) Set(key, value string) pb.KVUpdate {
	u, _ := kv.Write(key, value, WriteOptions{})
	return u
}

// Delete is an unconditional tombstone.
func (kv *KVStore) Delete(key string) pb.KVUpdate {
	u, _ := kv.Remove(key, WriteOptions{})
	return u
}

func (kv *KVStore) mutate(key, value string, remove bool, opt WriteOptions) (pb.KVUpdate, bool) {
	kv.mu.Lock()
	now := kv.now().Unix()
	if opt.Expect != nil {
		cur, ok := kv.store[key]
		// A caller that expects "absent" is satisfied by a key that is missing,
		// tombstoned or expired — all three read as absent through Get.
		if !ok || !cur.visible(now) {
			if !opt.Expect.Zero() {
				kv.mu.Unlock()
				return pb.KVUpdate{}, false
			}
		} else if !cur.Version.Equal(*opt.Expect) {
			kv.mu.Unlock()
			return pb.KVUpdate{}, false
		}
	}
	kv.clock++
	ver := pb.Version{Counter: kv.clock, Node: kv.node}
	e := kvEntry{Value: value, Version: ver, Deleted: remove, ExpiresAt: opt.ExpiresAt}
	if remove {
		e.Value = ""
		e.ExpiresAt = 0
		e.DeletedAt = now
	}
	kv.store[key] = e
	kv.recordLocked(key, e, true)
	cb := kv.changed()
	kv.mu.Unlock()
	if cb != nil {
		cb()
	}
	action := pb.KVSet
	if remove {
		action = pb.KVDelete
	}
	return pb.KVUpdate{
		Action:    action,
		Key:       key,
		Value:     e.Value,
		Version:   ver,
		ExpiresAt: e.ExpiresAt,
		DeletedAt: e.DeletedAt,
	}, true
}

// Apply merges a remote update under last-write-wins and reports whether it
// changed local state. The Lamport clock is advanced past any counter seen so
// this node's later writes sort after it.
func (kv *KVStore) Apply(u pb.KVUpdate) bool {
	kv.mu.Lock()
	if u.Version.Counter > kv.clock {
		kv.clock = u.Version.Counter
	}
	if cur, ok := kv.store[u.Key]; ok && !u.Version.Newer(cur.Version) {
		kv.mu.Unlock()
		return false
	}
	e := kvEntry{
		Value:     u.Value,
		Version:   u.Version,
		Deleted:   u.Action == pb.KVDelete,
		ExpiresAt: u.ExpiresAt,
		DeletedAt: u.DeletedAt,
	}
	if e.Deleted && e.DeletedAt == 0 {
		e.DeletedAt = kv.now().Unix()
	}
	kv.store[u.Key] = e
	kv.recordLocked(u.Key, e, false)
	cb := kv.changed()
	kv.mu.Unlock()
	if cb != nil {
		cb()
	}
	return true
}

func (kv *KVStore) Get(key string) (string, bool) {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	e, ok := kv.store[key]
	if !ok || !e.visible(kv.now().Unix()) {
		return "", false
	}
	return e.Value, true
}

// GetEntry returns the full entry for a key, which callers need to build a
// compare-and-swap precondition.
func (kv *KVStore) GetEntry(key string) (Entry, bool) {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	e, ok := kv.store[key]
	if !ok || !e.visible(kv.now().Unix()) {
		return Entry{}, false
	}
	return Entry{
		Key:       key,
		Value:     e.Value,
		Version:   e.Version,
		ExpiresAt: e.ExpiresAt,
	}, true
}

// Snapshot returns a copy of the live (non-tombstoned, non-expired) values.
func (kv *KVStore) Snapshot() map[string]string {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	now := kv.now().Unix()
	out := make(map[string]string, len(kv.store))
	for k, e := range kv.store {
		if e.visible(now) {
			out[k] = e.Value
		}
	}
	return out
}

// Entries returns every live key with its version and expiry, sorted by key.
func (kv *KVStore) Entries() []Entry {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	now := kv.now().Unix()
	out := make([]Entry, 0, len(kv.store))
	for k, e := range kv.store {
		if !e.visible(now) {
			continue
		}
		out = append(out, Entry{Key: k, Value: e.Value, Version: e.Version, ExpiresAt: e.ExpiresAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Updates returns every entry (tombstones included) as versioned updates, for
// shipping a full snapshot to a joining peer to merge.
func (kv *KVStore) Updates() []pb.KVUpdate {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	out := make([]pb.KVUpdate, 0, len(kv.store))
	for k, e := range kv.store {
		out = append(out, entryUpdate(k, e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// UpdatesFor returns updates for the named keys that this node actually holds,
// which is how a pull request is answered.
func (kv *KVStore) UpdatesFor(keys []string) []pb.KVUpdate {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	out := make([]pb.KVUpdate, 0, len(keys))
	for _, k := range keys {
		if e, ok := kv.store[k]; ok {
			out = append(out, entryUpdate(k, e))
		}
	}
	return out
}

func entryUpdate(key string, e kvEntry) pb.KVUpdate {
	action := pb.KVSet
	if e.Deleted {
		action = pb.KVDelete
	}
	return pb.KVUpdate{
		Action:    action,
		Key:       key,
		Value:     e.Value,
		Version:   e.Version,
		ExpiresAt: e.ExpiresAt,
		DeletedAt: e.DeletedAt,
	}
}

// Digest advertises what this node holds for up to limit keys starting after
// the cursor, and returns the key range (lo, hi) the digest covers — lo and hi
// both exclusive. An empty hi means the digest reaches the end of the keyspace,
// and the caller should restart its cursor.
//
// The range is the point of the whole exchange: without it the receiver could
// not tell "the sender has nothing for this key" from "the key was outside
// this batch", and keys missing entirely from the sender would never be
// repaired.
func (kv *KVStore) Digest(after string, limit int) pb.Digest {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	keys := make([]string, 0, len(kv.store))
	for k := range kv.store {
		if k > after || after == "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	d := pb.Digest{Lo: after}
	if len(keys) > limit {
		keys = keys[:limit]
		// hi is exclusive, so the next batch starts exactly where this stops.
		d.Hi = keys[len(keys)-1] + "\x00"
	}
	for _, k := range keys {
		e := kv.store[k]
		d.Entries = append(d.Entries, pb.KeyVersion{Key: k, Version: e.Version, Deleted: e.Deleted})
	}
	return d
}

// inDigestRange reports whether key falls in the range a digest covers. Lo is
// exclusive because it is the sender's cursor — the last key it already
// advertised in the previous round.
func inDigestRange(key, lo, hi string) bool {
	if lo != "" && key <= lo {
		return false
	}
	return hi == "" || key < hi
}

// Reconcile compares a peer's digest against local state and returns what to
// push back (entries where this node is newer, or that the peer is missing
// entirely) and what to pull (entries where the peer is newer, or that this
// node has never seen).
func (kv *KVStore) Reconcile(d pb.Digest, maxPush, maxPull int) (push []pb.KVUpdate, pull []string) {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	seen := make(map[string]struct{}, len(d.Entries))
	for _, adv := range d.Entries {
		seen[adv.Key] = struct{}{}
		local, ok := kv.store[adv.Key]
		switch {
		case !ok:
			pull = append(pull, adv.Key)
		case local.Version.Newer(adv.Version):
			push = append(push, entryUpdate(adv.Key, local))
		case adv.Version.Newer(local.Version):
			pull = append(pull, adv.Key)
		}
	}
	for k, e := range kv.store {
		if _, ok := seen[k]; ok {
			continue
		}
		if inDigestRange(k, d.Lo, d.Hi) {
			push = append(push, entryUpdate(k, e))
		}
	}
	sort.Slice(push, func(i, j int) bool { return push[i].Key < push[j].Key })
	sort.Strings(pull)
	if len(push) > maxPush {
		push = push[:maxPush]
	}
	if len(pull) > maxPull {
		pull = pull[:maxPull]
	}
	return push, pull
}

// History returns the recorded versions of a key, oldest first.
func (kv *KVStore) History(key string) []HistoryEntry {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	h := kv.history[key]
	out := make([]HistoryEntry, len(h))
	copy(out, h)
	return out
}

// SweepExpired converts entries whose TTL has passed into tombstones, keeping
// each entry's existing version.
//
// Keeping the version is what makes expiry safe: bumping the Lamport clock here
// would let a sweep outrank a concurrent legitimate write to the same key, and
// every replica sweeps independently. Since expiry is deterministic, replicas
// reach the same state without exchanging a single message.
func (kv *KVStore) SweepExpired() int {
	kv.mu.Lock()
	now := kv.now().Unix()
	n := 0
	for k, e := range kv.store {
		if e.Deleted || !e.expired(now) {
			continue
		}
		e.Deleted = true
		e.Value = ""
		e.DeletedAt = now
		kv.store[k] = e
		n++
	}
	var cb func()
	if n > 0 {
		cb = kv.changed()
	}
	kv.mu.Unlock()
	if cb != nil {
		cb()
	}
	return n
}

// GCTombstones drops tombstones older than the given age.
//
// This is the one operation that can resurrect a key: a peer that never saw the
// delete and still holds the old value will re-push it after the tombstone is
// gone. Anti-entropy makes that unlikely rather than impossible, so the default
// age is long — it has to exceed the longest partition you expect to heal.
func (kv *KVStore) GCTombstones(olderThan time.Duration) int {
	if olderThan <= 0 {
		return 0
	}
	kv.mu.Lock()
	cutoff := kv.now().Add(-olderThan).Unix()
	n := 0
	for k, e := range kv.store {
		if e.Deleted && e.DeletedAt != 0 && e.DeletedAt < cutoff {
			delete(kv.store, k)
			delete(kv.history, k)
			n++
		}
	}
	var cb func()
	if n > 0 {
		cb = kv.changed()
	}
	kv.mu.Unlock()
	if cb != nil {
		cb()
	}
	return n
}

// Len reports the number of live keys.
func (kv *KVStore) Len() int {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	now := kv.now().Unix()
	n := 0
	for _, e := range kv.store {
		if e.visible(now) {
			n++
		}
	}
	return n
}

// Tombstones reports how many tombstones are being retained.
func (kv *KVStore) Tombstones() int {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	n := 0
	for _, e := range kv.store {
		if e.Deleted {
			n++
		}
	}
	return n
}
