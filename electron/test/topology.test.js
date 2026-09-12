'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const {
  buildTopology, placeGraph, components, unconfirmed, nodeOf, neighboursOf, NodeRole, LinkKind,
} = require('../src/core/topology');

/**
 * The graph is derived, not received: every claim it makes about the cluster
 * comes from peer lists gossip happened to carry. These pin what that derivation
 * is allowed to conclude — the same cases the Android app is held to.
 */

const SELF = '10.0.0.1:3137';
const peer = (addr, nick = '', lastSeenMs = 1000) => ({ addr, nick, lastSeenMs, firstSeenMs: 500 });
const build = (peers, views) => buildTopology({
  selfAddr: SELF, selfNick: 'desktop', peers, views, nowMs: 1000,
});

test('a lone node is its own graph', () => {
  const t = build([], {});
  assert.equal(t.nodes.length, 1);
  assert.equal(t.nodes[0].role, NodeRole.SELF);
  assert.equal(t.nodes[0].label, 'desktop');
  assert.equal(t.links.length, 0);
  assert.equal(components(t).length, 1);
});

test('peers are direct and linked to us', () => {
  const t = build([peer('10.0.0.2:3137', 'bob')], {});
  assert.equal(t.nodes.length, 2);
  const bob = nodeOf(t, '10.0.0.2:3137');
  assert.equal(bob.role, NodeRole.DIRECT);
  assert.equal(bob.label, 'bob');
  assert.equal(t.links.length, 1);
  assert.equal(t.links[0].kind, LinkKind.DIRECT);
});

test('an address we only heard gossiped is indirect, and its link unconfirmed', () => {
  // The case that makes the graph worth drawing: bob still lists carol, but this
  // node has evicted her, so she is known without being reachable.
  const t = build(
    [peer('10.0.0.2:3137', 'bob')],
    { '10.0.0.2:3137': { peers: ['10.0.0.3:3137'], atMs: 900 } },
  );
  const carol = nodeOf(t, '10.0.0.3:3137');
  assert.equal(carol.role, NodeRole.INDIRECT);
  assert.equal(carol.label, '10.0.0.3:3137');
  assert.equal(carol.lastSeenMs, 900, 'the only freshness for her is when she was last mentioned');
  const link = t.links.find((l) => l.a.includes('10.0.0.3') || l.b.includes('10.0.0.3'));
  assert.equal(link.kind, LinkKind.OBSERVED);
  assert.equal(unconfirmed(t).length, 1);
});

test('a link both ends claim is mutual, and our own links outrank a gossiped claim', () => {
  const t = build(
    [peer('10.0.0.2:3137'), peer('10.0.0.3:3137')],
    {
      '10.0.0.2:3137': { peers: [SELF, '10.0.0.3:3137'], atMs: 900 },
      '10.0.0.3:3137': { peers: [SELF, '10.0.0.2:3137'], atMs: 950 },
    },
  );
  const between = t.links.find((l) => [l.a, l.b].sort().join() === ['10.0.0.2:3137', '10.0.0.3:3137'].join());
  assert.equal(between.kind, LinkKind.MUTUAL);
  assert.equal(unconfirmed(t).length, 0);
  assert.equal(t.links.filter((l) => l.kind === LinkKind.DIRECT).length, 2);
});

test('degrees and neighbours count every known link', () => {
  const t = build(
    [peer('10.0.0.2:3137'), peer('10.0.0.3:3137')],
    { '10.0.0.2:3137': { peers: ['10.0.0.3:3137'], atMs: 900 } },
  );
  assert.deepEqual(neighboursOf(t, SELF), ['10.0.0.2:3137', '10.0.0.3:3137']);
  assert.equal(nodeOf(t, '10.0.0.2:3137').degree, 2);
});

test('a split cluster shows as two components', () => {
  // We know of a pair that talks to each other and to nobody we can reach: the
  // failure a KV store cannot report about itself.
  const t = build(
    [peer('10.0.0.2:3137')],
    {
      '10.0.0.9:3137': { peers: ['10.0.0.8:3137'], atMs: 900 },
      '10.0.0.8:3137': { peers: ['10.0.0.9:3137'], atMs: 900 },
    },
  );
  const groups = components(t);
  assert.equal(groups.length, 2);
  assert.ok(groups.some((g) => g.includes('10.0.0.8:3137') && g.includes('10.0.0.9:3137')));
  assert.ok(groups.some((g) => g.includes(SELF) && g.includes('10.0.0.2:3137')));
});

test('address aliases collapse into one node', () => {
  // A peer advertising the wildcard address is the same node as one on loopback;
  // two circles for one node would be a lie about the size of the cluster.
  const t = buildTopology({
    selfAddr: '0.0.0.0:3137',
    selfNick: 'desktop',
    peers: [peer('localhost:4000')],
    views: { '127.0.0.1:4000': { peers: ['0.0.0.0:3137'], atMs: 900 } },
    nowMs: 1000,
  });
  assert.equal(t.nodes.length, 2);
  assert.equal(t.links.length, 1);
  assert.ok(nodeOf(t, '127.0.0.1:3137'));
  assert.equal(nodeOf(t, '0.0.0.0:3137'), null);
});

test('our own address never becomes a peer of ours', () => {
  const t = build([], { '10.0.0.2:3137': { peers: [SELF], atMs: 900 } });
  assert.equal(nodeOf(t, SELF).role, NodeRole.SELF);
  assert.equal(t.nodes.filter((n) => n.role === NodeRole.SELF).length, 1);
});

test('the layout puts us in the middle and peers on a ring', () => {
  const peers = [2, 3, 4, 5, 6].map((i) => peer(`10.0.0.${i}:3137`));
  const t = build(peers, {});
  const places = placeGraph(t);
  assert.equal(places.size, t.nodes.length);
  const centre = places.get(SELF);
  assert.equal(centre.x, 0);
  assert.equal(centre.y, 0);
  const radii = peers.map((p) => Math.hypot(places.get(p.addr).x, places.get(p.addr).y));
  for (const r of radii) assert.ok(Math.abs(r - radii[0]) < 0.001, 'one shell, one radius');
  assert.ok(radii[0] > 0.4 && radii[0] <= 1, `peers sit off centre and inside the canvas: ${radii[0]}`);
});

test('the layout separates direct peers from gossiped ones', () => {
  const t = build(
    [peer('10.0.0.2:3137')],
    { '10.0.0.2:3137': { peers: ['10.0.0.3:3137'], atMs: 900 } },
  );
  const places = placeGraph(t);
  const direct = Math.hypot(places.get('10.0.0.2:3137').x, places.get('10.0.0.2:3137').y);
  const indirect = Math.hypot(places.get('10.0.0.3:3137').x, places.get('10.0.0.3:3137').y);
  assert.ok(indirect > direct, 'distance from the middle means "how far from us"');
});

test('the layout is stable between frames and never stacks two nodes', () => {
  const peers = [2, 3, 4, 5, 6, 7, 8, 9].map((i) => peer(`10.0.0.${i}:3137`));
  const t = build(peers, {});
  const first = placeGraph(t);
  const second = placeGraph(t);
  assert.deepEqual([...first.entries()], [...second.entries()], 'a node must not jump between frames');
  const points = peers.map((p) => first.get(p.addr));
  for (let i = 0; i < points.length; i++) {
    for (let j = i + 1; j < points.length; j++) {
      const gap = Math.hypot(points[i].x - points[j].x, points[i].y - points[j].y);
      assert.ok(gap > 0.05, `nodes must not land on each other: ${gap}`);
    }
  }
});

test('more nodes than one shell holds spill outward and stay on the canvas', () => {
  const peers = Array.from({ length: 24 }, (_, i) => peer(`10.1.0.${i + 1}:3137`));
  const t = build(peers, {});
  const places = placeGraph(t);
  assert.equal(places.size, 25);
  for (const p of places.values()) {
    assert.ok(Math.hypot(p.x, p.y) <= 1.0001, `point off canvas: ${JSON.stringify(p)}`);
    assert.ok(Number.isFinite(p.x) && Number.isFinite(p.y));
  }
  const outermost = Math.max(...[...places.values()].map((p) => Math.hypot(p.x, p.y)));
  assert.ok(Math.abs(outermost - 1) < 0.001, 'the outer shell should use the canvas');
});

test('a node that has never gossiped is marked as such rather than as advertising nothing', () => {
  const t = build([peer('10.0.0.2:3137')], {});
  assert.equal(nodeOf(t, '10.0.0.2:3137').advertised, -1);
  const gossiped = build([peer('10.0.0.2:3137')], { '10.0.0.2:3137': { peers: [], atMs: 900 } });
  assert.equal(nodeOf(gossiped, '10.0.0.2:3137').advertised, 0);
});
