package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

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

// valueHash summarises the value for the consistency check, which compares
// replicas without shipping what they hold.
//
// A tombstone always hashes as empty. Apply stores whatever Value rides along
// with a remote delete, and nothing on the wire guarantees that field is
// cleared, so folding it in would let a harmless difference in an unused field
// read as divergence. What a tombstone means is carried by Deleted.
func (e kvEntry) valueHash() string {
	if e.Deleted || e.Value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(e.Value))
	return hex.EncodeToString(sum[:])
}

// PersistState is the on-disk form of a node: its identity, the Lamport clock,
// every entry (tombstones included) and the chat ring, so versions, write
// ordering and conversation all survive a restart.
type PersistState struct {
	NodeID  string             `json:"node_id,omitempty"`
	Clock   uint64             `json:"clock"`
	Entries map[string]kvEntry `json:"entries"`
	Chat    pb.ChatLog         `json:"chat,omitempty"`
	// ChatSeq is how many chat broadcasts this node has sent. It is persisted
	// for the same reason the node id is: a restart that began counting again
	// at 1 would make every peer read the whole conversation as a gap.
	ChatSeq uint64 `json:"chat_seq,omitempty"`
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

// Limits bound what a store will hold. Both are off (0) by default.
//
// A limit is a choice to stay up rather than to converge: an update refused for
// size is a deliberate divergence from the peer that sent it, and nothing on
// the wire reports that. Set them only when an unbounded store is the worse
// failure — and set the same values on every replica, or the cluster splits
// along whichever node was configured tightest.
type Limits struct {
	// MaxValueBytes refuses any value longer than this.
	MaxValueBytes int
	// MaxKeys refuses a write that would introduce a new key beyond this many
	// live keys. Updates to keys already held are always allowed, so a full
	// store still converges on the keys it has.
	MaxKeys int
}

// These say why a write was refused; a plain "false" is indistinguishable from
// a failed compare-and-swap, which is how a refused write came to be reported
// to HTTP clients as a 204.
var (
	ErrTooLarge        = errors.New("value exceeds the configured size limit")
	ErrTooManyKeys     = errors.New("store is at its configured key limit")
	ErrVersionMismatch = errors.New("key does not hold the expected version")
	ErrClockExhausted  = errors.New("version counter exhausted")
	// ErrUnshippable is not a configured limit but a fact about the protocol:
	// a value this large cannot be replicated by any path. The datagram
	// refuses it on size, the stream refuses the frame, and a state sync
	// builds that same frame — so accepting it would mean storing it here,
	// reporting success, and leaving it on one replica forever with nothing
	// able to repair it. Refusing the write is the only honest answer.
	ErrUnshippable = errors.New("value is too large to replicate to any peer")
	// ErrNotRepresentable is the same fact about a different limit of the
	// wire. Bodies are JSON, and every JSON encoder replaces a byte that is
	// not valid UTF-8 with U+FFFD — silently, with no error to notice.
	//
	// So a value of arbitrary bytes is held intact here and mangled on every
	// replica, under the *same version*: anti-entropy compares versions, finds
	// them equal, and is right to do nothing, which makes this the one kind of
	// divergence no repair path can reach. The consistency check does see it,
	// because the fingerprint folds a hash of the value, and then has nothing
	// to offer. Persistence marshals the same way, so a restart destroys the
	// last intact copy.
	//
	// A key is worse still. It is mangled the same way and the result is a
	// *different key*: the entry exists under one name here and another
	// everywhere else, and a later delete tombstones only one of them.
	//
	// Refusing is the honest answer for exactly the reason ErrUnshippable
	// refuses: storing it, reporting success and leaving the cluster
	// permanently split is the alternative. Callers with bytes to store
	// encode them — base64 — which is a decision for the caller to make
	// knowingly rather than for the encoder to make silently.
	ErrNotRepresentable = errors.New("not valid UTF-8, so it cannot survive the wire intact")
)

// representable reports whether a key and value can cross the wire unchanged.
//
// It is checked on every path, including deletes, because a delete carries a
// key and a mangled key tombstones something else. Remote updates are checked
// too, although a JSON decoder cannot produce an invalid string: that makes
// this an invariant of the store rather than of whoever remembered to call it,
// and the in-memory paths — imports, tests, a future transport — go through
// the same door.
func representable(key, value string, checkValue bool) error {
	if !utf8.ValidString(key) {
		return fmt.Errorf("key is %w", ErrNotRepresentable)
	}
	if checkValue && !utf8.ValidString(value) {
		return fmt.Errorf("value is %w", ErrNotRepresentable)
	}
	return nil
}

// WriteOptions modifies a write. The zero value is an unconditional write with
// no expiry.
type WriteOptions struct {
	ExpectMissing bool // exact absence, including absence of a tombstone (restore guard)
	// AfterCounter stamps explicit repairs after the peer clocks observed before applying.
	AfterCounter uint64
	// ExpectUpdate guards a repair against any change since its preview, including tombstones.
	ExpectUpdate *pb.KVUpdate
	// ExpiresAt is an absolute unix second; 0 means the key never expires.
	ExpiresAt int64
	// Expect turns the write into a compare-and-swap: it only lands if the
	// current version matches. A zero Version requires the key to be absent
	// (or tombstoned/expired) on this replica. It does not coordinate peers.
	Expect *pb.Version
}

// KVEventSource says what caused a mutation, which is the first thing a
// watcher wants to know: a local write is this process's own doing, a remote
// one arrived from a peer, and an expiry happened on every replica at once
// without anything being sent.
type KVEventSource string

const (
	KVSourceLocal  KVEventSource = "local"
	KVSourceRemote KVEventSource = "remote"
	KVSourceExpiry KVEventSource = "expiry"
)

// KVEvent is one mutation, as it would be replicated, plus how it arrived.
//
// Tombstone GC deliberately raises nothing. It reclaims a key that has already
// been invisible for as long as the configured age, so an event for it would
// tell a watcher a key was "deleted" long after the delete it is still holding.
type KVEvent struct {
	ID     string        `json:"id,omitempty"`
	Source KVEventSource `json:"source"`
	Update pb.KVUpdate   `json:"update"`
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
	onMutate func([]KVEvent)
	limits   Limits

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

// SetLimits bounds what the store will accept, from a local writer or a peer.
func (kv *KVStore) SetLimits(l Limits) {
	kv.mu.Lock()
	kv.limits = l
	kv.mu.Unlock()
}

// Limits reports the configured bounds.
func (kv *KVStore) Limits() Limits {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	return kv.limits
}

// admitLocked reports why a value of this size for this key cannot be stored.
// Callers hold the lock.
func (kv *KVStore) admitLocked(key, value string, now int64) error {
	// Measured as the value will be written on the wire, not as it sits in
	// memory: a byte the encoder has to escape costs up to six times its own
	// length, and a raw length check would wave such a value through to strand
	// itself.
	//
	// The cheap length test first, because the exact one walks the value and
	// almost every value is nowhere near: six is the worst any single byte can
	// cost (an ASCII control, written \u0001), so nothing shorter than a sixth
	// of the ceiling can exceed it however badly it escapes. That bound is
	// pinned by a test rather than trusted, because it is the encoder's number
	// and not ours.
	//
	// EscapedLen counts the surrounding quotes; the ceiling is about the value
	// itself, so that a run of plain text exactly MaxReplicableValueBytes long
	// is accepted rather than refused for two characters it did not write.
	// The reserve covers the quotes.
	if len(value) > pb.MaxReplicableValueBytes/6 &&
		pb.EscapedLen(value)-2 > pb.MaxReplicableValueBytes {
		return ErrUnshippable
	}
	if kv.limits.MaxValueBytes > 0 && len(value) > kv.limits.MaxValueBytes {
		return ErrTooLarge
	}
	if kv.limits.MaxKeys <= 0 {
		return nil
	}
	if e, ok := kv.store[key]; ok && e.visible(now) {
		return nil // already counted; an update never grows the keyspace
	}
	live := 0
	for _, e := range kv.store {
		if e.visible(now) {
			live++
		}
	}
	if live >= kv.limits.MaxKeys {
		return ErrTooManyKeys
	}
	return nil
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

// SetOnMutate registers a callback invoked (outside the lock) with every
// mutation the store accepts, whatever caused it.
//
// It lives here rather than at the six call sites that mutate a store because
// a watch that is assembled by remembering to publish is a watch that is
// complete until the next mutation path is added. Everything that changes an
// entry goes through this file; nothing that changes an entry can miss this.
func (kv *KVStore) SetOnMutate(f func([]KVEvent)) {
	kv.mu.Lock()
	kv.onMutate = f
	kv.mu.Unlock()
}

// mutated captures the mutation callback under the lock, for the caller to
// invoke after unlocking — the same contract as changed.
func (kv *KVStore) mutated() func([]KVEvent) {
	return kv.onMutate
}

// emit delivers events to a captured callback, skipping the work entirely when
// nobody registered one.
func emit(f func([]KVEvent), events []KVEvent) {
	if f == nil || len(events) == 0 {
		return
	}
	f(events)
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
func (kv *KVStore) Write(key, value string, opt WriteOptions) (pb.KVUpdate, error) {
	return kv.mutate(key, value, false, opt)
}

// Remove records a local tombstone and returns the versioned update to
// replicate.
func (kv *KVStore) Remove(key string, opt WriteOptions) (pb.KVUpdate, error) {
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

func (kv *KVStore) mutate(key, value string, remove bool, opt WriteOptions) (pb.KVUpdate, error) {
	// Before the lock: this reads nothing the store owns, and a caller handing
	// over bytes that cannot be replicated should not have to wait on writers
	// to be told so.
	if err := representable(key, value, !remove); err != nil {
		return pb.KVUpdate{}, err
	}
	kv.mu.Lock()
	now := kv.now().Unix()
	if opt.ExpectMissing {
		if _, ok := kv.store[key]; ok {
			kv.mu.Unlock()
			return pb.KVUpdate{}, ErrVersionMismatch
		}
	}
	if opt.ExpectUpdate != nil {
		cur, ok := kv.store[key]
		expected := opt.ExpectUpdate
		if !ok || !cur.Version.Equal(expected.Version) || cur.Deleted != (expected.Action == pb.KVDelete) || cur.Value != expected.Value || cur.ExpiresAt != expected.ExpiresAt {
			kv.mu.Unlock()
			return pb.KVUpdate{}, ErrVersionMismatch
		}
	}
	if !remove {
		if err := kv.admitLocked(key, value, now); err != nil {
			kv.mu.Unlock()
			return pb.KVUpdate{}, err
		}
	}
	if opt.Expect != nil {
		cur, ok := kv.store[key]
		// A caller that expects "absent" is satisfied by a key that is missing,
		// tombstoned or expired — all three read as absent through Get.
		if !ok || !cur.visible(now) {
			if !opt.Expect.Zero() {
				kv.mu.Unlock()
				return pb.KVUpdate{}, ErrVersionMismatch
			}
		} else if !cur.Version.Equal(*opt.Expect) {
			kv.mu.Unlock()
			return pb.KVUpdate{}, ErrVersionMismatch
		}
	}
	nextClock := kv.clock
	if opt.AfterCounter > nextClock {
		nextClock = opt.AfterCounter
	}
	if nextClock == ^uint64(0) {
		kv.mu.Unlock()
		return pb.KVUpdate{}, ErrClockExhausted
	}
	kv.clock = nextClock + 1
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
	watch := kv.mutated()
	kv.mu.Unlock()
	if cb != nil {
		cb()
	}
	action := pb.KVSet
	if remove {
		action = pb.KVDelete
	}
	u := pb.KVUpdate{
		Action:    action,
		Key:       key,
		Value:     e.Value,
		Version:   ver,
		ExpiresAt: e.ExpiresAt,
		DeletedAt: e.DeletedAt,
	}
	emit(watch, []KVEvent{{Source: KVSourceLocal, Update: u}})
	return u, nil
}

// Apply merges a remote update under last-write-wins and reports whether it
// changed local state. The Lamport clock is advanced past any counter seen so
// this node's later writes sort after it.
func (kv *KVStore) Apply(u pb.KVUpdate) bool {
	applied, _ := kv.ApplyWithReason(u)
	return applied
}

// ApplyWithReason is Apply, and additionally says why an update did not land.
//
// A bare bool collapses two outcomes that need opposite responses. "Not
// newer" is the ordinary result of last-write-wins and means the cluster is
// working; a refusal from admitLocked means this node will not hold the entry
// at all, so it can never converge with the peer that sent it and an operator
// has to raise a limit or free a key. Callers that reported both as "stale"
// counted a store at its limit as healthy, which is the one state where the
// counters most need to say otherwise.
//
// A nil error with applied=false is the genuinely-stale case.
func (kv *KVStore) ApplyWithReason(u pb.KVUpdate) (bool, error) {
	if err := representable(u.Key, u.Value, u.Action != pb.KVDelete); err != nil {
		return false, err
	}
	kv.mu.Lock()
	if u.Version.Counter > kv.clock {
		kv.clock = u.Version.Counter
	}
	if u.Action != pb.KVDelete {
		if err := kv.admitLocked(u.Key, u.Value, kv.now().Unix()); err != nil {
			kv.mu.Unlock()
			return false, err
		}
	}
	if cur, ok := kv.store[u.Key]; ok && !u.Version.Newer(cur.Version) {
		kv.mu.Unlock()
		return false, nil
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
	watch := kv.mutated()
	kv.mu.Unlock()
	if cb != nil {
		cb()
	}
	emit(watch, []KVEvent{{Source: KVSourceRemote, Update: entryUpdate(u.Key, e)}})
	return true, nil
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

// Fingerprint summarises the whole store as a fixed set of bucket digests.
//
// A key's bucket comes from the hash of its **name alone**, and what is folded
// into that bucket is the hash of the whole entry. Both halves matter:
//
//   - Bucketing on the name means a key stays in the same bucket however its
//     value changes, so a bucket that differs names a stable region of the
//     keyspace — which is the only thing that makes "they differ in bucket 7"
//     more useful than "they differ". Bucketing on the whole entry (as this
//     did first) relocates a key whenever it is written, so a single stale
//     value lights up two buckets and neither corresponds to anything.
//   - Folding with XOR means a bucket does not depend on the order entries were
//     learned in, so two replicas that took the same writes by different routes
//     agree.
//
// Tombstones are included: two replicas that disagree about whether a key is
// deleted have diverged just as much as two that disagree about its value.
func (kv *KVStore) Fingerprint(buckets int) pb.FingerprintReply {
	return kv.FingerprintVersion(buckets, 0)
}

// FingerprintVersion preserves legacy digests unless version 2 is requested.
func (kv *KVStore) FingerprintVersion(buckets, version int) pb.FingerprintReply {
	if buckets <= 0 {
		buckets = pb.FingerprintBuckets
	}
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	folds := make([][sha256.Size]byte, buckets)
	keys, tombstones := 0, 0
	now := kv.now().Unix()
	for k, e := range kv.store {
		if e.Deleted {
			tombstones++
		} else if e.visible(now) {
			keys++
		}
		idx := bucketOf(k, buckets)

		h := sha256.New()
		h.Write([]byte(k))
		h.Write([]byte{0})
		fmt.Fprintf(h, "%d/%s/%t", e.Version.Counter, e.Version.Node, e.Deleted)
		h.Write([]byte{0})
		h.Write([]byte(e.valueHash()))
		if version == 2 {
			fmt.Fprintf(h, "\x00%d", e.ExpiresAt)
		}
		var sum [sha256.Size]byte
		copy(sum[:], h.Sum(nil))

		for i := range folds[idx] {
			folds[idx][i] ^= sum[i]
		}
	}

	out := make([]string, buckets)
	for i, f := range folds {
		out[i] = hex.EncodeToString(f[:])
	}
	return pb.FingerprintReply{
		Keys:       keys,
		Tombstones: tombstones,
		Clock:      kv.clock,
		Buckets:    out,
	}
}

// bucketOf is which bucket a key name folds into. The one definition, so a
// digest and a listing can never disagree about where a key lives.
func bucketOf(key string, buckets int) int {
	nameHash := sha256.Sum256([]byte(key))
	return int(binary.BigEndian.Uint32(nameHash[:4]) % uint32(buckets))
}

// BucketEntries lists what this store holds in the named buckets, sorted by
// key, tombstones included.
//
// This is the second half of a consistency check: the digests say which
// sixteenth of the keyspace two replicas disagree about, and this says which
// keys. Bounded by the buckets asked for, so naming the divergence costs a
// slice of the store rather than all of it — a check that had to ship the
// whole keyspace to be useful would not be run.
func (kv *KVStore) BucketEntries(buckets int, want []int) []pb.KeyVersion {
	if buckets <= 0 {
		buckets = pb.FingerprintBuckets
	}
	if len(want) == 0 {
		return nil
	}
	wanted := make(map[int]bool, len(want))
	for _, b := range want {
		wanted[b] = true
	}

	kv.mu.RLock()
	out := make([]pb.KeyVersion, 0, 16)
	for k, e := range kv.store {
		if !wanted[bucketOf(k, buckets)] {
			continue
		}
		out = append(out, pb.KeyVersion{
			Key: k, Version: e.Version, Deleted: e.Deleted, ValueHash: e.valueHash(), ExpiresAt: e.ExpiresAt,
		})
	}
	kv.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
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
	var events []KVEvent
	for k, e := range kv.store {
		if e.Deleted || !e.expired(now) {
			continue
		}
		e.Deleted = true
		e.Value = ""
		e.DeletedAt = now
		kv.store[k] = e
		n++
		if kv.onMutate != nil {
			events = append(events, KVEvent{Source: KVSourceExpiry, Update: entryUpdate(k, e)})
		}
	}
	var cb func()
	var watch func([]KVEvent)
	if n > 0 {
		cb = kv.changed()
		watch = kv.mutated()
	}
	kv.mu.Unlock()
	if cb != nil {
		cb()
	}
	emit(watch, events)
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

// Clock reports the Lamport counter, which is the one number that says whether
// a node is keeping up with the writes around it.
func (kv *KVStore) Clock() uint64 {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	return kv.clock
}

// ValueBytes totals the live values held, which is the store's real footprint
// rather than its key count.
func (kv *KVStore) ValueBytes() int {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	now := kv.now().Unix()
	n := 0
	for _, e := range kv.store {
		if e.visible(now) {
			n += len(e.Value)
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
