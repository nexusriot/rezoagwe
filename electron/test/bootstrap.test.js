'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const { BootstrapServer } = require('../src/core/bootstrap-server');
const { MemNetwork } = require('../src/net/mem');
const { Codec } = require('../src/proto/codec');
const W = require('../src/proto/wire');
const { Kind } = W;

const settle = (ms = 25) => new Promise((r) => setTimeout(r, ms));

function harness(config = {}) {
  const net = new MemNetwork();
  const addr = '127.0.0.1:9990';
  const server = new BootstrapServer(
    { port: 9990, psk: 'k', cluster: 'test', nodeTimeoutMs: 3_600_000, ...config },
    config.dataFile || null,
    { listen: async () => net.attach(addr), now: config.now },
  );
  // A stand-in client: it speaks the wire, so what it proves is what a real node
  // would experience rather than what an internal method does.
  const client = { codec: new Codec(config.psk || 'k', config.cluster || 'test'), replies: [] };
  const clientAddr = '127.0.0.1:4001';
  const transport = net.attach(clientAddr);
  transport.on('packet', (pkt) => {
    try {
      const frame = client.codec.decode(pkt.data);
      client.replies.push({ kind: frame.kind, body: JSON.parse(frame.body.toString('utf8')) });
    } catch (e) {
      client.replies.push({ error: e.reason || e.message });
    }
  });
  client.send = (kind, value) => transport.send(addr, client.codec.encode(kind, value));
  client.addr = clientAddr;
  return { net, server, client, addr };
}

test('a REGISTER puts a node on the roster, and a repeat is not a new arrival', async () => {
  const { server, client } = harness();
  await server.start();
  await client.send(Kind.BOOTSTRAP_REGISTER, W.encodeBootstrapRegister({ from: client.addr, nick: 'alice' }));
  await settle();
  assert.deepEqual(server.roster().map((n) => n.addr), [client.addr]);
  assert.equal(server.roster()[0].nick, 'alice');
  assert.equal(server.register(client.addr, 'alice'), false, 'a heartbeat is not a join');
  await server.stop();
});

test('a DISCOVER is answered with the roster, minus the requester', async () => {
  const { server, client } = harness();
  await server.start();
  server.register('127.0.0.1:5001', 'bob');
  server.register(client.addr, 'me');
  await client.send(Kind.BOOTSTRAP_DISCOVER, W.encodeBootstrapDiscover({ from: client.addr }));
  await settle();

  const reply = client.replies.find((r) => r.kind === Kind.BOOTSTRAP_ROSTER);
  assert.ok(reply, 'a joiner that gets no answer never finds the cluster');
  const roster = W.decodeBootstrapRoster(reply.body);
  assert.deepEqual(roster.peers.map((p) => p.addr), ['127.0.0.1:5001']);
  assert.equal(roster.peers[0].nick, 'bob', 'the nickname lets a joiner label peers before its first Hello');
  await server.stop();
});

test('asking for the roster is itself proof of life', async () => {
  // A joiner whose REGISTER was lost still ends up known, which is the whole
  // reason the two messages are not one.
  const { server, client } = harness();
  await server.start();
  await client.send(Kind.BOOTSTRAP_DISCOVER, W.encodeBootstrapDiscover({ from: client.addr }));
  await settle();
  assert.deepEqual(server.roster().map((n) => n.addr), [client.addr]);
  await server.stop();
});

test('a packet framed with another key is ignored in silence', async () => {
  const { server, net, addr } = harness();
  await server.start();
  const stranger = net.attach('127.0.0.1:4002');
  const wrong = new Codec('different', 'test');
  await stranger.send(addr, wrong.encode(Kind.BOOTSTRAP_REGISTER, { from: '127.0.0.1:4002' }));
  await settle();
  assert.equal(server.roster().length, 0, 'unauthenticated traffic on an open port is background noise');
  await server.stop();
});

test('the answer goes to the address a node advertises, not its source port', async () => {
  // A node bound to a wildcard address is reachable at what it advertises, while
  // the source port of its datagram may be ephemeral.
  const { server } = harness();
  const advertised = '10.0.0.5:3137';
  assert.equal(
    server.replyAddr('192.168.1.9:54321', Kind.BOOTSTRAP_DISCOVER, { from: advertised }),
    advertised,
  );
  assert.equal(
    server.replyAddr('192.168.1.9:54321', Kind.BOOTSTRAP_REGISTER, { from: advertised }),
    '192.168.1.9:54321',
    'a REGISTER needs no answer, so the source stays the fallback',
  );
});

test('a node that stops registering is dropped after the timeout', async () => {
  let now = 1_000_000;
  const { server } = harness({ nodeTimeoutMs: 30_000, now: () => now });
  server.register('127.0.0.1:5001', 'ghost');
  server.register('127.0.0.1:5002', 'alive');
  now += 40_000;
  server.register('127.0.0.1:5002', 'alive'); // still checking in
  const removed = server.removeStale();
  assert.deepEqual(removed.map((n) => n.addr), ['127.0.0.1:5001']);
  assert.deepEqual(server.roster().map((n) => n.addr), ['127.0.0.1:5002']);
});

test('the roster survives a restart, with every last-seen reset rather than expired', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rezoagwe-boot-'));
  const file = path.join(dir, 'bootstrap.json');
  fs.writeFileSync(file, JSON.stringify({
    nodes: [{ addr: '127.0.0.1:5001', nick: 'alice', lastSeen: 1 }],
  }));

  const net = new MemNetwork();
  const server = new BootstrapServer(
    { port: 9990, nodeTimeoutMs: 30_000 }, file, { listen: async () => net.attach('127.0.0.1:9990') },
  );
  assert.deepEqual(server.roster().map((n) => n.addr), ['127.0.0.1:5001']);
  // The service was down, so nobody could have checked in; expiring the whole
  // roster the instant it loads would make persistence pointless.
  assert.equal(server.removeStale().length, 0);
  fs.rmSync(dir, { recursive: true, force: true });
});

test('a corrupt roster file is not worth failing to start over', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rezoagwe-boot-'));
  const file = path.join(dir, 'bootstrap.json');
  fs.writeFileSync(file, 'not json at all');
  const net = new MemNetwork();
  const server = new BootstrapServer({ port: 9990 }, file, { listen: async () => net.attach('127.0.0.1:9990') });
  assert.deepEqual(server.roster(), []);
  fs.rmSync(dir, { recursive: true, force: true });
});

test('the roster is written when a node arrives or is renamed, not on every heartbeat', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rezoagwe-boot-'));
  const file = path.join(dir, 'bootstrap.json');
  const net = new MemNetwork();
  const server = new BootstrapServer({ port: 9990 }, file, { listen: async () => net.attach('127.0.0.1:9990') });

  server.register('127.0.0.1:5001', 'alice');
  await server.persister.flush();
  const first = server.persister.lastGen;
  server.register('127.0.0.1:5001', 'alice'); // heartbeat
  assert.equal(server.persister.lastGen, first, 'a heartbeat is not a roster change');
  server.register('127.0.0.1:5001', 'renamed');
  await server.persister.flush();
  assert.ok(server.persister.lastGen > first);

  const onDisk = JSON.parse(fs.readFileSync(file, 'utf8'));
  assert.equal(onDisk.nodes[0].nick, 'renamed');
  fs.rmSync(dir, { recursive: true, force: true });
});

test('status reports what the service is actually doing', async () => {
  const { server } = harness();
  assert.equal(server.status().running, false);
  await server.start();
  server.register('127.0.0.1:5001', 'a');
  const status = server.status();
  assert.equal(status.running, true);
  assert.equal(status.nodes, 1);
  assert.equal(status.port, 9990);
  await server.stop();
  assert.equal(server.status().running, false);
});
