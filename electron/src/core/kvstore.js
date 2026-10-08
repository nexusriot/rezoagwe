'use strict';

const { createHash } = require('crypto');

const {
  KVAction, version, versionNewer, versionEqual, versionZero, FINGERPRINT_BUCKETS,
  MAX_REPLICABLE_VALUE_BYTES, escapedLen,
} = require('../proto/wire');

/**
 * Why a write was refused. A bare null says only that it did not land, which
 * reads the same for a lost compare-and-swap and a value no peer could ever
 * receive — and those need opposite answers.
 */
const REFUSAL = Object.freeze({
  UNSHIPPABLE: 'unshippable',
  TOO_LARGE: 'too-large',
  TOO_MANY_KEYS: 'too-many-keys',
  VERSION_MISMATCH: 'version-mismatch',
});

/**
 * Which bucket a key name folds into. The one definition, so a digest and a
 * listing can never disagree about where a key lives.
 */
function bucketOf(key, buckets) {
  return createHash('sha256').update(key, 'utf8').digest().readUInt32BE(0) % buckets;
}

/** How many versions of a key are remembered. History is a debugging aid: neither persisted nor synced. */
const HISTORY_PER_KEY = 20;

/**
 * Version-aware KV store with last-write-wins merge — the JavaScript twin of the
 * Go model.KVStore and the Kotlin KvStore. All three have to agree on every rule
 * here, or two replicas of the same cluster silently disagree about a key.
 *
 * Entries are { value, version, deleted, expiresAt, deletedAt }; a delete is a
 * tombstone carrying the version of the delete, so a stale set cannot resurrect
 * a concurrently deleted key.
 */
class KvStore {
  constructor(nodeId, opts = {}) {
    this.nodeId = nodeId;
    this.nowSec = opts.nowSec || (() => Math.floor(Date.now() / 1000));
    // Bounds on what the store will hold, from a local writer or a peer. Off by
    // default: a limit is a choice to stay up rather than to converge, since an
    // update refused for size is a deliberate divergence nothing on the wire
    // reports. Set the same values on every replica or the cluster splits along
    // whichever node was configured tightest.
    this.limits = { maxValueBytes: 0, maxKeys: 0, ...(opts.limits || {}) };
    this.clock = 0;
    this.store = new Map();
    this.history = new Map();
    this.onChange = null;
  }

  setOnChange(f) {
    this.onChange = f;
  }

  /** Installs a persisted state without firing onChange (startup, before the persister is wired). */
  loadState(clock, entries) {
    this.clock = Number(clock) || 0;
    this.store.clear();
    for (const [k, e] of Object.entries(entries || {})) {
      if (!e || typeof e !== 'object') continue;
      this.store.set(k, {
        value: typeof e.value === 'string' ? e.value : '',
        version: version(e.version && e.version.counter, e.version && e.version.node),
        deleted: e.deleted === true,
        expiresAt: Number(e.expires_at) || 0,
        deletedAt: Number(e.deleted_at) || 0,
      });
    }
  }

  /** The persistable snapshot: the clock and every entry, in the Go file layout. */
  snapshotState() {
    const entries = {};
    for (const [k, e] of this.store) {
      const out = { value: e.value, version: { counter: e.version.counter, node: e.version.node } };
      if (e.deleted) out.deleted = true;
      if (e.expiresAt) out.expires_at = e.expiresAt;
      if (e.deletedAt) out.deleted_at = e.deletedAt;
      entries[k] = out;
    }
    return { clock: this.clock, entries };
  }

  setLimits(limits) {
    this.limits = { maxValueBytes: 0, maxKeys: 0, ...(limits || {}) };
  }

  /**
   * Why a value may not be stored under this key, or null when it may.
   *
   * The reason matters: "refused" reads the same for a lost race and a value
   * that will never fit, and the caller owes the user different answers — and
   * for a while owed them none at all, since a refusal came back as success.
   */
  admit(key, value, nowSec = this.nowSec()) {
    // Measured as the value will be written on the wire, not as it sits in
    // memory: an unpaired surrogate is escaped to six characters, and a raw
    // length check would wave it through to strand itself.
    //
    // The cheap test first, because the exact one walks the value and almost
    // every value is nowhere near: nothing shorter than a sixth of the ceiling
    // can exceed it however badly it escapes. escapedLen counts the quotes,
    // which the reserve covers, so they come back off here.
    if (value.length > MAX_REPLICABLE_VALUE_BYTES / 6
      && escapedLen(value) - 2 > MAX_REPLICABLE_VALUE_BYTES) {
      return REFUSAL.UNSHIPPABLE;
    }
    const { maxValueBytes, maxKeys } = this.limits;
    if (maxValueBytes > 0 && Buffer.byteLength(value, 'utf8') > maxValueBytes) return REFUSAL.TOO_LARGE;
    if (maxKeys <= 0) return null;
    const existing = this.store.get(key);
    if (existing && KvStore.visible(existing, nowSec)) return null; // an update never grows the keyspace
    let live = 0;
    for (const e of this.store.values()) if (KvStore.visible(e, nowSec)) live++;
    return live < maxKeys ? null : REFUSAL.TOO_MANY_KEYS;
  }

  /** Whether a value of this size may be stored under this key. */
  admits(key, value, nowSec) {
    return this.admit(key, value, nowSec) === null;
  }

  /**
   * The (key, version) pairs held in the named buckets, sorted by key,
   * tombstones included.
   *
   * This is the second half of a consistency check: the digests say which
   * sixteenth of the keyspace two replicas disagree about, and this says which
   * keys. Bounded by the buckets asked for, so naming the divergence costs a
   * slice of the store rather than all of it.
   */
  bucketEntries(buckets = FINGERPRINT_BUCKETS, want = []) {
    if (!want.length) return [];
    const wanted = new Set(want);
    const out = [];
    for (const [k, e] of this.store) {
      if (!wanted.has(bucketOf(k, buckets))) continue;
      out.push({ key: k, version: e.version, deleted: e.deleted });
    }
    out.sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
    return out;
  }

  static expired(e, nowSec) {
    return e.expiresAt !== 0 && nowSec >= e.expiresAt;
  }

  static visible(e, nowSec) {
    return !e.deleted && !KvStore.expired(e, nowSec);
  }

  record(key, e, local) {
    let list = this.history.get(key);
    if (!list) {
      list = [];
      this.history.set(key, list);
    }
    list.push({ version: e.version, value: e.value, deleted: e.deleted, local, at: this.nowSec() });
    while (list.length > HISTORY_PER_KEY) list.shift();
  }

  changed() {
    if (this.onChange) this.onChange();
  }

  /** Records a local set and returns the update to replicate. Null means a CAS precondition failed. */
  write(key, value, opt = {}) {
    return this.mutate(key, value, false, opt);
  }

  /** Records a local tombstone. Null means a CAS precondition failed. */
  remove(key, opt = {}) {
    return this.mutate(key, '', true, opt);
  }

  mutate(key, value, remove, opt) {
    const now = this.nowSec();
    if (!remove && !this.admits(key, value, now)) return null;
    if (opt.expect) {
      const current = this.store.get(key);
      if (!current || !KvStore.visible(current, now)) {
        // Missing, tombstoned and expired all read as absent, which is what makes
        // "claim this lock" work with a zero expected version.
        if (!versionZero(opt.expect)) return null;
      } else if (!versionEqual(current.version, opt.expect)) {
        return null;
      }
    }
    this.clock++;
    const ver = version(this.clock, this.nodeId);
    const e = remove
      ? { value: '', version: ver, deleted: true, expiresAt: 0, deletedAt: now }
      : { value, version: ver, deleted: false, expiresAt: opt.expiresAt || 0, deletedAt: 0 };
    this.store.set(key, e);
    this.record(key, e, true);
    this.changed();
    return KvStore.entryUpdate(key, e);
  }

  /**
   * Merges a remote update under last-write-wins, reporting whether local state
   * changed. The Lamport clock advances past any counter seen, so this node's
   * later writes sort after it.
   */
  apply(u) {
    return this.applyWithReason(u).applied;
  }

  /**
   * apply, and additionally why an update did not land.
   *
   * A bare boolean collapses two outcomes that deserve opposite responses.
   * "Not newer" is last-write-wins working; a refusal from admit means this
   * node will not hold the entry at all, so it can never converge with the
   * peer that sent it and someone has to raise a limit or free a key. Callers
   * that read both as "stale" counted a store at its limit as healthy — the
   * one state where the counters most need to say otherwise.
   *
   * Returns {applied, refusal}; a null refusal with applied false is the
   * ordinary stale case.
   */
  applyWithReason(u) {
    if (u.version.counter > this.clock) this.clock = u.version.counter;
    // Enforced against a peer as well as a local writer: a limit that the node
    // it protects is the only one unable to fill is not a limit.
    if (u.action !== KVAction.DELETE) {
      const refusal = this.admit(u.key, u.value, this.nowSec());
      if (refusal !== null) return { applied: false, refusal };
    }
    const current = this.store.get(u.key);
    if (current && !versionNewer(u.version, current.version)) return { applied: false, refusal: null };
    const deleted = u.action === KVAction.DELETE;
    const e = {
      value: u.value || '',
      version: u.version,
      deleted,
      expiresAt: u.expiresAt || 0,
      deletedAt: deleted && !u.deletedAt ? this.nowSec() : u.deletedAt || 0,
    };
    this.store.set(u.key, e);
    this.record(u.key, e, false);
    this.changed();
    return { applied: true, refusal: null };
  }

  get(key) {
    const e = this.entry(key);
    return e ? e.value : undefined;
  }

  /** The full live entry for a key, which a compare-and-swap precondition is built from. */
  entry(key) {
    const e = this.store.get(key);
    if (!e || !KvStore.visible(e, this.nowSec())) return null;
    return { key, value: e.value, version: e.version, expiresAt: e.expiresAt };
  }

  /** Every live key with its version and expiry, sorted by key. */
  entries() {
    const now = this.nowSec();
    const out = [];
    for (const [k, e] of this.store) {
      if (KvStore.visible(e, now)) out.push({ key: k, value: e.value, version: e.version, expiresAt: e.expiresAt });
    }
    out.sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
    return out;
  }

  size() {
    const now = this.nowSec();
    let n = 0;
    for (const e of this.store.values()) if (KvStore.visible(e, now)) n++;
    return n;
  }

  tombstones() {
    let n = 0;
    for (const e of this.store.values()) if (e.deleted) n++;
    return n;
  }

  /** The shape of the store, for the diagnostics view. */
  stats() {
    const now = this.nowSec();
    let valueBytes = 0;
    let largestKey = '';
    let largestValueBytes = 0;
    let keys = 0;
    let tombstones = 0;
    for (const [k, e] of this.store) {
      const size = Buffer.byteLength(e.value, 'utf8');
      valueBytes += size;
      if (size > largestValueBytes) {
        largestValueBytes = size;
        largestKey = k;
      }
      if (KvStore.visible(e, now)) keys++;
      if (e.deleted) tombstones++;
    }
    return {
      keys,
      tombstones,
      valueBytes,
      clock: this.clock,
      historyKeys: this.history.size,
      largestKey,
      largestValueBytes,
    };
  }

  /** Every entry, tombstones included — the snapshot a joining peer merges. */
  updates() {
    const out = [];
    for (const [k, e] of this.store) out.push(KvStore.entryUpdate(k, e));
    out.sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
    return out;
  }

  /** Updates for the named keys this node actually holds — how a pull request is answered. */
  updatesFor(keys) {
    const out = [];
    for (const k of keys) {
      const e = this.store.get(k);
      if (e) out.push(KvStore.entryUpdate(k, e));
    }
    return out;
  }

  historyOf(key) {
    return (this.history.get(key) || []).slice();
  }

  /**
   * Advertises up to limit keys after the cursor, reporting the range (lo, hi) it
   * covers with both bounds exclusive. An empty hi means the digest reached the
   * end of the keyspace and the caller should restart its cursor.
   *
   * The range is the point of the whole exchange: without it the receiver could
   * not tell "the sender has nothing for this key" from "the key was outside
   * this batch", and keys the sender is missing entirely would never be repaired.
   */
  digest(after, limit) {
    const keys = [];
    for (const k of this.store.keys()) if (after === '' || k > after) keys.push(k);
    keys.sort();
    const batch = keys.length > limit ? keys.slice(0, limit) : keys;
    // hi is exclusive and has to be the last key's exact successor, so the next
    // batch starts where this one stopped. Go and Kotlin append the same NUL.
    const hi = keys.length > limit ? batch[batch.length - 1] + '\u0000' : '';
    return {
      from: '',
      lo: after,
      hi,
      entries: batch.map((k) => {
        const e = this.store.get(k);
        return { key: k, version: e.version, deleted: e.deleted };
      }),
    };
  }

  /**
   * Summarises the whole store as a fixed set of bucket digests, so two
   * replicas can be compared without shipping either of them.
   *
   * A key's bucket comes from the hash of its **name alone**, and what is
   * folded in is the hash of the whole entry. Bucketing on the name keeps a key
   * in one place however its value changes, so a differing bucket names a
   * stable region of the keyspace; folding with XOR keeps a bucket independent
   * of the order entries were learned in. Tombstones count: two replicas that
   * disagree about whether a key is deleted have diverged just as much as two
   * that disagree about its value.
   *
   * Byte-for-byte identical to Go's KVStore.Fingerprint, and pinned there.
   */
  fingerprint(buckets = FINGERPRINT_BUCKETS) {
    const n = buckets > 0 ? buckets : FINGERPRINT_BUCKETS;
    const folds = Array.from({ length: n }, () => Buffer.alloc(32));
    let keys = 0;
    let tombstones = 0;
    const now = this.nowSec();

    for (const [k, e] of this.store) {
      if (e.deleted) tombstones++;
      else if (KvStore.visible(e, now)) keys++;

      const idx = bucketOf(k, n);

      const sum = createHash('sha256')
        .update(k, 'utf8')
        .update(Buffer.from([0]))
        .update(`${e.version.counter}/${e.version.node}/${e.deleted ? 'true' : 'false'}`, 'utf8')
        .digest();
      const fold = folds[idx];
      for (let i = 0; i < 32; i++) fold[i] ^= sum[i];
    }

    return {
      from: '',
      nick: '',
      keys,
      tombstones,
      clock: this.clock,
      buckets: folds.map((f) => f.toString('hex')),
    };
  }

  /** Lo is exclusive: it is the sender's cursor, the last key it already advertised. */
  static inDigestRange(key, lo, hi) {
    if (lo !== '' && key <= lo) return false;
    return hi === '' || key < hi;
  }

  /**
   * Compares a peer's digest against local state: what to push back (this node is
   * newer, or the peer is missing it entirely) and what to pull (the peer is
   * newer, or this node has never seen it).
   */
  reconcile(d, maxPush, maxPull) {
    const push = [];
    const pull = [];
    const advertised = new Set();
    for (const adv of d.entries) {
      advertised.add(adv.key);
      const local = this.store.get(adv.key);
      if (!local) pull.push(adv.key);
      else if (versionNewer(local.version, adv.version)) push.push(KvStore.entryUpdate(adv.key, local));
      else if (versionNewer(adv.version, local.version)) pull.push(adv.key);
    }
    for (const [k, e] of this.store) {
      if (advertised.has(k)) continue;
      if (KvStore.inDigestRange(k, d.lo, d.hi)) push.push(KvStore.entryUpdate(k, e));
    }
    push.sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
    pull.sort();
    return { push: push.slice(0, maxPush), pull: pull.slice(0, maxPull) };
  }

  /**
   * Turns entries whose TTL has passed into tombstones, keeping each entry's
   * existing version.
   *
   * Keeping the version is what makes expiry safe: bumping the Lamport clock here
   * would let a sweep outrank a concurrent legitimate write to the same key, and
   * every replica sweeps independently. Expiry being deterministic, replicas
   * reach the same state without exchanging a message.
   */
  sweepExpired() {
    const now = this.nowSec();
    let swept = 0;
    for (const [k, e] of this.store) {
      if (e.deleted || !KvStore.expired(e, now)) continue;
      this.store.set(k, { ...e, value: '', deleted: true, deletedAt: now });
      swept++;
    }
    if (swept > 0) this.changed();
    return swept;
  }

  /**
   * Drops tombstones older than olderThanSec.
   *
   * This is the one operation that can resurrect a key: a peer that never saw the
   * delete and still holds the value will push it back once the tombstone is
   * gone. The age has to exceed the longest partition expected to heal.
   */
  gcTombstones(olderThanSec) {
    if (!olderThanSec || olderThanSec <= 0) return 0;
    const cutoff = this.nowSec() - olderThanSec;
    let removed = 0;
    for (const [k, e] of [...this.store]) {
      if (e.deleted && e.deletedAt !== 0 && e.deletedAt < cutoff) {
        this.store.delete(k);
        this.history.delete(k);
        removed++;
      }
    }
    if (removed > 0) this.changed();
    return removed;
  }

  static entryUpdate(key, e) {
    return {
      action: e.deleted ? KVAction.DELETE : KVAction.SET,
      key,
      value: e.value,
      version: e.version,
      expiresAt: e.expiresAt,
      deletedAt: e.deletedAt,
    };
  }
}

module.exports = { KvStore, HISTORY_PER_KEY, REFUSAL, bucketOf };
