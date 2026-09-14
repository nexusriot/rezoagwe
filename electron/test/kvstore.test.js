'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const { KvStore } = require('../src/core/kvstore');
const { version, KVAction, FINGERPRINT_BUCKETS } = require('../src/proto/wire');

/**
 * The replication rules, asserted the way the Go and Kotlin stores assert them.
 * These are protocol, not implementation detail: if this store disagrees with
 * either of the others, two replicas of one cluster silently hold different
 * values and nothing reports it.
 */

const at = (t) => {
  let now = t;
  return { store: (id) => new KvStore(id, { nowSec: () => now }), set: (v) => { now = v; }, now: () => now };
};

test('a local write stamps a version and reads back', () => {
  const kv = new KvStore('n1');
  const u = kv.write('colour', 'blue');
  assert.equal(u.version.counter, 1);
  assert.equal(u.version.node, 'n1');
  assert.equal(kv.get('colour'), 'blue');
});

test('last-write-wins: a newer version applies, an older one does not', () => {
  const kv = new KvStore('local');
  kv.write('k', 'mine'); // v1 local
  assert.ok(kv.apply({ action: KVAction.SET, key: 'k', value: 'theirs', version: version(5, 'remote') }));
  assert.equal(kv.get('k'), 'theirs');
  assert.ok(!kv.apply({ action: KVAction.SET, key: 'k', value: 'stale', version: version(2, 'remote') }));
  assert.equal(kv.get('k'), 'theirs');
});

test('re-delivering the same version is idempotent', () => {
  const kv = new KvStore('local');
  const u = { action: KVAction.SET, key: 'k', value: 'v', version: version(3, 'remote') };
  assert.ok(kv.apply(u));
  assert.ok(!kv.apply(u), 'equal is not newer');
});

test('applying a remote version advances the Lamport clock past it', () => {
  const kv = new KvStore('local');
  kv.apply({ action: KVAction.SET, key: 'k', value: 'v', version: version(9, 'remote') });
  const next = kv.write('other', 'x');
  assert.equal(next.version.counter, 10, "this node's later writes have to sort after what it has seen");
});

test('a delete is a tombstone a stale set cannot resurrect', () => {
  const kv = new KvStore('local');
  kv.apply({ action: KVAction.SET, key: 'k', value: 'v', version: version(4, 'remote') });
  kv.remove('k'); // local tombstone at v5
  assert.equal(kv.get('k'), undefined);
  assert.ok(!kv.apply({ action: KVAction.SET, key: 'k', value: 'back', version: version(4, 'remote') }));
  assert.equal(kv.get('k'), undefined);
  assert.equal(kv.tombstones(), 1);
});

test('compare-and-swap lands only against the expected version', () => {
  const kv = new KvStore('n');
  const first = kv.write('lock', 'a');
  assert.equal(kv.write('lock', 'b', { expect: version(99, 'nobody') }), null);
  assert.equal(kv.get('lock'), 'a');
  assert.ok(kv.write('lock', 'b', { expect: first.version }));
  assert.equal(kv.get('lock'), 'b');
});

test('a zero expected version means "only if absent", and absent covers a tombstone', () => {
  const kv = new KvStore('n');
  assert.ok(kv.write('lock', 'mine', { expect: version(0, '') }), 'claiming a free lock succeeds');
  assert.equal(kv.write('lock', 'yours', { expect: version(0, '') }), null, 'the lock is taken');
  kv.remove('lock');
  assert.ok(kv.write('lock', 'next', { expect: version(0, '') }), 'a tombstoned key reads as absent');
});

test('an expired key reads as absent and can be claimed again', () => {
  const clock = at(1000);
  const kv = clock.store('n');
  kv.write('session', 'v', { expiresAt: 1010 });
  assert.equal(kv.get('session'), 'v');
  clock.set(1010);
  assert.equal(kv.get('session'), undefined, 'expiry is exact at the second');
  assert.ok(kv.write('session', 'again', { expect: version(0, '') }));
});

test('expiry keeps the entry version instead of bumping the clock', () => {
  // Bumping it would let a sweep outrank a concurrent legitimate write, and
  // every replica sweeps independently.
  const clock = at(1000);
  const kv = clock.store('n');
  const written = kv.write('k', 'v', { expiresAt: 1005 });
  clock.set(1006);
  assert.equal(kv.sweepExpired(), 1);
  const tomb = kv.updates().find((u) => u.key === 'k');
  assert.equal(tomb.action, KVAction.DELETE);
  assert.deepEqual(tomb.version, written.version, 'the version must survive the sweep unchanged');
  assert.equal(kv.clock, written.version.counter);
});

test('two replicas sweeping independently reach the same state with no message', () => {
  const clock = at(1000);
  const a = clock.store('a');
  const b = clock.store('b');
  const u = a.write('k', 'v', { expiresAt: 1005 });
  b.apply(u);
  clock.set(1006);
  a.sweepExpired();
  b.sweepExpired();
  assert.deepEqual(a.updates(), b.updates());
});

test('a digest covers a bounded range and the cursor walks the keyspace', () => {
  const kv = new KvStore('n');
  for (const k of ['a', 'b', 'c', 'd', 'e']) kv.write(k, k);
  const first = kv.digest('', 2);
  assert.deepEqual(first.entries.map((e) => e.key), ['a', 'b']);
  assert.equal(first.lo, '');
  assert.equal(first.hi, 'b\u0000', 'hi is the last key’s exact successor, so the next batch starts there');
  const second = kv.digest(first.entries[first.entries.length - 1].key, 2);
  assert.deepEqual(second.entries.map((e) => e.key), ['c', 'd']);
  const third = kv.digest('d', 2);
  assert.deepEqual(third.entries.map((e) => e.key), ['e']);
  assert.equal(third.hi, '', 'reaching the tail is signalled by an empty hi, which restarts the cursor');
});

test('the digest range is what lets a receiver spot a key the sender never had', () => {
  const sender = new KvStore('s');
  const receiver = new KvStore('r');
  receiver.write('only-here', 'v');
  const digest = sender.digest('', 256); // empty store: entries [], hi ''
  const { push, pull } = receiver.reconcile(digest, 128, 128);
  assert.equal(pull.length, 0);
  assert.deepEqual(push.map((u) => u.key), ['only-here'],
    'without the range this key would look like "outside the batch" and never be repaired');
});

test('reconcile pushes what is newer here and pulls what is newer there', () => {
  const mine = new KvStore('mine');
  const theirs = new KvStore('theirs');
  mine.write('shared', 'old');
  const newer = theirs.apply({ action: KVAction.SET, key: 'shared', value: 'new', version: version(9, 'z') });
  assert.ok(newer);
  theirs.write('only-theirs', 'v');
  mine.write('only-mine', 'v');

  const digest = theirs.digest('', 256);
  const { push, pull } = mine.reconcile(digest, 128, 128);
  assert.deepEqual(pull, ['only-theirs', 'shared'],
    'their newer copy is pulled, and so is the key this node has never seen');
  assert.deepEqual(push.map((u) => u.key), ['only-mine'], 'what they lack inside the range is pushed');
});

test('a key outside the digest range is left alone', () => {
  const kv = new KvStore('n');
  kv.write('a', '1');
  kv.write('z', '26');
  const { push } = kv.reconcile({ from: 'p', lo: '', hi: 'b', entries: [] }, 128, 128);
  assert.deepEqual(push.map((u) => u.key), ['a'], 'z is outside (lo, hi) and must not be pushed');
});

test('repair traffic is bounded by maxPush and maxPull', () => {
  const kv = new KvStore('n');
  for (let i = 0; i < 50; i++) kv.write(`k${i}`, 'v');
  const { push } = kv.reconcile({ from: 'p', lo: '', hi: '', entries: [] }, 10, 10);
  assert.equal(push.length, 10);
});

test('tombstone GC is off by default and age-based when enabled', () => {
  const clock = at(1000);
  const kv = clock.store('n');
  kv.write('k', 'v');
  kv.remove('k');
  assert.equal(kv.gcTombstones(0), 0, 'GC off means nothing is ever reclaimed');
  clock.set(1000 + 30);
  assert.equal(kv.gcTombstones(60), 0, 'the tombstone is younger than the age');
  clock.set(1000 + 120);
  assert.equal(kv.gcTombstones(60), 1);
  assert.equal(kv.tombstones(), 0);
  assert.equal(kv.historyOf('k').length, 0, 'history goes with the key it described');
});

test('history records who wrote each version and whether it was local', () => {
  const kv = new KvStore('local');
  kv.write('k', 'first');
  kv.apply({ action: KVAction.SET, key: 'k', value: 'remote', version: version(7, 'other') });
  const history = kv.historyOf('k');
  assert.equal(history.length, 2);
  assert.equal(history[0].local, true);
  assert.equal(history[1].local, false);
  assert.equal(history[1].value, 'remote');
});

test('history is a bounded ring, not an ever-growing log', () => {
  const kv = new KvStore('n');
  for (let i = 0; i < 40; i++) kv.write('k', `v${i}`);
  assert.equal(kv.historyOf('k').length, 20);
});

test('entries hide tombstones and expired keys but updates carry them', () => {
  const clock = at(1000);
  const kv = clock.store('n');
  kv.write('live', 'v');
  kv.write('gone', 'v');
  kv.remove('gone');
  kv.write('soon', 'v', { expiresAt: 1001 });
  clock.set(1002);
  assert.deepEqual(kv.entries().map((e) => e.key), ['live']);
  assert.deepEqual(kv.updates().map((u) => u.key), ['gone', 'live', 'soon'], 'a joiner needs the tombstones too');
  assert.equal(kv.size(), 1);
});

test('a persisted store reloads with its clock, values and tombstones intact', () => {
  const kv = new KvStore('n');
  kv.write('a', 'v');
  kv.remove('b');
  const snapshot = kv.snapshotState();

  const restored = new KvStore('n');
  restored.loadState(snapshot.clock, snapshot.entries);
  assert.equal(restored.clock, kv.clock);
  assert.equal(restored.get('a'), 'v');
  assert.equal(restored.tombstones(), 1);
  const next = restored.write('c', 'v');
  assert.equal(next.version.counter, kv.clock + 1, 'the clock must not restart at 1 after a reload');
});

test('the change callback fires once per mutation and not on a refused write', () => {
  const kv = new KvStore('n');
  let changes = 0;
  kv.setOnChange(() => { changes++; });
  kv.write('k', 'v');
  assert.equal(changes, 1);
  kv.write('k', 'v2', { expect: version(99, 'nobody') });
  assert.equal(changes, 1, 'a refused compare-and-swap changed nothing to persist');
  kv.remove('k');
  assert.equal(changes, 2);
});

test('stats describe the shape of the store for the diagnostics screen', () => {
  const kv = new KvStore('n');
  kv.write('small', 'x');
  kv.write('big', 'y'.repeat(100));
  kv.remove('gone');
  const stats = kv.stats();
  assert.equal(stats.keys, 2);
  assert.equal(stats.tombstones, 1);
  assert.equal(stats.largestKey, 'big');
  assert.equal(stats.largestValueBytes, 100);
  assert.ok(stats.valueBytes >= 101);
});

// The store fingerprint is what a consistency check compares, so a JavaScript
// node and a Go node must produce byte-identical digests for the same entries.
// Anything else reports two converged replicas as divergent — the loudest
// possible false alarm, from the one feature whose whole job is to be trusted.
//
// These are the digests the Go KVStore.Fingerprint emits for the entries below.
const GO_FINGERPRINT = {
  7: 'a2026aaa5ddb78cb47d1db8e5973b787420dafb7fc1ad30cca5944a5495f9898',
  13: '5afa98b8cab4abc9294acac825fad1b3f02d6125ec2f5b0def0456acb56e3abc',
};
const ZERO = '0'.repeat(64);

function vectorStore() {
  const kv = new KvStore('node-a');
  kv.apply({ action: KVAction.SET, key: 'alpha', value: 'one', version: { counter: 3, node: 'node-a' }, expiresAt: 0, deletedAt: 0 });
  kv.apply({ action: KVAction.SET, key: 'beta', value: 'two', version: { counter: 7, node: 'node-b' }, expiresAt: 0, deletedAt: 0 });
  kv.apply({ action: KVAction.DELETE, key: 'gamma', value: '', version: { counter: 9, node: 'node-a' }, expiresAt: 0, deletedAt: 1700000000 });
  return kv;
}

test('the store fingerprint matches the Go vectors byte for byte', () => {
  const f = vectorStore().fingerprint();

  assert.equal(f.buckets.length, FINGERPRINT_BUCKETS);
  assert.equal(f.keys, 2, 'the tombstone must not be counted as a live key');
  assert.equal(f.tombstones, 1);
  assert.equal(f.clock, 9, 'the clock must have advanced past every version applied');

  for (let i = 0; i < FINGERPRINT_BUCKETS; i++) {
    assert.equal(f.buckets[i], GO_FINGERPRINT[i] ?? ZERO, `bucket ${i} differs from Go`);
  }
});

// The fold is an XOR so it cannot depend on insertion order: two replicas that
// learned the same writes in a different sequence have to agree, or the check
// is worse than not having one.
test('the fingerprint does not depend on the order entries were learned', () => {
  const forwards = vectorStore().fingerprint();

  const backwards = new KvStore('node-a');
  backwards.apply({ action: KVAction.DELETE, key: 'gamma', value: '', version: { counter: 9, node: 'node-a' }, expiresAt: 0, deletedAt: 1700000000 });
  backwards.apply({ action: KVAction.SET, key: 'beta', value: 'two', version: { counter: 7, node: 'node-b' }, expiresAt: 0, deletedAt: 0 });
  backwards.apply({ action: KVAction.SET, key: 'alpha', value: 'one', version: { counter: 3, node: 'node-a' }, expiresAt: 0, deletedAt: 0 });

  assert.deepEqual(backwards.fingerprint().buckets, forwards.buckets);
});

// A key stays in the bucket its *name* chose, whatever happens to its value.
// Bucketing on the whole entry instead relocates a key on every write, so one
// stale value lights up two buckets and neither names a region you could go and
// look at — which is the only reason to have buckets rather than one digest.
test('a differing version changes exactly one bucket, and does not relocate the key', () => {
  const a = vectorStore();
  const b = vectorStore();
  b.apply({ action: KVAction.SET, key: 'alpha', value: 'one', version: { counter: 4, node: 'node-a' }, expiresAt: 0, deletedAt: 0 });

  const fa = a.fingerprint();
  const fb = b.fingerprint();
  const differing = fa.buckets.filter((v, i) => v !== fb.buckets[i]).length;
  assert.equal(differing, 1, 'exactly one bucket should differ for one differing key');
});

// Two empty stores must agree, or every fresh cluster starts out "divergent".
test('two empty stores fingerprint identically', () => {
  const a = new KvStore('node-a').fingerprint();
  const b = new KvStore('node-b').fingerprint();
  assert.deepEqual(a.buckets, b.buckets);
  assert.ok(a.buckets.every((v) => v === ZERO), 'an empty store should fold to zeroes');
});
