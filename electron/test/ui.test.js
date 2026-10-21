'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const S = require('../renderer/shared');
const G = require('../renderer/graph');
const {
  healthChecks, asReport, Severity, SEND_ERROR_RECENT_WINDOW_MS,
} = require('../src/core/diagnostics');
const { sameHost } = require('../src/net/addr');

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

/**
 * The chat box sends on Enter, and the Android app has to as well.
 *
 * Pinned on both sides after the Compose field shipped with no ImeAction: its
 * keyboard offered a Done key that did nothing, so the same gesture worked in
 * the terminal client and here but not on a tablet, where a keyboard is the
 * only way to type. The assertion is cheap; the asymmetry was not obvious from
 * either file alone.
 */
test('the chat composer sends on Enter, not only on the button', () => {
  const renderer = fs.readFileSync(path.join(__dirname, '..', 'renderer', 'renderer.js'), 'utf8');
  assert.match(renderer, /e\.key === 'Enter'/, 'the composer must act on Enter');
  assert.match(renderer, /onkeydown:/, 'the composer needs a key handler at all');
});

// An oversized frame and an unroutable peer are both "send error", and the
// advice is opposite. Blaming the route for the first is what the live run
// found: the counter said "message too long" and the prose said to go and
// check an address that was perfectly fine.
test('an oversized frame is not blamed on the route', () => {
  const d = baseDiagnostics();
  d.metrics.sendErrors = 7;
  d.metrics.lastSendError = {
    addr: '10.0.0.4:3137', cause: 'PacketTooLargeError', oversize: true,
    message: 'packet of 200000 bytes exceeds the 65507-byte datagram payload',
  };
  const detail = find(healthChecks(d, Date.now()), 'send error').detail;
  assert.ok(!/no longer routes/.test(detail), `blamed on the route:\n${detail}`);
  for (const want of ['too large for one datagram', 'stream', 'divergent']) {
    assert.match(detail, new RegExp(want), `detail should mention ${want}`);
  }
});

test('an oversized frame with no stream listener names the listener', () => {
  const d = baseDiagnostics({ streamListener: false });
  d.metrics.sendErrors = 1;
  d.metrics.lastSendError = {
    addr: '10.0.0.4:3137', cause: 'PacketTooLargeError', oversize: true, message: 'too long',
  };
  const checks = healthChecks(d, Date.now());
  assert.match(find(checks, 'send error').detail, /no stream listener/);
  assert.match(find(checks, 'No stream listener').detail, /oversized frame/);
});

// An ordinary failure keeps the ordinary explanation: the network really is
// the usual cause, and the oversize wording would be a wrong guess.
test('an ordinary send error still blames the network', () => {
  const d = baseDiagnostics();
  d.metrics.sendErrors = 1;
  d.metrics.lastSendError = { addr: '10.0.0.4:3137', cause: 'EHOSTUNREACH', message: 'no route to host' };
  const detail = find(healthChecks(d, Date.now()), 'send error').detail;
  assert.match(detail, /no longer routes/);
  assert.ok(!/too large/.test(detail), `an ordinary failure was called oversized:\n${detail}`);
});

// The send-error counter only climbs, so one blip — an interface bouncing, a
// peer that went away before its eviction — used to hold a node at "warning"
// for the rest of its life while every packet since had gone out fine. A
// warning that cannot clear is one the reader learns to scroll past.
test('a send error ages out of the warnings', () => {
  const d = baseDiagnostics();
  const now = Date.now();
  d.metrics.sendErrors = 2;
  d.metrics.lastSendError = {
    addr: '10.0.0.4:3137',
    cause: 'ENETUNREACH',
    message: 'network is unreachable',
    at: Math.floor((now - SEND_ERROR_RECENT_WINDOW_MS - 1000) / 1000),
  };

  const checks = healthChecks(d, now);
  const c = find(checks, 'send error');
  assert.equal(c.severity, Severity.INFO, `an old send error is still ${c.severity}`);
  assert.match(c.detail, /history/);
  assert.match(c.title, /2 send error/, 'the count was dropped');
  assert.equal(checks[0].severity, Severity.OK, 'the healthy line is still suppressed');
});

// Still a warning while it is happening: the fix must not silence the case the
// check exists for.
test('a send error happening now is still a warning', () => {
  const d = baseDiagnostics();
  const now = Date.now();
  d.metrics.sendErrors = 1;
  d.metrics.lastSendError = {
    addr: '10.0.0.4:3137',
    cause: 'ENETUNREACH',
    message: 'network is unreachable',
    at: Math.floor((now - 1000) / 1000),
  };
  const checks = healthChecks(d, now);
  assert.equal(find(checks, 'send error').severity, Severity.WARN);
  assert.notEqual(checks[0].severity, Severity.OK, 'a node failing to send right now reads as healthy');
});

// The renderer is sandboxed and cannot require the protocol module, so the
// bucket count is written out twice. Two copies of a constant drift.
test('the renderer and the protocol agree on how many buckets there are', () => {
  const W = require('../src/proto/wire');
  assert.equal(S.FINGERPRINT_BUCKETS, W.FINGERPRINT_BUCKETS);
});

/**
 * A node whose store limits are turning peers away is the one node in a cluster
 * that cannot gossip its way back to agreement, and it used to be the one
 * reporting "Node looks healthy": the refusals were counted as stale
 * rejections, which no rule looks at because staleness is normal.
 */
test('refused remote updates are an error, not a healthy node', () => {
  const d = baseDiagnostics();
  d.metrics.kvRefused = 189;

  const checks = healthChecks(d, Date.now());
  assert.notEqual(checks[0].severity, Severity.OK, 'a node refusing remote updates reported as healthy');

  const found = checks.find((c) => c.title.includes('refused'));
  assert.ok(found, `no check named the refusals: ${JSON.stringify(checks)}`);
  assert.equal(found.severity, Severity.ERROR);
  assert.ok(found.title.includes('189'), found.title);
  // The point of the check is that waiting does not fix it.
  for (const want of ['not stale', 'divergent']) {
    assert.ok(found.detail.includes(want), `detail does not mention ${want}: ${found.detail}`);
  }
});

/**
 * Stale rejections stay unremarkable: they are last-write-wins working, and a
 * rule that fired on them would cry wolf on every healthy cluster.
 */
test('stale rejections alone are not a finding', () => {
  const d = baseDiagnostics();
  d.metrics.kvRejectedStale = 5000;

  const checks = healthChecks(d, Date.now());
  assert.equal(checks[0].severity, Severity.OK, JSON.stringify(checks));
});

/**
 * Both guards refuse the frame, so nothing is broken — but neither has a
 * legitimate cause, and these are exactly the frames that used to raise an
 * author's sequence out of reach and silence them for good.
 */
test('malformed chat repair is reported', () => {
  const d = baseDiagnostics();
  d.metrics.chatUnsolicited = 3;
  d.metrics.chatSelfAttributed = 1;

  const checks = healthChecks(d, Date.now());
  const found = checks.find((c) => c.title.includes('malformed repair'));
  assert.ok(found, `no check named the refused frames: ${JSON.stringify(checks)}`);
  assert.equal(found.severity, Severity.WARN);
  assert.ok(found.title.includes('4'), found.title);
  for (const want of ['nobody asked for', 'claiming to be']) {
    assert.ok(found.detail.includes(want), `detail does not separate the causes: ${found.detail}`);
  }
});

test('a clean cluster reports no malformed repair', () => {
  const checks = healthChecks(baseDiagnostics(), Date.now());
  assert.ok(!checks.some((c) => c.title.includes('malformed repair')));
});

/**
 * Two nodes sharing an identity is the one divergence replication cannot
 * repair, so it has to be said out loud. Measured on a live cluster: the two
 * stayed split on a shared key for as long as they ran, and every tool
 * reported the node healthy.
 */
test('a peer using this node\'s identity is an error that names it', () => {
  const checks = healthChecks(baseDiagnostics({ impostors: ['192.168.18.79:3142'] }), Date.now());
  const check = find(checks, "using this node's identity");
  assert.ok(check, 'a peer announcing this node\'s own id was not reported');
  assert.equal(check.severity, Severity.ERROR);
  assert.match(check.title, /192\.168\.18\.79:3142/);
  assert.match(check.detail, /state file/);
});

test('a single address change is not reported as a shared identity', () => {
  const checks = healthChecks(baseDiagnostics({
    idConflicts: [{ id: 'peer-id', addrs: ['a:1', 'b:1'], flips: 1, at: 0 }],
  }), Date.now());
  assert.ok(!find(checks, 'is claimed by'), 'a node changing address was reported as a conflict');
});

test('one id claimed by two live addresses is an error naming both', () => {
  const checks = healthChecks(baseDiagnostics({
    idConflicts: [{
      id: '00000000-dead-beef', addrs: ['192.168.18.79:3141', '192.168.18.79:3142'], flips: 12, at: 0,
    }],
  }), Date.now());
  const check = find(checks, 'is claimed by');
  assert.ok(check, 'one id claimed by two addresses was not reported');
  assert.equal(check.severity, Severity.ERROR);
  assert.match(check.title, /3141/);
  assert.match(check.title, /3142/);
});

/**
 * Tombstone GC is the one setting that can silently undo a delete: a node away
 * longer than the age comes back still holding the value and pushes it back
 * (DESIGN.md §5.5). Demonstrated on a live pair — the delete was undone on
 * both nodes and the consistency check reported them converged.
 *
 * This port exposes the age as a field in Settings, so recommending it without
 * saying what bounds it is telling a user to lose deletes. The caveat was
 * present in the Go port and missing from both GUI ports.
 */
test('the tombstone advice says what bounds the GC age, not only the benefit', () => {
  const base = baseDiagnostics();
  const checks = healthChecks({ ...base, store: { ...base.store, tombstones: 7 } }, Date.now());
  const check = find(checks, 'tombstone(s) kept forever');
  assert.ok(check, 'the tombstone advice was not produced');
  assert.match(check.detail, /longest/, 'the advice does not say what the age must exceed');
});

// A node that failed to *start* is not a node nobody started, and the
// difference is the whole answer. Autostart runs before the window has loaded,
// so the toast for a failed bind reaches nobody — and a toast is gone in six
// seconds, while "STOPPED" stays on screen for as long as the app is open. The
// app opened on "STOPPED" with no reason anywhere, in the case a user is most
// likely to hit (the port already taken) and least able to guess.
test('a node that could not start says why, where it lasts', () => {
  const stopped = baseDiagnostics({ running: false });

  const nobodyStarted = healthChecks(stopped, Date.now());
  assert.ok(nobodyStarted.some((c) => c.title === 'Node stopped'));

  const failed = healthChecks({ ...stopped, lastError: 'bind EADDRINUSE 0.0.0.0:3137' }, Date.now());
  const check = failed.find((c) => c.title === 'The node could not start');
  assert.ok(check, `no check named the failure: ${JSON.stringify(failed.map((c) => c.title))}`);
  assert.equal(check.severity, Severity.ERROR);
  assert.match(check.detail, /EADDRINUSE/);
  assert.ok(!failed.some((c) => c.title === 'Node stopped'), 'two explanations for one state');
});

// A key is arbitrary UTF-8. CSS collapses a newline and a tab to a single
// space, so two genuinely different keys drew identically — next to a delete
// button. The value on the row had always been escaped; the key was the one
// field that was not.
test('a key is escaped and capped for a one-line row, like its value', () => {
  assert.equal(S.displayKey('line1\nline2\ttabbed'), 'line1 ⏎ line2 tabbed');
  assert.equal(S.displayValue('first\nsecond'), 'first ⏎ second');

  // Two keys that differ only in a control character must not render alike.
  assert.notEqual(S.displayKey('a\nb'), S.displayKey('a b'));

  assert.equal(S.displayKey(''), '(empty key)');
  assert.equal(S.displayValue(''), '(empty)');

  // Nothing bounds a key's length, and a long one pushes the row's controls
  // off screen.
  assert.equal(S.displayKey('x'.repeat(500)).length, 120);
  assert.ok(S.displayKey('x'.repeat(500)).endsWith('...'));
});

// Counted since the beginning and explained by nothing: on a tablet with a
// wrong key, five stream errors against an unreachable seed raised not one
// word. The stream is the only path for an oversized value and for a
// whole-store sync on join.
test('stream errors are explained, not just counted', () => {
  const d = baseDiagnostics({ metrics: { ...baseDiagnostics().metrics, streamErrors: 5 } });
  const check = healthChecks(d, Date.now()).find((c) => c.title.includes('stream (TCP) error'));
  assert.ok(check, 'stream errors raised nothing');
  assert.equal(check.severity, Severity.WARN);
  assert.match(check.detail, /firewall that passes UDP and blocks TCP/);

  assert.ok(!healthChecks(baseDiagnostics(), Date.now()).some((c) => c.title.includes('stream (TCP)')));
});

// The reflexive address, consumed. The rendezvous service is the one component
// that sees where a node's packets actually come from, so when it has said so
// that outranks the loopback heuristic: the heuristic can say an address looks
// wrong and only this can say which address is right.
test('the address the rendezvous observed outranks the loopback guess and names the fix', () => {
  const checks = healthChecks(baseDiagnostics({
    addr: '0.0.0.0:3137',
    advertiseMismatch: '192.168.1.42:48812',
  }), Date.now());
  const check = find(checks, 'does not see this node at');
  assert.equal(check.severity, Severity.ERROR);
  assert.match(check.detail, /192\.168\.1\.42/);
  assert.match(check.detail, /192\.168\.1\.42:3137/, 'the port the node binds, not the ephemeral one observed');
  assert.ok(!find(checks, 'loopback'), 'the weaker guess must not sit beside the answer');
});

test('an observed address that agrees with the advertised one says nothing', () => {
  // Reported on every roster, so a node that is configured correctly must not
  // see a finding at all — otherwise the check is noise and gets ignored.
  const checks = healthChecks(baseDiagnostics({ advertiseMismatch: '' }), Date.now());
  assert.ok(!find(checks, 'does not see this node at'));
});

test('a wildcard advertise is a mismatch and a matching literal is not', () => {
  // The engine decides this, not the renderer: a node bound to a wildcard is
  // precisely the case the field exists for, and a node whose advertised host
  // is what the service saw is correct however it got there.
  assert.equal(sameHost('0.0.0.0', '192.168.1.42'), false);
  assert.equal(sameHost('', '192.168.1.42'), false);
  assert.equal(sameHost('192.168.1.42', '192.168.1.42'), true);
  assert.equal(sameHost('192.168.1.9', '192.168.1.42'), false);
  assert.equal(sameHost('gateway.local', '192.168.1.42'), true, 'a name peers can resolve is not a mistake');
});

// GC off warns that a node away too long will undo a delete. GC on is that same
// hazard, configured — and said nothing at all, which is the asymmetry: the
// advice appeared only while it was not being followed.
//
// It cannot be reported after the fact. A node that reclaimed a tombstone and
// then accepted a set for that key cannot tell a resurrection from someone
// writing it again, so the only honest moment is while the age is configured.
test('tombstone GC names the exposure it creates', () => {
  const checks = healthChecks(baseDiagnostics({ tombstoneTtlSec: 30 }), Date.now());
  const check = find(checks, 'Tombstones are reclaimed');
  assert.equal(check.severity, Severity.INFO);
  assert.match(check.title, /30s/);
  assert.match(check.detail, /away for longer/);
  assert.match(check.detail, /undone/);
});

test('GC off says the opposite and not both', () => {
  const d = baseDiagnostics({ tombstoneTtlSec: 0 });
  d.store = { ...d.store, tombstones: 3 };
  const checks = healthChecks(d, Date.now());
  assert.ok(find(checks, 'kept forever'));
  assert.ok(!find(checks, 'Tombstones are reclaimed'));
});

// The exposure is the setting, not the tombstones it happens to hold.
test('the GC exposure is reported before anything is deleted', () => {
  const d = baseDiagnostics({ tombstoneTtlSec: 3600 });
  d.store = { ...d.store, tombstones: 0 };
  assert.ok(find(healthChecks(d, Date.now()), 'Tombstones are reclaimed'));
});
