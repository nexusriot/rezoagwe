'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const { KvStore } = require('../src/core/kvstore');
const { NodeEngine, DEFAULTS } = require('../src/core/node-engine');
const { MemNetwork } = require('../src/net/mem');
const { KVAction } = require('../src/proto/wire');

/**
 * The three implementations are one protocol, so a correctness fix in the Go
 * node is a bug still open here until it lands here too. These are the ones
 * that were: an anti-entropy round that repaired half of what it identified,
 * a single keyspace cursor shared across every peer, and a store with no
 * bound on what a peer could push into it.
 */

/** Long intervals: these tests drive gossip themselves. */
const QUIET = {
  gossipIntervalMs: 3_600_000,
  heartbeatIntervalMs: 3_600_000,
  evictThresholdMs: 3_600_000,
  sweepIntervalMs: 3_600_000,
};

function cluster() {
  const net = new MemNetwork();
  const nodes = [];
  const make = async (name, port) => {
    const addr = `127.0.0.1:${port}`;
    const engine = new NodeEngine(
      { advertiseHost: '127.0.0.1', port, nick: name, psk: 'k', cluster: 'test', ...QUIET },
      null,
      { listen: async () => net.attach(addr) },
    );
    await engine.start();
    await engine.joining;
    nodes.push(engine);
    return engine;
  };
  const stopAll = async () => {
    for (const n of nodes) await n.stop();
  };
  return { make, stopAll };
}

const settle = (ms = 60) => new Promise((r) => setTimeout(r, ms));

test('the digest is never wider than one round can repair', () => {
  const widths = (cfg) => {
    const e = Object.create(NodeEngine.prototype);
    e.config = { ...DEFAULTS, ...cfg };
    return e.effectiveDigestBatch();
  };
  assert.equal(widths({ digestBatch: 4096, maxPush: 128, maxPull: 64 }), 64,
    'a wide digest was not clamped to the smaller budget');
  assert.equal(widths({ digestBatch: 32, maxPush: 128, maxPull: 128 }), 32,
    'a digest inside the budget was changed');

  // The shipped default has to already satisfy it, or every cluster runs with
  // the bug until someone tunes it.
  assert.ok(DEFAULTS.digestBatch <= Math.min(DEFAULTS.maxPush, DEFAULTS.maxPull));
});

test('one round repairs everything the digest identified', async () => {
  const c = cluster();
  const a = await c.make('alice', 3210);
  const b = await c.make('bob', 3211);
  a.addPeer(b.addr);
  b.addPeer(a.addr);

  for (let i = 0; i < 300; i++) b.store.write(`k${String(i).padStart(4, '0')}`, 'v', {});

  await a.antiEntropyRound(b.addr);
  await settle(300);

  assert.ok(a.store.size() >= a.effectiveDigestBatch(),
    `one round repaired ${a.store.size()} keys of the ${a.effectiveDigestBatch()} it advertised for`);

  await c.stopAll();
});

test('each peer gets its own keyspace cursor', async () => {
  const cl = cluster();
  const a = await cl.make('alice', 3220);
  const b = await cl.make('bob', 3221);
  const c = await cl.make('carol', 3222);
  a.addPeer(b.addr);
  a.addPeer(c.addr);

  for (let i = 0; i < a.effectiveDigestBatch() * 3; i++) a.store.write(`k${String(i).padStart(5, '0')}`, 'v', {});

  await a.antiEntropyRound(b.addr);
  await a.antiEntropyRound(c.addr);
  const firstB = a.aeCursors.get(b.addr);
  const firstC = a.aeCursors.get(c.addr);
  assert.ok(firstB && firstC, 'cursors were not advanced');
  assert.equal(firstB, firstC, 'the first round against each peer covered different ranges');

  // A second round with b alone must not move c's cursor.
  await a.antiEntropyRound(b.addr);
  assert.notEqual(a.aeCursors.get(b.addr), firstB, "b's cursor did not advance");
  assert.equal(a.aeCursors.get(c.addr), firstC, "c's cursor moved because of a round with b");

  // And a departing peer takes its cursor with it.
  a.removePeer(b.addr);
  assert.equal(a.aeCursors.has(b.addr), false, 'the cursor survived the peer');

  await cl.stopAll();
});

test('a store limit refuses an oversize write from a peer as well as a local one', () => {
  const kv = new KvStore('n1', { limits: { maxValueBytes: 8 } });

  assert.ok(kv.write('k', '12345678', {}), 'a value exactly at the limit was refused');
  assert.equal(kv.write('k', '123456789', {}), null, 'a value over the limit was accepted');

  const applied = kv.apply({
    action: KVAction.SET, key: 'big', value: 'x'.repeat(64),
    version: { counter: 9, node: 'peer' }, expiresAt: 0, deletedAt: 0,
  });
  assert.equal(applied, false, 'an oversize remote update was applied');
  assert.equal(kv.get('big'), undefined);
  // The clock still advances: refusing the value is not a reason to let this
  // node's later writes sort before the one it refused.
  assert.equal(kv.clock, 9);
});

test('a key limit counts live keys and always allows an update', () => {
  const kv = new KvStore('n1', { limits: { maxKeys: 2 } });
  kv.write('a', '1', {});
  kv.write('b', '1', {});
  assert.equal(kv.write('c', '1', {}), null, 'a third key was accepted past maxKeys=2');
  assert.ok(kv.write('a', '2', {}), 'an update to a key already held was refused');

  kv.remove('a', {});
  assert.ok(kv.write('c', '1', {}), 'a deleted key did not free its slot');
});

test('an unlimited store is the default', () => {
  const kv = new KvStore('n1');
  assert.ok(kv.write('k', 'x'.repeat(100000), {}), 'the default store refused a large value');
});

test('a consistency check finds a divergent replica and localises it', async () => {
  const cl = cluster();
  const a = await cl.make('alice', 3230);
  const b = await cl.make('bob', 3231);
  a.addPeer(b.addr);
  b.addPeer(a.addr);

  for (let i = 0; i < 10; i++) {
    const u = a.store.write(`k${i}`, 'v', {});
    b.store.apply(u);
  }

  let report = await a.checkConsistency();
  assert.equal(report.converged, true, `identical replicas reported divergent: ${JSON.stringify(report.peers)}`);
  assert.equal(report.peers.length, 1);
  assert.equal(report.peers[0].reachable, true);
  assert.equal(report.peers[0].keys, 10);

  // One write b never sees.
  a.store.write('lost', 'value', {});
  report = await a.checkConsistency();
  assert.equal(report.converged, false, 'a missing key was reported as converged');
  assert.equal(report.peers[0].differingBuckets.length, 1, 'one differing key should light one bucket');
  assert.equal(report.peers[0].keys, 10);

  await cl.stopAll();
});

// A peer that does not answer says nothing about whether it agrees.
test('an unreachable peer is reported separately, not as a disagreement', async () => {
  const cl = cluster();
  const a = await cl.make('alice', 3240);
  a.addPeer('127.0.0.1:3999'); // never started

  const report = await a.checkConsistency();
  assert.equal(report.unreachable, 1);
  assert.equal(report.converged, true, 'an unreachable peer was counted as a disagreement');
  assert.equal(report.peers[0].reachable, false);
  assert.ok(report.peers[0].error, 'no reason given for the unreachable peer');

  await cl.stopAll();
});

test('the sweep summary describes the per-peer cursors', async () => {
  const cl = cluster();
  const a = await cl.make('alice', 3250);
  const b = await cl.make('bob', 3251);
  a.addPeer(b.addr);

  assert.match(a.sweepSummary(), /at the start of the keyspace/);

  for (let i = 0; i < a.effectiveDigestBatch() * 2; i++) a.store.write(`k${String(i).padStart(5, '0')}`, 'v', {});
  await a.antiEntropyRound(b.addr);
  assert.match(a.sweepSummary(), /1 of 1 peer\(s\) mid-sweep/);

  await cl.stopAll();
});
