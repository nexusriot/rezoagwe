'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const os = require('node:os');
const path = require('node:path');
const fs = require('node:fs');
const { spawn, spawnSync } = require('node:child_process');

const { NodeEngine } = require('../src/core/node-engine');

/**
 * The only test that proves this client can actually join a rezoagwe cluster.
 *
 * Everything else here asserts against a second copy of the same rules; this
 * builds the real Go binaries, runs a real rendezvous and a real peer on real
 * sockets, and drives them through the Go node's own HTTP gateway. If the
 * framing, the field names or the last-write-wins tiebreak had drifted, this is
 * where it shows.
 *
 * Off by default because it compiles Go: run it with
 *   REZOAGWE_GO_INTEROP=1 npm run test:interop
 */

const ENABLED = process.env.REZOAGWE_GO_INTEROP === '1';
const REPO = path.join(__dirname, '..', '..');
const PSK = 'interop-secret';
const CLUSTER = 'interop';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * A port nothing is listening on, asked of the kernel rather than picked.
 *
 * Fixed ports made this test lie once already: another service on the machine
 * held the gateway port, the Go node exited without one, and the run failed
 * several steps later with "no peer" — after a health check that had happily
 * passed against the stranger's own /health.
 */
function freePort() {
  // listen() binds asynchronously, so the address is only there once it has
  // fired: reading it a line later hands back null, and the port becomes the
  // string "undefined" on a command line.
  return new Promise((resolve, reject) => {
    const server = require('node:net').createServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      server.close(() => resolve(port));
    });
  });
}

/** Confirms the gateway on this port is a rezoagwe node, not whatever else answered. */
async function ourGateway(httpAddr, cluster) {
  const res = await fetch(`http://${httpAddr}/health`);
  if (res.status !== 200) return false;
  const status = await res.json();
  return status.cluster === cluster && typeof status.node_id === 'string';
}

async function waitFor(what, check, timeoutMs = 20_000) {
  const deadline = Date.now() + timeoutMs;
  let last;
  while (Date.now() < deadline) {
    try {
      if (await check()) return;
    } catch (e) {
      last = e;
    }
    await sleep(200);
  }
  throw new Error(`timed out waiting for ${what}${last ? `: ${last.message}` : ''}`);
}

test('the Electron client joins a real Go cluster and replicates both ways', { skip: !ENABLED }, async (t) => {
  const BOOTSTRAP_PORT = await freePort();
  const goNodePort = await freePort();
  const GO_NODE = `127.0.0.1:${goNodePort}`;
  const GO_HTTP = `127.0.0.1:${await freePort()}`;
  const JS_PORT = await freePort();
  const http = async (method, urlPath, body, headers) => {
    const res = await fetch(`http://${GO_HTTP}${urlPath}`, { method, body, headers });
    return { status: res.status, text: await res.text() };
  };

  const goBin = fs.mkdtempSync(path.join(os.tmpdir(), 'rezoagwe-go-'));
  const build = (pkg, out) => {
    const result = spawnSync('go', ['build', '-o', path.join(goBin, out), pkg], { cwd: REPO, encoding: 'utf8' });
    assert.equal(result.status, 0, `go build ${pkg} failed: ${result.stderr}`);
  };
  build('./cmd/bootstrap', 'bootstrap');
  build('./cmd/discovery', 'discovery');

  const processes = [];
  const run = (bin, args) => {
    const child = spawn(path.join(goBin, bin), args, { stdio: ['ignore', 'pipe', 'pipe'] });
    child.stdout.on('data', () => {});
    child.stderr.on('data', () => {});
    processes.push(child);
    return child;
  };

  run('bootstrap', [
    '-headless', `-port=${BOOTSTRAP_PORT}`, `-psk=${PSK}`, `-cluster=${CLUSTER}`, '-data=-',
  ]);
  run('discovery', [
    '-headless', `-node=${GO_NODE}`, `-bootstrap=127.0.0.1:${BOOTSTRAP_PORT}`, `-psk=${PSK}`,
    `-cluster=${CLUSTER}`, '-nick=golang', `-http=${GO_HTTP}`, '-data=-',
  ]);

  const engine = new NodeEngine({
    advertiseHost: '127.0.0.1',
    port: JS_PORT,
    nick: 'electron',
    seeds: [`127.0.0.1:${BOOTSTRAP_PORT}`],
    psk: PSK,
    cluster: CLUSTER,
    gossipIntervalMs: 1000,
    heartbeatIntervalMs: 1000,
  }, null);

  t.after(async () => {
    await engine.stop().catch(() => {});
    for (const child of processes) child.kill('SIGTERM');
    await sleep(300);
    fs.rmSync(goBin, { recursive: true, force: true });
  });

  await waitFor('the Go node’s own gateway', () => ourGateway(GO_HTTP, CLUSTER));

  await engine.start();
  await engine.joining;

  // 1. The rendezvous handshake: each side has to end up knowing the other.
  await waitFor('the Go node to appear as a peer here', async () => engine.peerAddrs().includes(GO_NODE));
  await waitFor('this client to appear in the Go node’s peer list', async () => {
    const { text } = await http('GET', '/peers');
    return text.includes(`127.0.0.1:${JS_PORT}`);
  });

  // 2. A write made here has to reach a store nobody replayed it to by hand.
  await engine.set('from-electron', 'hello-go', 0);
  await waitFor('the write to reach the Go node', async () => {
    const { status, text } = await http('GET', '/kv/from-electron');
    return status === 200 && text === 'hello-go';
  });

  // 3. And the reverse: a write on the Go side has to land here.
  await http('PUT', '/kv/from-go', 'hello-electron');
  await waitFor('the Go write to reach this client', async () => engine.store.get('from-go') === 'hello-electron');

  // 4. A delete is a versioned tombstone on both sides, not a local disappearance.
  await http('DELETE', '/kv/from-go');
  await waitFor('the delete to replicate', async () => engine.store.get('from-go') === undefined);

  // 5. A TTL is applied deterministically by each replica with nothing on the wire.
  await engine.set('short-lived', 'value', 2);
  await waitFor('the TTL key to reach Go', async () => (await http('GET', '/kv/short-lived')).status === 200);
  await waitFor('both replicas to expire it on their own', async () => {
    const gone = (await http('GET', '/kv/short-lived')).status === 404;
    engine.store.sweepExpired();
    return gone && engine.store.get('short-lived') === undefined;
  });

  // 6. Chat shares one vocabulary across every front end.
  await engine.submit('hello from the desktop client');
  await waitFor('the chat line to reach Go', async () => {
    const { text } = await http('GET', '/chat');
    return text.includes('hello from the desktop client');
  });

  // 7. Anti-entropy: a write made while this node is deaf still converges.
  const realHandle = engine.handlePacket.bind(engine);
  engine.handlePacket = () => {};
  await http('PUT', '/kv/while-deaf', 'repaired');
  await sleep(1500);
  assert.equal(engine.store.get('while-deaf'), undefined, 'the write really was missed');
  engine.handlePacket = realHandle;
  await waitFor('a digest exchange to repair the miss', async () => {
    await engine.gossipTo(GO_NODE);
    await sleep(400);
    return engine.store.get('while-deaf') === 'repaired';
  });

  // 8. A clean shutdown is announced rather than timed out.
  await engine.stop();
  await waitFor('the Go node to drop this client on goodbye', async () => {
    const { text } = await http('GET', '/peers');
    return !text.includes(`127.0.0.1:${JS_PORT}`);
  }, 10_000);
});

test('a client on another cluster cannot read this one', { skip: !ENABLED }, async (t) => {
  // The same network, the same rendezvous address, a different cluster name:
  // the only thing between a stranger and the data.
  const bootstrapPort = await freePort();
  const goNodePort = await freePort();
  const goHttp = `127.0.0.1:${await freePort()}`;
  const strangerPort = await freePort();

  const goBin = fs.mkdtempSync(path.join(os.tmpdir(), 'rezoagwe-go-'));
  const build = (pkg, out) => spawnSync('go', ['build', '-o', path.join(goBin, out), pkg], { cwd: REPO });
  build('./cmd/bootstrap', 'bootstrap');
  build('./cmd/discovery', 'discovery');

  const processes = [];
  const run = (bin, args) => {
    const child = spawn(path.join(goBin, bin), args, { stdio: 'ignore' });
    processes.push(child);
  };
  run('bootstrap', ['-headless', `-port=${bootstrapPort}`, `-psk=${PSK}`, `-cluster=${CLUSTER}`, '-data=-']);
  run('discovery', [
    '-headless', `-node=127.0.0.1:${goNodePort}`, `-bootstrap=127.0.0.1:${bootstrapPort}`, `-psk=${PSK}`,
    `-cluster=${CLUSTER}`, `-http=${goHttp}`, '-data=-',
  ]);

  const stranger = new NodeEngine({
    advertiseHost: '127.0.0.1',
    port: strangerPort,
    nick: 'stranger',
    seeds: [`127.0.0.1:${bootstrapPort}`],
    psk: PSK,
    cluster: 'not-the-same-cluster',
    heartbeatIntervalMs: 1000,
  }, null);

  t.after(async () => {
    await stranger.stop().catch(() => {});
    for (const child of processes) child.kill('SIGTERM');
    await sleep(300);
    fs.rmSync(goBin, { recursive: true, force: true });
  });

  await waitFor('the second Go node’s own gateway', () => ourGateway(goHttp, CLUSTER));
  await fetch(`http://${goHttp}/kv/secret`, { method: 'PUT', body: 'do-not-share' });

  await stranger.start();
  await stranger.joining;
  await sleep(3000);

  assert.equal(stranger.store.get('secret'), undefined, 'a foreign cluster name must not decode the data');
  assert.equal(stranger.peerAddrs().length, 0, 'and the roster reply must not authenticate either');
});
