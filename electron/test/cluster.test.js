'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const { NodeEngine } = require('../src/core/node-engine');
const { BootstrapServer } = require('../src/core/bootstrap-server');
const { MemNetwork } = require('../src/net/mem');

/**
 * A whole cluster in one process, over an in-memory network with configurable
 * loss and partitions.
 *
 * This is where convergence is asserted rather than hoped for: the rules that
 * matter (a dropped write is repaired, a delete is not resurrected, a joiner
 * inherits a store nobody replayed to it) only show up between nodes, and only a
 * deterministic network can test a partition at all.
 */

/** Long intervals: the tests drive gossip themselves, so nothing fires behind their back. */
const QUIET = {
  gossipIntervalMs: 3_600_000,
  heartbeatIntervalMs: 3_600_000,
  evictThresholdMs: 3_600_000,
  sweepIntervalMs: 3_600_000,
};

function cluster(opts = {}) {
  const net = new MemNetwork(opts);
  const nodes = [];
  const make = async (name, config = {}) => {
    const addr = `127.0.0.1:${config.port || 3100 + nodes.length}`;
    const engine = new NodeEngine(
      { advertiseHost: '127.0.0.1', port: Number(addr.split(':')[1]), nick: name, psk: 'k', cluster: 'test', ...QUIET, ...config },
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
  return { net, make, nodes, stopAll };
}

/** Wires two nodes to each other directly, the way a bootstrap roster would. */
async function link(a, b) {
  a.addPeer(b.addr);
  b.addPeer(a.addr);
  await a.helloAllPeers();
  await b.helloAllPeers();
  await settle();
}

/** Lets the in-memory network drain: delivery is scheduled, not synchronous. */
const settle = (ms = 30) => new Promise((r) => setTimeout(r, ms));

test('a write reaches a peer', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  await a.set('colour', 'blue');
  await settle();
  assert.equal(b.store.get('colour'), 'blue');
  await c.stopAll();
});

test('concurrent writes to one key converge on the same value everywhere', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  // Both write the same key before either has seen the other's packet.
  a.store.write('k', 'from-a');
  b.store.write('k', 'from-b');
  await a.gossipTo(b.addr);
  await settle();
  await b.gossipTo(a.addr);
  await settle();
  assert.equal(a.store.get('k'), b.store.get('k'), 'last-write-wins has to pick the same winner on both');
  await c.stopAll();
});

test('a joiner pulls the store and the chat over a stream', async () => {
  const c = cluster();
  const a = await c.make('alice');
  for (let i = 0; i < 5; i++) await a.set(`key${i}`, `value${i}`);
  await a.sendChat('hello from before you arrived');

  const b = await c.make('bob');
  b.addPeer(a.addr);
  await b.requestStateFrom(a.addr);
  await settle();

  assert.equal(b.store.size(), 5, 'the whole store syncs, not a datagram-sized slice');
  assert.equal(b.store.get('key3'), 'value3');
  assert.ok(b.chat().some((e) => e.text === 'hello from before you arrived'),
    'a joiner gets context instead of an empty pane');
  await c.stopAll();
});

test('a direct message is never handed to a joiner with the history', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);
  await a.sendDirect(b.addr, 'this is between us');
  await a.sendChat('this is public');
  await settle();

  const joiner = await c.make('carol');
  joiner.addPeer(a.addr);
  await joiner.requestStateFrom(a.addr);
  await settle();

  const texts = joiner.chat().map((e) => e.text);
  assert.ok(texts.includes('this is public'));
  assert.ok(!texts.includes('this is between us'), 'handing over a private conversation would be a quiet leak');
  await c.stopAll();
});

test('a dropped write is repaired by the next anti-entropy round', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  // The write's packet never arrives: exactly the case the write path cannot fix.
  c.net.partition(a.addr, b.addr);
  await a.set('lost', 'value');
  await settle();
  assert.equal(b.store.get('lost'), undefined);

  c.net.heal(a.addr, b.addr);
  await a.gossipTo(b.addr);
  await settle();
  await b.gossipTo(a.addr);
  await settle();
  assert.equal(b.store.get('lost'), 'value', 'the digest exchange is what makes convergence a mechanism');
  await c.stopAll();
});

test('anti-entropy repairs in both directions in one exchange', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);
  c.net.partition(a.addr, b.addr);
  await a.set('only-a', '1');
  await b.set('only-b', '2');
  c.net.heal(a.addr, b.addr);

  await a.gossipTo(b.addr); // b pushes what it has and pulls what it lacks
  await settle();
  await settle();
  assert.equal(b.store.get('only-a'), '1');
  assert.equal(a.store.get('only-b'), '2');
  await c.stopAll();
});

test('a whole cluster converges under heavy packet loss', async () => {
  const c = cluster({ loss: 0.3 });
  const a = await c.make('alice');
  const b = await c.make('bob');
  const d = await c.make('dave');
  await link(a, b);
  await link(b, d);
  await link(a, d);

  for (let i = 0; i < 8; i++) await a.set(`k${i}`, `v${i}`);
  // Enough rounds that a run of dropped packets is repaired rather than fatal.
  for (let round = 0; round < 14; round++) {
    for (const from of [a, b, d]) {
      for (const to of [a, b, d]) {
        if (from !== to) await from.gossipTo(to.addr);
      }
    }
    await settle();
  }
  for (let i = 0; i < 8; i++) {
    assert.equal(b.store.get(`k${i}`), `v${i}`, `bob is missing k${i} after repair rounds`);
    assert.equal(d.store.get(`k${i}`), `v${i}`, `dave is missing k${i} after repair rounds`);
  }
  await c.stopAll();
});

test('a delete is not resurrected by the peer that still holds the value', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);
  await a.set('doomed', 'v');
  await settle();
  assert.equal(b.store.get('doomed'), 'v');

  c.net.partition(a.addr, b.addr);
  await a.delete('doomed');
  c.net.heal(a.addr, b.addr);

  // Bob still has the value and advertises it; the tombstone has to win.
  await b.gossipTo(a.addr);
  await settle();
  await a.gossipTo(b.addr);
  await settle();
  assert.equal(a.store.get('doomed'), undefined, 'the deleting node must not get the value back');
  assert.equal(b.store.get('doomed'), undefined, 'the tombstone has to reach the other side');
  await c.stopAll();
});

test('gossip teaches a third node, and the graph shows the link it only heard about', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  const d = await c.make('dave');
  await link(a, b);
  await link(b, d);

  await b.gossipTo(a.addr); // b tells a about dave
  await settle();
  assert.ok(a.peerAddrs().includes(d.addr), 'a learns dave from the peer list gossip carries');

  const topology = a.topology();
  assert.ok(topology.nodes.some((n) => n.addr.includes(String(d.addr.split(':')[1]))));
  await c.stopAll();
});

test('a clean shutdown announces itself instead of being timed out', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);
  assert.equal(b.peerAddrs().length, 1);

  await a.stop();
  await settle();
  assert.equal(b.peerAddrs().length, 0, 'a goodbye drops the peer at once, not after the eviction window');
  assert.ok(b.chat().some((e) => e.kind === 'system' && e.text.includes('left')));
  await b.stop();
});

test('a stale peer is evicted once the threshold passes', async () => {
  const c = cluster();
  const a = await c.make('alice', { evictThresholdMs: 50 });
  const b = await c.make('bob');
  await link(a, b);
  assert.equal(a.peerAddrs().length, 1);
  await settle(80);
  a.evictTick();
  assert.equal(a.peerAddrs().length, 0);
  await c.stopAll();
});

test('two clusters on one network cannot read each other', async () => {
  const net = new MemNetwork();
  const mk = async (port, cfg) => {
    const addr = `127.0.0.1:${port}`;
    const engine = new NodeEngine(
      { advertiseHost: '127.0.0.1', port, ...QUIET, ...cfg },
      null,
      { listen: async () => net.attach(addr) },
    );
    await engine.start();
    await engine.joining;
    return engine;
  };
  const home = await mk(3301, { nick: 'home', psk: 'one', cluster: 'home' });
  const other = await mk(3302, { nick: 'other', psk: 'two', cluster: 'home' });

  home.addPeer(other.addr);
  await home.set('secret', 'value');
  await settle();
  assert.equal(other.store.get('secret'), undefined, 'a different key must not decode');
  assert.ok(other.metrics.authFailures > 0, 'and the drop has to be counted, not silent');
  await home.stop();
  await other.stop();
});

test('a node joins through the rendezvous service and inherits the store', async () => {
  const net = new MemNetwork();
  const bootstrapAddr = '127.0.0.1:9990';
  const bootstrap = new BootstrapServer(
    { port: 9990, psk: 'k', cluster: 'test', nodeTimeoutMs: 3_600_000 },
    null,
    { listen: async () => net.attach(bootstrapAddr) },
  );
  await bootstrap.start();

  const mk = async (port, nick) => {
    const addr = `127.0.0.1:${port}`;
    const engine = new NodeEngine(
      {
        advertiseHost: '127.0.0.1', port, nick, psk: 'k', cluster: 'test', seeds: [bootstrapAddr], ...QUIET,
      },
      null,
      { listen: async () => net.attach(addr) },
    );
    await engine.start();
    await engine.joining;
    return engine;
  };

  const first = await mk(3401, 'alice');
  await settle();
  assert.equal(bootstrap.roster().length, 1, 'a REGISTER puts the node on the roster');
  await first.set('shared', 'value');

  const second = await mk(3402, 'bob');
  await settle(60);
  assert.ok(second.peerAddrs().includes(first.addr), 'the roster is how a joiner finds anyone at all');
  assert.equal(second.store.get('shared'), 'value', 'and the state sync gives it the store');

  // The rendezvous never sees the data it helped replicate.
  assert.equal(bootstrap.roster().length, 2);
  await first.stop();
  await second.stop();
  await bootstrap.stop();
});

test('an unreachable seed does not stall startup', async () => {
  const net = new MemNetwork();
  const addr = '127.0.0.1:3501';
  const engine = new NodeEngine(
    {
      advertiseHost: '127.0.0.1', port: 3501, psk: 'k', cluster: 'test', seeds: ['127.0.0.1:9', '10.0.0.1:9999'], ...QUIET,
    },
    null,
    { listen: async () => net.attach(addr) },
  );
  const started = Date.now();
  await engine.start();
  await engine.joining;
  assert.ok(Date.now() - started < 3000, 'nothing may block on a rendezvous that is not there');
  assert.ok(engine.isRunning);
  await engine.stop();
});

test('chat, emotes and direct messages reach the right nodes', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  const d = await c.make('dave');
  await link(a, b);
  await link(a, d);

  await a.submit('hello everyone');
  await a.submit('/me waves');
  await a.submit(`/msg ${b.addr} just for you`);
  await settle();

  assert.ok(b.chat().some((e) => e.text === 'hello everyone' && e.kind === ''));
  assert.ok(b.chat().some((e) => e.text === 'waves' && e.kind === 'action'));
  assert.ok(b.chat().some((e) => e.text === 'just for you' && e.kind === 'dm'));
  assert.ok(!d.chat().some((e) => e.text === 'just for you'), 'a direct message is not a broadcast');
  await c.stopAll();
});

test('slash commands operate the store the same way the terminal client does', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  await a.submit('/set motd hello');
  await settle();
  assert.equal(b.store.get('motd'), 'hello');

  await a.submit('/setttl session 60 token');
  assert.ok(a.store.entry('session').expiresAt > 0);

  await a.submit('/del motd');
  await settle();
  assert.equal(b.store.get('motd'), undefined);

  await a.submit('/nick renamed');
  assert.equal(a.status().nick, 'renamed');
  await c.stopAll();
});

test('a guarded write is refused when a peer changed the key first', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  const original = await a.set('lock', 'first');
  await settle();
  await b.set('lock', 'second'); // b's write outranks a's
  await settle();

  const refused = await a.compareAndSet('lock', 'third', 0, original.version);
  assert.equal(refused, null, 'the version on screen is stale, so the save has to be refused');
  assert.equal(a.metrics.kvCasFailures, 1);
  assert.equal(a.store.get('lock'), 'second');
  await c.stopAll();
});

test('an import merges by version and a seed forces the data to win', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);
  await b.set('key', 'first');
  await b.set('key', 'newer-on-b');
  await settle();

  // Counter 1 against the cluster's counter 2: strictly older, whichever way the
  // node-id tiebreak would have gone.
  const older = JSON.stringify({
    node: 'x', cluster: 'test', exported_at: 1,
    entries: [{ action: 'set', key: 'key', value: 'older', version: { counter: 1, node: 'ancient' } }],
  });
  assert.equal(await a.import(older, false), 0, 'merging keeps the cluster’s newer value');
  assert.equal(a.store.get('key'), 'newer-on-b');

  assert.equal(await a.import(older, true), 1, 'seeding re-stamps it as a local write, so it wins');
  await settle();
  assert.equal(a.store.get('key'), 'older');
  assert.equal(b.store.get('key'), 'older', 'and the seed replicates');
  await c.stopAll();
});

test('an export round-trips through import on a fresh node', async () => {
  const c = cluster();
  const a = await c.make('alice');
  await a.set('a', '1');
  await a.set('b', '2');
  await a.delete('b');
  const dump = a.export();

  const fresh = await c.make('fresh');
  assert.equal(await fresh.import(dump, false), 2, 'the tombstone travels too, or the delete is lost');
  assert.equal(fresh.store.get('a'), '1');
  assert.equal(fresh.store.get('b'), undefined);
  await c.stopAll();
});

test('a large store syncs over a stream rather than being cut to a datagram', async () => {
  const c = cluster();
  const a = await c.make('alice');
  // Comfortably more than the 200-entry datagram cap.
  for (let i = 0; i < 400; i++) await a.set(`key-${String(i).padStart(4, '0')}`, 'x'.repeat(64));

  const b = await c.make('bob');
  b.addPeer(a.addr);
  await b.requestStateFrom(a.addr);
  await settle(60);
  assert.equal(b.store.size(), 400, 'the stream path has no datagram ceiling');
  await c.stopAll();
});

test('a node that has never gossiped still reports itself in the graph', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const topology = a.topology();
  assert.equal(topology.nodes.length, 1);
  assert.equal(topology.nodes[0].role, 'self');
  assert.equal(topology.nodes[0].x, 0, 'the layout arrives with the topology, ready to draw');
  await c.stopAll();
});

test('metrics count what actually happened on the wire', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);
  await a.set('k', 'v');
  await settle();

  assert.ok(a.metrics.packetsSent > 0);
  assert.ok(b.metrics.packetsReceived > 0);
  assert.equal(a.metrics.kvLocalWrites, 1);
  assert.equal(b.metrics.kvApplied, 1);

  await a.gossipTo(b.addr);
  await settle();
  assert.ok(a.metrics.aeRounds > 0, 'anti-entropy is invisible when it works; the counter is the proof');
  const perPeer = b.metrics.addrs().find((t) => t.addr === a.addr);
  assert.ok(perPeer && perPeer.packetsIn > 0, 'traffic is attributed per peer, not just per cluster');
  await c.stopAll();
});

test('diagnostics describe one consistent instant of the node', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);
  await a.set('k', 'v');
  await settle();

  const d = a.diagnostics();
  assert.equal(d.running, true);
  assert.equal(d.peers.length, 1);
  assert.equal(d.peers[0].addr, b.addr);
  assert.equal(d.store.keys, 1);
  assert.equal(d.cluster, 'test');
  assert.equal(d.keyFingerprint.length, 8);
  assert.ok(d.topology.nodes.length >= 2);
  await c.stopAll();
});
