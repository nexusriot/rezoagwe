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
  // A value that cannot cross the wire unchanged has to be refused by *both*
  // implementations, or one of them creates a split the other cannot see.
  //
  // Found on a three-node cluster of real hardware: this client accepted a
  // value holding an unpaired surrogate, the Go node stored U+FFFD and the
  // Android node stored "?" — three values at one version, which anti-entropy
  // compares and is right to call agreement. Worse, this client hashed the
  // normalised form, so the consistency check reported agreement with a peer
  // holding something else.
  const lone = `lone-${String.fromCharCode(0xd800)}-end`;
  assert.equal(await engine.set('corner/lone', lone, 0).then(() => 'accepted', () => 'refused'), 'refused',
    'this client accepted a value that cannot reach a peer intact');
  assert.equal(engine.store.get('corner/lone'), undefined, 'a refused value was stored anyway');

  // And the Go node refuses the same thing through its gateway, with a status
  // a client can act on rather than a silent success.
  const goRefusal = await http('PUT', '/kv/corner/lone', Buffer.from('lone-\xed\xa0\x80-end', 'binary'));
  assert.equal(goRefusal.status, 422, `the Go node answered ${goRefusal.status} for an unrepresentable value`);
  assert.equal((await http('GET', '/kv/corner/lone')).status, 404, 'the Go node stored it anyway');

  // Everything that *can* cross the wire still must, byte for byte: the
  // refusal is about bytes that cannot arrive as they left, not unusual ones.
  for (const [key, value] of Object.entries({
    'corner/emoji': '🙂 ünïcödé 日本語',
    'corner/control': '\u0000\u0001\u0002',
    'corner/quotes': '"q" \\b\\',
    'corner/newlines': 'one\ntwo\r\nthree',
    'corner/replacement': '\ufffd',
  })) {
    await engine.set(key, value, 0);
    // eslint-disable-next-line no-await-in-loop
    await waitFor(`${key} to reach the Go node unchanged`, async () => {
      const { status, text } = await http('GET', `/kv/${encodeURIComponent(key).replace(/%2F/g, '/')}`);
      return status === 200 && text === value;
    });
  }

  await engine.submit('hello from the desktop client');
  await waitFor('the chat line to reach Go', async () => {
    const { text } = await http('GET', '/chat');
    return text.includes('hello from the desktop client');
  });

  // 6b. …and the sequence it carries survives the hop. A port that dropped
  // the field would still deliver the line, so "it arrived" proves nothing on
  // its own: the evidence is that Go counted no gap and no duplicate, which it
  // could only do by reading this node's seq and node id.
  const chatMetrics = (await http('GET', '/metrics')).text;
  const chatCounter = (name) => {
    const m = new RegExp(`^${name} (\\d+)$`, 'm').exec(chatMetrics);
    assert.ok(m, `${name} is not exposed`);
    return Number(m[1]);
  };
  assert.equal(chatCounter('rezoagwe_chat_gaps_total'), 0,
    'Go claims a gap in a conversation that lost nothing');
  assert.equal(chatCounter('rezoagwe_chat_duplicates_total'), 0,
    'Go counted a duplicate with nothing re-sending anything');

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

  // 8. A value too large for a datagram, both ways, over real sockets.
  //
  // The datagram is refused on size, so the frame has to travel as a stream
  // and arrive whole, framed by one implementation and parsed by the other.
  // Four megabytes rather than a few hundred kilobytes so the frame outgrows
  // the socket buffer and is written in several passes.
  //
  // What this does and does not prove is worth being exact about. It proves
  // the two implementations agree on the wire for a value no packet can
  // carry. It does not isolate either of the bugs that lived here — a sender
  // that dropped the frame, and one that destroyed the socket before it had
  // flushed — because the cluster has more than one way to deliver a missing
  // key, and anti-entropy or a state sync repairs what the write path lost.
  // That is the system working; it is also why a test at this level cannot
  // tell you which path carried it. The deterministic guards are the
  // in-process ones: cluster.test.js for the flush, and the Go suite for the
  // fallback and the deadline.
  const big = 'o'.repeat(4 * 1024 * 1024);
  await engine.set('big-from-electron', big, 0);
  await waitFor('the oversized value to reach the Go node whole', async () => {
    const { status, text } = await http('GET', '/kv/big-from-electron');
    return status === 200 && text.length === big.length && text === big;
  });

  const bigFromGo = 'g'.repeat(4 * 1024 * 1024);
  await http('PUT', '/kv/big-from-go', bigFromGo);
  await waitFor('the Go node’s oversized value to arrive whole', async () =>
    engine.store.get('big-from-go') === bigFromGo);

  // And carrying it cost neither side a send error: the counter climbing was
  // the only evidence the first time, and it is still the only evidence that
  // the datagram path is being retried for a frame that cannot fit it.
  assert.equal(engine.metrics.sendErrors, 0,
    'this client counted a send error while replicating an oversized value');
  const { text: metrics } = await http('GET', '/metrics');
  const goSendErrors = Number(/^rezoagwe_send_errors_total (\d+)/m.exec(metrics)?.[1] ?? 0);
  assert.equal(goSendErrors, 0, 'the Go node counted a send error while replicating an oversized value');

  // 9. A clean shutdown is announced rather than timed out.
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

/**
 * A real Go node joining *this* client's rendezvous.
 *
 * Every other case here points the JavaScript at a Go rendezvous. This is the
 * direction that was broken: the Go service reported the address it observed
 * and this one did not, so a Go node run with `-advertise auto` against the
 * desktop client resolved to the wildcard and published `0.0.0.0:<port>` to the
 * whole cluster — an address no peer can dial. Nothing failed anywhere: Go
 * simply found no address to adopt and said so on stderr while carrying on.
 *
 * `-doctor` is the whole exchange in one process: it starts a node, waits out
 * two heartbeats, prints the report and exits.
 */
test('a Go node learns from this client’s rendezvous where it is seen from', { skip: !ENABLED }, async (t) => {
  const bootstrapPort = await freePort();
  const goNodePort = await freePort();

  const goBin = fs.mkdtempSync(path.join(os.tmpdir(), 'rezoagwe-go-'));
  const build = spawnSync('go', ['build', '-o', path.join(goBin, 'discovery'), './cmd/discovery'], {
    cwd: REPO, encoding: 'utf8',
  });
  assert.equal(build.status, 0, `go build failed: ${build.stderr}`);

  const { BootstrapServer } = require('../src/core/bootstrap-server');
  const rendezvous = new BootstrapServer(
    { port: bootstrapPort, psk: PSK, cluster: CLUSTER, nodeTimeoutMs: 3_600_000 }, null, {},
  );
  t.after(async () => {
    await rendezvous.stop().catch(() => {});
    fs.rmSync(goBin, { recursive: true, force: true });
  });
  await rendezvous.start();

  // Bound to a wildcard and told to ask: a node in this state knows every
  // address its machine has and not which one its packets leave by.
  //
  // spawn, never spawnSync: the rendezvous being tested runs in *this* process,
  // and spawnSync blocks the event loop for as long as the child lives. The
  // kernel still completes the TCP handshake into the backlog, so Go sees a
  // connected socket, writes its request and times out reading — which looks
  // exactly like a service that accepted the stream and refused to answer.
  const report = await new Promise((resolve, reject) => {
    const child = spawn(path.join(goBin, 'discovery'), [
      '-headless', `-node=0.0.0.0:${goNodePort}`, '-advertise=auto',
      `-bootstrap=127.0.0.1:${bootstrapPort}`, `-psk=${PSK}`, `-cluster=${CLUSTER}`,
      '-nick=golang', '-data=-', '-doctor',
    ], { stdio: ['ignore', 'pipe', 'pipe'] });
    let out = '';
    child.stdout.on('data', (d) => { out += d; });
    child.stderr.on('data', (d) => { out += d; });
    child.once('error', reject);
    child.once('close', () => resolve(out));
    const kill = setTimeout(() => child.kill('SIGKILL'), 40_000);
    child.once('close', () => clearTimeout(kill));
  });
  assert.doesNotMatch(
    report, /no seed reported an address/,
    'this rendezvous has to answer with the address it saw',
  );
  assert.match(
    report, /advertising 127\.0\.0\.1:\d+ \(-advertise\)/,
    'and `auto` has to resolve to that host with the port the node binds, not to the wildcard',
  );
  assert.doesNotMatch(report, /advertising 0\.0\.0\.0/, 'the wildcard is an address no peer can dial');
});
