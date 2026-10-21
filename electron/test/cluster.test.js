'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const { NodeEngine, STRANGER_QUIET_WINDOW_MS } = require('../src/core/node-engine');
const { ID_CONFLICT_FLIPS, healthChecks, Severity } = require('../src/core/diagnostics');
const { BootstrapServer } = require('../src/core/bootstrap-server');
const { MemNetwork } = require('../src/net/mem');
const { MAX_DATAGRAM_PAYLOAD } = require('../src/net/udp');
const W = require('../src/proto/wire');

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

/**
 * A value too big for one datagram still has to reach the peer.
 *
 * Found on a live LAN cluster of a Go node, this client and the Android app: a
 * 200 KB write was accepted, reported success, and then never replicated.
 * Every gossip tick retried the same oversized datagram, the OS refused it,
 * and the node counted a send error — so the one mechanism meant to repair
 * divergence was the mechanism generating it, and the cluster stayed split for
 * as long as the key existed with nothing but a counter to say so.
 */
test('a value too large for a datagram replicates over a stream', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  const big = 'x'.repeat(MAX_DATAGRAM_PAYLOAD + 1);
  await a.set('big', big);
  await settle(120);

  assert.equal(b.store.get('big'), big, 'the peer never received the oversized value');
  assert.equal(a.metrics.sendErrors, 0, 'the stream fallback still counted a send error');
  await c.stopAll();
});

test('anti-entropy repairs an oversized value, not just the write path', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  // A partition, not packet loss: loss only drops datagrams, and the point of
  // the fix is that this write no longer travels as one.
  c.net.partition(a.addr, b.addr);
  const big = 'y'.repeat(MAX_DATAGRAM_PAYLOAD + 1);
  await a.set('big', big);
  await settle();
  assert.equal(b.store.get('big'), undefined, 'the write crossed a partition');

  c.net.heal(a.addr, b.addr);
  await a.antiEntropyRound(b.addr);
  await settle(120);
  assert.equal(b.store.get('big'), big, 'anti-entropy never repaired the oversized value');
  await c.stopAll();
});

test('with no stream there is nowhere to put an oversized value, and it counts as an error', async () => {
  const c = cluster({ streams: false });
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  await a.set('big', 'z'.repeat(MAX_DATAGRAM_PAYLOAD + 1));
  await settle(120);

  assert.equal(b.store.get('big'), undefined);
  assert.ok(a.metrics.sendErrors > 0, 'a drop with no fallback must still be reported');
  await c.stopAll();
});

/**
 * "They differ somewhere in bucket 7" is where a report used to stop, which on
 * a store of a few hundred keys is a sixteenth of the keyspace to go and read
 * by hand. The check knows which key it is; it just never said.
 */
test('a consistency report names the diverging key', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);
  for (const k of ['alpha', 'beta', 'gamma', 'delta']) await a.set(k, 'shared');
  await settle();

  c.net.partition(a.addr, b.addr);
  await b.set('beta', 'only-on-b');
  await settle();
  c.net.heal(a.addr, b.addr);

  const report = await a.checkConsistency();
  assert.equal(report.converged, false, 'the report claims convergence over a key they hold differently');
  const peer = report.peers[0];
  assert.equal(peer.differences.length, 1, `named ${JSON.stringify(peer.differences)}`);
  assert.equal(peer.differences[0].key, 'beta');
  assert.ok(peer.differences[0].local && peer.differences[0].remote,
    'both sides hold the key, so both versions should be named');
  assert.notEqual(peer.differences[0].local, peer.differences[0].remote);
  await c.stopAll();
});

// A key one replica has never seen is the divergence that matters most — a
// write that never arrived — and it cannot be found by walking the local
// store, which is the direction a naive diff takes.
test('a report names a key this node has never seen', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  c.net.partition(a.addr, b.addr);
  await b.set('orphan', 'never-arrived');
  await settle();
  c.net.heal(a.addr, b.addr);

  const peer = (await a.checkConsistency()).peers[0];
  const found = peer.differences.find((d) => d.key === 'orphan');
  assert.ok(found, `a key only the peer holds was not named: ${JSON.stringify(peer.differences)}`);
  assert.equal(found.local, '', 'this node never saw the key, so its version must be empty');
  assert.ok(found.remote, "the peer's version should be named");
  await c.stopAll();
});

test('a report says which side holds a tombstone', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);
  await a.set('doomed', 'v');
  await settle();

  c.net.partition(a.addr, b.addr);
  await b.delete('doomed');
  await settle();
  c.net.heal(a.addr, b.addr);

  const peer = (await a.checkConsistency()).peers[0];
  assert.equal(peer.differences.length, 1);
  assert.equal(peer.differences[0].localDeleted, false, 'this node holds the value');
  assert.equal(peer.differences[0].remoteDeleted, true, 'the peer holds a tombstone and it must say so');
  await c.stopAll();
});

test('an agreeing peer is given no differences to read', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);
  await a.set('k', 'v');
  await settle();

  const report = await a.checkConsistency();
  assert.equal(report.converged, true);
  assert.equal(report.peers[0].differences.length, 0);
  await c.stopAll();
});

/**
 * Saying the same thing twice is not a duplicate to be cleaned up.
 *
 * The merge deduplicated on the entry itself, and nothing in a chat entry
 * distinguishes two identical lines — same text, same sender, same second. A
 * joiner syncing a conversation where someone said "ok" twice was handed one
 * of them. The Go node had it worse: the second copy was deleted from a log
 * that already held it.
 */
test('a line genuinely said twice survives a state sync', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  await a.submit('ok');
  await a.submit('ok'); // same text, same sender, same second
  await settle();
  const saidTwice = (node) => node.chatLog.filter((e) => e.text === 'ok').length;
  assert.equal(saidTwice(a), 2, 'the writer should hold both lines');

  // A joiner pulls the whole conversation and must see both.
  const d = await c.make('carol');
  d.addPeer(a.addr);
  await d.requestStateFrom(a.addr);
  await settle(100);
  assert.equal(saidTwice(d), 2, 'the joiner was handed one copy of a line said twice');

  // And syncing again adds nothing.
  await d.requestStateFrom(a.addr);
  await settle(100);
  assert.equal(saidTwice(d), 2, 'a repeated snapshot duplicated the conversation');
  await c.stopAll();
});

/**
 * A one-way stream send must wait for the flush before closing.
 *
 * Found by re-running the end-to-end suite: `write()` buffers and
 * `destroy()` discards whatever is still queued, so the stream fallback
 * worked for every frame small enough to land in the socket buffer in one go
 * and silently truncated the ones that did not. The receiver read a length
 * prefix promising more than arrived and counted a stream error; the entry
 * never replicated. 200 KB was fine, tens of megabytes were not, which is
 * why the earlier tests all passed.
 */
test('a frame too large to flush in one go still arrives whole', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  // Comfortably past any socket buffer, so the write cannot complete
  // synchronously and the close has something to race.
  const big = 'x'.repeat(4 * 1024 * 1024);
  await a.set('big', big);
  await settle(300);

  assert.equal(b.store.get('big')?.length, big.length,
    'the peer received a truncated value, or none');
  assert.equal(a.metrics.streamErrors, 0, 'the send reported a stream error');
});

/**
 * And the same for a state response, which is a whole store rather than one
 * entry: the responder used a fixed grace period before destroying the
 * connection, which a large reply outgrows.
 */
test('a large state response is not truncated by the close that follows it', async () => {
  const c = cluster();
  const a = await c.make('alice');
  for (let i = 0; i < 40; i++) await a.set(`k${i}`, 'v'.repeat(100 * 1024));

  const joiner = await c.make('carol');
  joiner.addPeer(a.addr);
  await joiner.requestStateFrom(a.addr);
  await settle(400);

  assert.equal(joiner.store.size(), 40, 'the joiner got a partial store');
  assert.equal(joiner.metrics.streamErrors, 0, 'the joiner saw a broken stream');
});

/**
 * The whole feature, between two nodes over a network that drops a packet.
 *
 * Before the sequence, the dropped line was gone and nothing anywhere said so:
 * the author's log showed it sent, the receiver's simply never had it, and no
 * counter moved at either end.
 */
test('a dropped chat line is detected and repaired', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  await a.sendChat('first');
  await settle();

  // Exactly one line lost — the case that used to be invisible at both ends.
  c.net.partition(a.addr, b.addr);
  await a.sendChat('the line that never arrives');
  await settle();
  c.net.heal(a.addr, b.addr);

  await a.sendChat('third');
  await settle();

  assert.equal(b.metrics.chatGapsFound, 1, 'the loss was still invisible');
  assert.equal(b.chatGapStats()[0].outstanding, 1);

  // The repair: b asks the author, which is the only node that can answer.
  await b.pullChatGaps();
  await settle();
  await settle();

  assert.equal(b.metrics.chatRecovered, 1);
  assert.equal(b.chatGapStats()[0].outstanding, 0);

  // And it is back where it was said, not after the line that followed it.
  const said = b.chat().filter((e) => e.sender === a.addr && e.kind === '').map((e) => e.text);
  assert.deepEqual(said, ['first', 'the line that never arrives', 'third']);
  await c.stopAll();
});

/**
 * The repair and the original can both land — a late datagram and a backlog
 * racing — and the user must still see the line once.
 */
test('a repaired line is not delivered twice', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  await a.sendChat('first');
  await settle();
  c.net.partition(a.addr, b.addr);
  await a.sendChat('only once');
  await settle();
  c.net.heal(a.addr, b.addr);
  await a.sendChat('third');
  await settle();

  await b.pullChatGaps();
  await settle();
  await settle();
  assert.equal(b.metrics.chatRecovered, 1, 'the backlog did not fill the gap');

  // And now the original turns up anyway, as a late datagram would.
  await a.handleChat({
    sender: a.addr, node: a.nodeId, nick: 'alice', text: 'only once',
    ts: Math.floor(Date.now() / 1000), seq: 2, action: false, to: '',
  });
  await b.handleChat({
    sender: a.addr, node: a.nodeId, nick: 'alice', text: 'only once',
    ts: Math.floor(Date.now() / 1000), seq: 2, action: false, to: '',
  });
  assert.ok(b.metrics.chatDuplicates > 0, 'the late original was not recognised');
  assert.equal(b.chat().filter((e) => e.text === 'only once').length, 1);
  await c.stopAll();
});

/**
 * A backlog may only fill a gap this node is already waiting for.
 *
 * Found on hardware: one backlog carrying a line attributed to another peer,
 * with a large sequence, was accepted — which raised that author's high-water
 * mark so far that every genuine line it sent afterwards sorted below it and
 * was dropped as a duplicate. One datagram silenced an author at that node
 * permanently, through the mechanism built to stop chat going missing.
 */
test('a backlog cannot introduce a line nobody asked for', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  await a.sendChat('genuine one');
  await settle();

  b.handleChatBacklog({
    from: a.addr,
    messages: [{
      sender: '10.0.0.9:3137', node: 'a-third-party', nick: 'mallory',
      text: 'UNASKED', ts: Math.floor(Date.now() / 1000), seq: 900001, action: false, to: '',
    }],
    unavailable: [],
  });

  assert.ok(b.metrics.chatUnsolicited > 0, 'the unasked-for line was not refused');
  assert.equal(b.chat().filter((e) => e.text === 'UNASKED').length, 0,
    'a backlog introduced a line nobody asked for');

  // And the author it was attributed to is not silenced.
  await a.sendChat('genuine two');
  await settle();
  assert.equal(b.chat().filter((e) => e.text === 'genuine two').length, 1);
  await c.stopAll();
});

/**
 * The same guard refuses a line this node does want, offered by someone other
 * than its author — only the author can be sure what its own seq 7 was.
 */
test('a backlog from the wrong peer is refused', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  const third = await c.make('carol');
  await link(a, b);
  await link(b, third);

  await a.sendChat('first');
  await settle();
  c.net.partition(a.addr, b.addr);
  await a.sendChat('the missing one');
  await settle();
  c.net.heal(a.addr, b.addr);
  await a.sendChat('third');
  await settle();
  assert.equal(b.chatGapStats()[0].outstanding, 1, 'no gap to offer hearsay for');

  b.handleChatBacklog({
    from: third.addr,
    messages: [{
      sender: a.addr, node: a.nodeId, nick: 'alice', text: 'HEARSAY',
      ts: Math.floor(Date.now() / 1000), seq: 2, action: false, to: '',
    }],
    unavailable: [],
  });
  assert.ok(b.metrics.chatUnsolicited > 0, 'took one peer\'s word for another peer\'s line');
  assert.equal(b.chat().filter((e) => e.text === 'HEARSAY').length, 0);

  // The real author can still answer it.
  await b.pullChatGaps();
  await settle();
  await settle();
  assert.equal(b.chat().filter((e) => e.text === 'the missing one').length, 1);
  await c.stopAll();
});

/**
 * An author that has rotated the line out of its ring says so, so the receiver
 * stops asking and reports it lost rather than pending forever.
 */
test('an author says when a line is beyond recovery', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  await a.sendChat('one');
  await settle();

  // Seq 2 was never sent, so a cannot supply it: the same answer it would give
  // for a line that has aged out of its ring.
  b.chatGaps.observe({ sender: a.addr, node: a.nodeId, seq: 3, text: 'later' });
  await b.pullChatGaps();
  await settle();
  await settle();

  assert.ok(b.metrics.chatLost > 0, 'the author never said the line was gone');
  const stat = b.chatGapStats()[0];
  assert.equal(stat.outstanding, 0);
  assert.equal(stat.lost, 1);
  await c.stopAll();
});

/**
 * A node never receives its own broadcast: it appends its own lines on send,
 * and no peer echoes them back. Taking one is how a single bad line spreads —
 * it lands in this node's log, is handed out in every state snapshot
 * afterwards, and every node that syncs seeds its high-water mark for this
 * author from it. Measured on hardware: one forged sequence and three peers
 * stopped hearing the author entirely.
 */
test('a chat line attributed to ourself is refused', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  await a.sendChat('said by a');
  await settle();

  await a.handleChat({
    sender: a.addr, node: a.nodeId, nick: 'alice', text: 'NOT-SAID-BY-A',
    ts: Math.floor(Date.now() / 1000), seq: 900001, action: false, to: '',
  });
  assert.ok(a.metrics.chatSelfAttributed > 0, 'took a line claiming to be our own');
  assert.equal(a.chat().filter((e) => e.text === 'NOT-SAID-BY-A').length, 0);

  // The log is what every snapshot is built from, so keeping it out is what
  // stops one forged sequence spreading to everyone who syncs from here.
  assert.equal(a.chat().filter((e) => e.seq === 900001).length, 0);

  // And our own numbering is untouched.
  await a.sendChat('still audible');
  await settle();
  const mine = a.chat().find((e) => e.text === 'still audible');
  assert.equal(mine.seq, 2, 'the forgery moved our own counter');
  assert.equal(b.chat().filter((e) => e.text === 'still audible').length, 1);
  await c.stopAll();
});

/**
 * A node id is what every version is stamped with, so two nodes sharing one
 * write versions that compare equal while holding different values. Neither
 * side is newer, so neither accepts the other and the two stay split for good.
 * The engine cannot repair that; it has to notice it.
 */
test('a node that moved address rebinds its id once', async (t) => {
  const c = cluster();
  t.after(() => c.stopAll());
  const a = await c.make('a');

  a.setNodeId('peer-id', '127.0.0.1:3138');
  a.setNodeId('peer-id', '127.0.0.1:4000');
  a.setNodeId('peer-id', '127.0.0.1:4000');

  assert.equal(a.idToAddr.get('peer-id'), '127.0.0.1:4000');
  const conflicts = a.idConflicts();
  assert.equal(conflicts.length, 1);
  assert.equal(conflicts[0].flips, 1, 'a single move must not look like two live nodes');
});

test('two addresses claiming one id keep flipping', async (t) => {
  const c = cluster();
  t.after(() => c.stopAll());
  const a = await c.make('a');

  for (let i = 0; i < 6; i++) {
    a.setNodeId('shared-id', '127.0.0.1:3141');
    a.setNodeId('shared-id', '127.0.0.1:3142');
  }

  const [conflict] = a.idConflicts();
  assert.ok(conflict.flips >= ID_CONFLICT_FLIPS, `twins flipped only ${conflict.flips} times`);
  assert.notEqual(conflict.addrs[0], conflict.addrs[1]);
  assert.ok(a.diagnostics().idConflicts.length === 1, 'the conflict never reached the diagnostics');
});

// The unambiguous case: nothing legitimate makes a peer announce *our* id.
test('a peer claiming our own id is named, and we never accuse ourselves', async (t) => {
  const c = cluster();
  t.after(() => c.stopAll());
  const a = await c.make('a');

  a.setNodeId(a.nodeId, a.addr);
  assert.deepEqual([...a.impostors], [], 'the node accused itself');

  a.setNodeId(a.nodeId, '127.0.0.1:3142');
  assert.deepEqual(a.diagnostics().impostors, ['127.0.0.1:3142']);
});

/**
 * Two nodes started from copies of one state file share a node id, so their
 * writes to one key carry the identical version. Neither is newer, so neither
 * replica ever accepts the other and the split is permanent — the one
 * divergence anti-entropy cannot repair.
 *
 * The check used to report it as convergence, because the fingerprint folded
 * the version and not the value. Measured on a six-node cluster before the
 * fix: 45 seconds of anti-entropy, both sides still holding different values,
 * `converged: true`.
 */
test('a key held at one version with two values is reported, not hidden', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  const version = { counter: 100, node: '00000000-dead-beef' };
  a.store.apply({ action: 'set', key: 'split', value: 'written-by-a', version, expiresAt: 0, deletedAt: 0 });
  b.store.apply({ action: 'set', key: 'split', value: 'written-by-b', version, expiresAt: 0, deletedAt: 0 });
  await settle();

  const report = await a.checkConsistency();
  assert.equal(report.converged, false, 'a permanently split key was reported as converged');

  const diff = report.peers[0].differences.find((d) => d.key === 'split');
  assert.ok(diff, `the diverging key was not named: ${JSON.stringify(report.peers[0])}`);
  assert.equal(diff.sameVersion, true, 'the difference was not marked as one replication cannot repair');
  assert.equal(diff.local, diff.remote, 'both sides should report the identical version');
  await c.stopAll();
});

// A node is the only one that can answer a pull for its own seq 7, and it
// decides which lines are its own. Deciding that by address meant a node whose
// address had moved disowned everything it had said before the move: a phone
// that loses Wi-Fi falls back to advertising loopback, writes a line there, and
// comes back on its LAN address — then tells the asker its own line is gone for
// good while that line is on its own screen. Measured on hardware against the
// Go node: gaps 2, recovered 0, lost 2.
test('a node still serves chat lines it wrote under another address', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  await a.sendChat('first');
  await settle();

  // Off the air, with the address underneath the node changed.
  c.net.partition(a.addr, b.addr);
  const onAir = a.addr;
  // `addr` is derived from the advertise host, which is exactly what moves when
  // a phone loses Wi-Fi and falls back to loopback.
  const onAirHost = a.config.advertiseHost;
  a.config.advertiseHost = '127.0.0.2';
  await a.sendChat('said while the address had moved');
  a.config.advertiseHost = onAirHost;
  c.net.heal(onAir, b.addr);

  await a.sendChat('third');
  await settle();
  assert.equal(b.metrics.chatGapsFound, 1, 'the loss should have been named');

  await b.pullChatGaps();
  await settle();

  assert.equal(b.metrics.chatLost, 0, 'the author still held the line');
  assert.equal(b.metrics.chatRecovered, 1);
  assert.ok(
    b.chatLog.some((e) => e.text === 'said while the address had moved'),
    'the line never arrived',
  );
  await c.stopAll();
});

// Every path that learns a peer announces it, and eviction always announces a
// departure. The roster path announced nothing, so a peer adopted from a
// bootstrap roster and never heard from again produced a "left" for a node that
// had never been seen to join — a node leaving twice, in the one log a user
// actually watches.
test('a peer learned from a bootstrap roster is announced', async () => {
  const c = cluster();
  const a = await c.make('alice');

  await a.applyRoster({ peers: [{ addr: '127.0.0.1:3999', nick: 'ghost' }] });
  assert.ok(
    a.chatLog.some((e) => e.kind === 'system' && e.text.includes('127.0.0.1:3999') && e.text.includes('joined')),
    `the roster peer was adopted in silence: ${JSON.stringify(a.chatLog)}`,
  );

  // Only learning is news: a second roster with the same peer says nothing.
  const before = a.chatLog.length;
  await a.applyRoster({ peers: [{ addr: '127.0.0.1:3999', nick: 'ghost' }] });
  assert.equal(a.chatLog.length, before);
  await c.stopAll();
});

// The activity feed is a one-line-per-event log, and both a key and a value are
// arbitrary UTF-8. A newline in either broke the entry in two in every pane
// that shows it, so one event read as two. Only the value was ever previewed.
test('an activity entry stays on one line whatever the key holds', async () => {
  const c = cluster();
  const a = await c.make('alice');
  const b = await c.make('bob');
  await link(a, b);

  await a.set('line1\nline2', 'first\nsecond');
  await settle();

  const line = b.activity().find((l) => l.includes('line1'));
  assert.ok(line, `no activity entry named the key: ${JSON.stringify(b.activity())}`);
  assert.ok(!/[\n\t]/.test(line), `entry still breaks the line: ${JSON.stringify(line)}`);
  await c.stopAll();
});

// End to end over the real wire: the service observes, the roster carries, the
// node records, and the diagnosis names the address to use. Each half of that
// shipped in Go alone, so a node joining through this port's rendezvous had no
// way to learn it was advertising something nobody could reach.
test('a node learns from the rendezvous which address the cluster sees it at', async () => {
  const net = new MemNetwork();
  const bootstrapAddr = '127.0.0.1:9991';
  const bootstrap = new BootstrapServer(
    { port: 9991, psk: 'k', cluster: 'test', nodeTimeoutMs: 3_600_000 },
    null,
    { listen: async () => net.attach(bootstrapAddr) },
  );
  await bootstrap.start();

  // The node binds at one address and tells the cluster another — the wildcard
  // case, where a machine knows every address it has and not which one its
  // packets leave by.
  const bound = '127.0.0.1:3601';
  const engine = new NodeEngine(
    {
      advertiseHost: '10.9.9.9', port: 3601, nick: 'misconfigured',
      psk: 'k', cluster: 'test', seeds: [bootstrapAddr], ...QUIET,
    },
    null,
    { listen: async () => net.attach(bound) },
  );
  await engine.start();
  await engine.joining;
  await settle(60);

  // The join runs over a stream, whose source port is ephemeral: only the host
  // carries information, and that is what the rule compares.
  assert.match(engine.seenAt, /^127\.0\.0\.1:\d+$/, 'the rendezvous reported where the packets came from');
  assert.notEqual(engine.seenAt, bound, 'the observed port is ephemeral, not the bound one');
  assert.equal(engine.advertiseMismatch(), engine.seenAt, 'and its host disagrees with what peers are told');

  const checks = healthChecks(engine.diagnostics(), Date.now());
  const check = checks.find((c) => c.title.includes('does not see this node at'));
  assert.ok(check, 'the finding only a rendezvous can produce');
  assert.equal(check.severity, Severity.ERROR);
  assert.match(
    check.detail, /127\.0\.0\.1:3601/,
    'it names the observed host with the port the node binds, never the ephemeral one',
  );

  await engine.stop();
  await bootstrap.stop();
});

test('a node whose advertised address is the one observed is told nothing', async () => {
  const net = new MemNetwork();
  const bootstrapAddr = '127.0.0.1:9992';
  const bootstrap = new BootstrapServer(
    { port: 9992, psk: 'k', cluster: 'test', nodeTimeoutMs: 3_600_000 },
    null,
    { listen: async () => net.attach(bootstrapAddr) },
  );
  await bootstrap.start();
  const engine = new NodeEngine(
    {
      advertiseHost: '127.0.0.1', port: 3602, nick: 'fine',
      psk: 'k', cluster: 'test', seeds: [bootstrapAddr], ...QUIET,
    },
    null,
    { listen: async () => net.attach('127.0.0.1:3602') },
  );
  await engine.start();
  await engine.joining;
  await settle(60);

  // The source port differs from the bound one on every stream, so a rule that
  // compared addresses rather than hosts would accuse every correct node here.
  assert.match(engine.seenAt, /^127\.0\.0\.1:\d+$/);
  assert.notEqual(engine.seenAt, '127.0.0.1:3602');
  assert.equal(engine.advertiseMismatch(), '', 'the hosts agree, so there is nothing to report');
  const checks = healthChecks(engine.diagnostics(), Date.now());
  assert.ok(!checks.some((c) => c.title.includes('does not see this node at')));
  await engine.stop();
  await bootstrap.stop();
});

// The rendezvous service answers with a roster and is never a peer, so keying
// "sent packets without being a peer" on the peer table alone made a correctly
// configured node report its own seed as an anomaly — in the words of a NAT
// misconfiguration, and permanently. It shows whenever the roster comes back by
// datagram, which is the fallback taken when the stream exchange cannot be made.
test('the rendezvous service is not reported as an unknown source', async () => {
  const net = new MemNetwork();
  const addr = '127.0.0.1:3601';
  const seed = '127.0.0.1:9992';
  const engine = new NodeEngine(
    {
      advertiseHost: '127.0.0.1', port: 3601, nick: 'alice', psk: 'k', cluster: 'test', seeds: [seed], ...QUIET,
    },
    null,
    { listen: async () => net.attach(addr) },
  );
  await engine.start();
  await engine.joining;

  // The roster as it arrives on the datagram path.
  const frame = engine.codec.encode(W.Kind.BOOTSTRAP_ROSTER, { peers: [], seen_at: addr });
  engine.handlePacket({ from: seed, data: frame });
  await settle();

  const d = engine.diagnostics();
  assert.deepEqual(d.strangers.map((s) => s.addr), [],
    'the seed that answered with the roster is reported as an unknown source');
  const noise = healthChecks(d, Date.now()).filter((c) => c.title.includes('without being a peer'));
  assert.deepEqual(noise, [], 'a healthy node warns about its own rendezvous service');

  // A genuine unknown source is still reported.
  const hello = engine.codec.encode(W.Kind.HELLO, { from: '127.0.0.1:3999', nick: 'nobody', node_id: 'x' });
  engine.handlePacket({ from: '10.9.9.9:3137', data: hello });
  await settle();
  assert.ok(engine.diagnostics().strangers.some((s) => s.addr === '10.9.9.9:3137'),
    'a real stranger was lost with the fix');

  await engine.stop();
});

// The unknown-source list answers "who is talking to us wrongly *now*". A
// misconfigured node talks continuously and stays on it; one packet from an
// address that then goes quiet — a peer evicted a moment before its last
// gossip landed, or a node that simply left — is the other case entirely, and
// keeping those forever turned every departure into a permanent anomaly
// reported in the words of a NAT misconfiguration.
test('an unknown source that went quiet stops being reported', async () => {
  const net = new MemNetwork();
  const addr = '127.0.0.1:3611';
  const engine = new NodeEngine(
    { advertiseHost: '127.0.0.1', port: 3611, nick: 'alice', psk: 'k', cluster: 'test', ...QUIET },
    null,
    { listen: async () => net.attach(addr) },
  );
  await engine.start();
  await engine.joining;

  // The body claims one address while the packet comes from another, which is
  // what the list exists to catch.
  const hello = engine.codec.encode(W.Kind.HELLO, { from: '127.0.0.1:3999', nick: 'nobody', node_id: 'x' });
  engine.handlePacket({ from: '10.9.9.9:3137', data: hello });
  await settle();
  assert.equal(engine.diagnostics().strangers.length, 1, 'precondition: not recorded');

  // Still reported while it is talking: the check must keep working.
  engine.metrics.counters('10.9.9.9:3137').lastInMs = Date.now() - STRANGER_QUIET_WINDOW_MS / 2;
  assert.equal(engine.diagnostics().strangers.length, 1, 'a source still sending was dropped');

  // And forgotten once it has been quiet long enough.
  engine.metrics.counters('10.9.9.9:3137').lastInMs = Date.now() - STRANGER_QUIET_WINDOW_MS - 1000;
  assert.deepEqual(engine.diagnostics().strangers, [],
    'an address that went quiet is still reported');

  await engine.stop();
});
