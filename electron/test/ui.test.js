'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const S = require('../renderer/shared');
const G = require('../renderer/graph');
const { healthChecks, asReport, Severity } = require('../src/core/diagnostics');

/**
 * The rules a screenshot cannot check: which screen pairs with which, when a
 * second pane fits, how an age reads, where a node lands on the canvas — and,
 * above all, which counter means which misconfiguration.
 */

test('every screen has a companion, and none pairs with itself', () => {
  for (const screen of S.SCREENS) {
    const companion = S.companionOf(screen.id);
    assert.ok(S.SCREENS.some((s) => s.id === companion), `${screen.id} pairs with an unknown screen`);
    assert.notEqual(companion, screen.id, 'a split view must never show the same screen twice');
  }
});

test('the companion answers the question the main pane raises', () => {
  assert.equal(S.companionOf('peers'), 'graph', 'a peer list cannot show that two peers do not talk');
  assert.equal(S.companionOf('chat'), 'peers');
  assert.equal(S.companionOf('keys'), 'activity', 'a write is worth seeing replicate');
});

test('a second pane needs both width and height', () => {
  assert.ok(S.splitFits(1400, 900));
  assert.ok(!S.splitFits(900, 900), 'too narrow: neither pane would be usable');
  assert.ok(!S.splitFits(1400, 400), 'a short window has no room for two headers');
});

test('an uptime reads at the scale it deserves', () => {
  assert.equal(S.formatUptime(0), '0s');
  assert.equal(S.formatUptime(45), '45s');
  assert.equal(S.formatUptime(125), '2m05s');
  assert.equal(S.formatUptime(3725), '1h02m05s');
  assert.equal(S.formatUptime(-5), '0s', 'a clock that stepped back must not print a negative age');
});

test('byte counts stay short enough to sit in a row', () => {
  assert.equal(S.formatBytes(512), '512B');
  assert.equal(S.formatBytes(2048), '2.0kB');
  assert.equal(S.formatBytes(5 * 1024 * 1024), '5.0MB');
});

test('a peer keeps the same colour on every screen', () => {
  const first = S.colorFor('10.0.0.2:3137');
  assert.equal(first, S.colorFor('10.0.0.2:3137'));
  assert.notEqual(first, S.colorFor('10.0.0.3:3137'));
  assert.match(first, /^#[0-9a-f]{6}$/);
  assert.ok(S.colorFor(''), 'an empty seed still needs a colour rather than undefined');
});

test('the header says stopped, connected or degraded — never a bare "running"', () => {
  assert.deepEqual(S.statusLabel({ running: false, peers: 3 }), { label: 'STOPPED', tone: 'muted' });
  assert.deepEqual(S.statusLabel({ running: true, peers: 2 }), { label: 'CONNECTED', tone: 'ok' });
  // Running with nobody to talk to is the state worth flagging: it looks fine
  // and replicates nothing.
  assert.deepEqual(S.statusLabel({ running: true, peers: 0 }), { label: 'DEGRADED', tone: 'bad' });
});

test('a TTL reads as remaining time and never goes negative', () => {
  assert.equal(S.ttlRemaining(0, 1000), null);
  assert.equal(S.ttlRemaining(1060, 1000), 60);
  assert.equal(S.ttlRemaining(900, 1000), 0);
});

test('a node nothing has been heard from is drawn as stale', () => {
  const now = 1_000_000;
  assert.ok(!S.isStale(now - 5_000, now));
  assert.ok(S.isStale(now - 60_000, now));
  assert.ok(!S.isStale(0, now), 'never-seen is not the same as gone quiet');
});

test('the graph projection puts the centre in the middle and honours zoom and pan', () => {
  const size = { width: 400, height: 300 };
  const centre = G.project({ x: 0, y: 0 }, size, { scale: 1, panX: 0, panY: 0 });
  assert.deepEqual(centre, { x: 200, y: 150 });

  const edge = G.project({ x: 1, y: 0 }, size, { scale: 1, panX: 0, panY: 0 });
  assert.ok(edge.x > 200 && edge.x <= 400 - 1, 'the outermost node stays on the canvas with room for its label');

  const panned = G.project({ x: 0, y: 0 }, size, { scale: 1, panX: 30, panY: -10 });
  assert.deepEqual(panned, { x: 230, y: 140 });

  const zoomed = G.project({ x: 0.5, y: 0 }, size, { scale: 2, panX: 0, panY: 0 });
  assert.ok(zoomed.x > G.project({ x: 0.5, y: 0 }, size, { scale: 1, panX: 0, panY: 0 }).x);
});

test('a click finds the node under it, and empty space selects nothing', () => {
  const size = { width: 400, height: 300 };
  const view = { scale: 1, panX: 0, panY: 0 };
  const nodes = [
    { addr: 'self:1', x: 0, y: 0 },
    { addr: 'peer:2', x: 0.8, y: 0 },
  ];
  assert.equal(G.nodeAt({ x: 200, y: 150 }, nodes, size, view).addr, 'self:1');
  assert.equal(G.nodeAt({ x: 205, y: 155 }, nodes, size, view).addr, 'self:1', 'a pointer is coarser than a dot');
  assert.equal(G.nodeAt({ x: 20, y: 280 }, nodes, size, view), null);
});

test('the closest node wins when two are near the pointer', () => {
  const size = { width: 400, height: 300 };
  const view = { scale: 1, panX: 0, panY: 0 };
  const nodes = [{ addr: 'a', x: 0, y: 0 }, { addr: 'b', x: 0.1, y: 0 }];
  const b = G.project({ x: 0.1, y: 0 }, size, view);
  assert.equal(G.nodeAt(b, nodes, size, view).addr, 'b');
});

// ---- health checks ----------------------------------------------------------

const baseDiagnostics = (over = {}) => ({
  addr: '10.0.0.1:3137',
  heartbeatIntervalMs: 5000,
  cluster: 'test',
  keyFingerprint: 'deadbeef',
  pskSet: true,
  configuredPort: 3137,
  streamListener: true,
  advertiseHost: '',
  localIpv4: '10.0.0.1',
  running: true,
  uptimeSec: 60,
  evictThresholdMs: 15_000,
  tombstoneTtlSec: 0,
  seeds: ['10.0.0.9:9999'],
  peers: [{ addr: '10.0.0.2:3137', nick: 'bob', packetsIn: 5, packetsOut: 5, lastSeenMs: Date.now() }],
  strangers: [],
  store: { keys: 3, tombstones: 0, valueBytes: 10, clock: 4, historyKeys: 3, largestKey: '', largestValueBytes: 0, digestCursor: '' },
  metrics: {
    authFailures: 0, skewDrops: 0, malformedDrops: 0, replayDrops: 0, sendErrors: 0, lastSendError: null,
    packetsSent: 10, packetsReceived: 10, bytesSent: 100, bytesReceived: 100, aeRounds: 1, aePushed: 0,
    aePulled: 0, stateSyncOut: 0, stateSyncIn: 0, streamErrors: 0, kinds: [],
    rates: { packetsSent: 0, packetsReceived: 0, bytesSent: 0, bytesReceived: 0 },
  },
  topology: { nodes: [{ addr: '10.0.0.1:3137' }, { addr: '10.0.0.2:3137' }], links: [{ a: '10.0.0.1:3137', b: '10.0.0.2:3137', kind: 'direct' }] },
  chatLines: 2,
  activityLines: 2,
  dataFile: '/tmp/node.json',
  loadError: '',
  ...over,
});

const find = (checks, fragment) => checks.find((c) => c.title.includes(fragment));

test('a healthy node says so once, and not alongside a warning', () => {
  const checks = healthChecks(baseDiagnostics(), Date.now());
  assert.equal(checks[0].severity, Severity.OK);
  assert.equal(checks.filter((c) => c.severity === Severity.OK).length, 1);
});

test('failed authentication is reported as a key mismatch, with the fingerprint to compare', () => {
  // A counter says a packet was dropped; a check says which misconfiguration
  // drops packets that way.
  const checks = healthChecks(baseDiagnostics({ metrics: { ...baseDiagnostics().metrics, authFailures: 4 } }), Date.now());
  const check = find(checks, 'failed authentication');
  assert.equal(check.severity, Severity.ERROR);
  assert.match(check.detail, /pre-shared key/);
  assert.match(check.detail, /deadbeef/);
  assert.ok(!checks.some((c) => c.severity === Severity.OK), 'a healthy line beside an error would dilute it');
});

test('clock skew is called out separately from a wrong key', () => {
  const checks = healthChecks(baseDiagnostics({ metrics: { ...baseDiagnostics().metrics, skewDrops: 2 } }), Date.now());
  assert.match(find(checks, 'clock skew').detail, /30s/);
});

test('a node seconds old is not accused of anything yet', () => {
  // A peer answers on its own heartbeat and a seed replies when it feels like
  // it; flagging either immediately turns every start into a red screen.
  const young = { uptimeSec: 3 };
  const silentPeer = [{ addr: '10.0.0.2:3137', nick: '', packetsIn: 0, packetsOut: 9, lastSeenMs: Date.now() }];
  assert.ok(!find(healthChecks(baseDiagnostics({ ...young, peers: silentPeer }), Date.now()), 'never answered'));
  assert.ok(!find(healthChecks(baseDiagnostics({ ...young, peers: [] }), Date.now()), 'No peers learned'));
  // And once it has had time, it says so.
  assert.ok(find(healthChecks(baseDiagnostics({ uptimeSec: 30, peers: silentPeer }), Date.now()), 'never answered'));
  assert.ok(find(healthChecks(baseDiagnostics({ uptimeSec: 30, peers: [] }), Date.now()), 'No peers learned'));
});

test('a peer that never answers is a firewall, not packet loss', () => {
  const checks = healthChecks(baseDiagnostics({
    peers: [{ addr: '10.0.0.2:3137', nick: '', packetsIn: 0, packetsOut: 40, lastSeenMs: Date.now() }],
  }), Date.now());
  const check = find(checks, 'never answered');
  assert.equal(check.severity, Severity.ERROR);
  assert.match(check.detail, /firewall|advertised port/);
});

test('a split cluster is reported with the sizes of the groups', () => {
  const checks = healthChecks(baseDiagnostics({
    topology: {
      nodes: [{ addr: 'a:1' }, { addr: 'b:2' }, { addr: 'c:3' }, { addr: 'd:4' }],
      links: [{ a: 'a:1', b: 'b:2', kind: 'direct' }, { a: 'c:3', b: 'd:4', kind: 'mutual' }],
    },
  }), Date.now());
  const check = find(checks, 'partitioned');
  assert.equal(check.severity, Severity.ERROR);
  assert.match(check.detail, /2 \| 2/);
});

test('running with no peers names the likely cause, seeds or none', () => {
  const withSeeds = healthChecks(baseDiagnostics({ peers: [] }), Date.now());
  assert.match(find(withSeeds, 'No peers learned').detail, /10\.0\.0\.9:9999/);
  const without = healthChecks(baseDiagnostics({ peers: [], seeds: [] }), Date.now());
  assert.match(find(without, 'No peers and no seeds').detail, /Bootstrap role|seed/);
});

test('a loopback advertisement is flagged, since no peer can reach it', () => {
  const checks = healthChecks(baseDiagnostics({ addr: '127.0.0.1:3137' }), Date.now());
  assert.ok(find(checks, 'loopback'));
});

test('a peer going quiet is flagged before it is evicted, not after', () => {
  const now = Date.now();
  const checks = healthChecks(baseDiagnostics({
    peers: [{ addr: '10.0.0.2:3137', nick: 'bob', packetsIn: 3, packetsOut: 3, lastSeenMs: now - 10_000 }],
  }), now);
  assert.equal(find(checks, 'going stale').severity, Severity.WARN);
});

test('an unreadable state file is an error, not a silent fresh start', () => {
  const checks = healthChecks(baseDiagnostics({ loadError: 'Unexpected token' }), Date.now());
  const check = find(checks, 'State file');
  assert.equal(check.severity, Severity.ERROR);
  assert.match(check.detail, /empty store/);
});

test('a missing stream listener explains why a big store converges slowly', () => {
  const checks = healthChecks(baseDiagnostics({ streamListener: false }), Date.now());
  assert.match(find(checks, 'No stream listener').detail, /datagrams/);
});

test('running without a key is stated as no secrecy rather than left implied', () => {
  const checks = healthChecks(baseDiagnostics({ pskSet: false }), Date.now());
  assert.match(find(checks, 'No pre-shared key').detail, /no secrecy/);
});

test('a stopped node reports that first and claims nothing about health', () => {
  const checks = healthChecks(baseDiagnostics({ running: false }), Date.now());
  assert.equal(checks[0].title, 'Node stopped');
  assert.ok(!checks.some((c) => c.severity === Severity.OK));
});

test('the report carries what a bug report needs, in one paste', () => {
  const report = asReport(baseDiagnostics(), Date.now());
  for (const fragment of ['rezoagwe node diagnostics', '10.0.0.1:3137', 'deadbeef', 'peers (1)', 'checks']) {
    assert.ok(report.includes(fragment), `the report should mention ${fragment}`);
  }
});
