'use strict';

const { normalizeAddr } = require('../net/addr');

/** What a node is to us: ourselves, a peer we talk to, or one we only heard about. */
const NodeRole = Object.freeze({ SELF: 'self', DIRECT: 'direct', INDIRECT: 'indirect' });

/**
 * How much we know about a link.
 *
 * A gossip packet carries the sender's own peer list, so a link can be asserted
 * by one end only. Telling that apart from a link both ends agree on is what
 * makes a half-open cluster visible instead of looking healthy.
 */
const LinkKind = Object.freeze({ DIRECT: 'direct', MUTUAL: 'mutual', OBSERVED: 'observed' });

/** Nodes per shell before a further one is started, so labels stay readable. */
const SHELL_CAPACITY = 10;
/** Where the innermost shell sits, leaving the middle to this node. */
const INNER_RADIUS = 0.45;

/**
 * Assembles the cluster graph from what the engine already holds: our own peer
 * table and the peer lists our peers have gossiped.
 *
 * Everything here is derived from packets already exchanged — no extra protocol
 * — so the picture is exactly as complete as gossip has made it, which is itself
 * the diagnostic.
 */
function buildTopology({ selfAddr, selfNick, peers, views, nowMs }) {
  const self = normalizeAddr(selfAddr);
  const direct = new Map();
  for (const p of peers || []) direct.set(normalizeAddr(p.addr), p);

  const claims = new Map();
  const claim = (from, to) => {
    if (!from || !to || from === to) return;
    if (!claims.has(from)) claims.set(from, new Set());
    claims.get(from).add(to);
  };
  for (const peer of direct.keys()) claim(self, peer);
  for (const [owner, view] of Object.entries(views || {})) {
    const from = normalizeAddr(owner);
    for (const target of view.peers || []) claim(from, normalizeAddr(target));
  }

  const addrs = new Set([self, ...direct.keys()]);
  for (const [from, targets] of claims) {
    addrs.add(from);
    for (const t of targets) addrs.add(t);
  }

  const links = [];
  const seenPairs = new Set();
  for (const [from, targets] of claims) {
    for (const to of targets) {
      const pair = from < to ? `${from}|${to}` : `${to}|${from}`;
      if (seenPairs.has(pair)) continue;
      seenPairs.add(pair);
      const bothWays = claims.has(to) && claims.get(to).has(from);
      const touchesSelf = from === self || to === self;
      links.push({
        a: from,
        b: to,
        // Our own links are observed first-hand, so they outrank a gossiped claim.
        kind: touchesSelf ? LinkKind.DIRECT : bothWays ? LinkKind.MUTUAL : LinkKind.OBSERVED,
      });
    }
  }

  const degrees = new Map();
  for (const link of links) {
    degrees.set(link.a, (degrees.get(link.a) || 0) + 1);
    degrees.set(link.b, (degrees.get(link.b) || 0) + 1);
  }

  const viewsByAddr = new Map();
  for (const [owner, view] of Object.entries(views || {})) viewsByAddr.set(normalizeAddr(owner), view);

  // For a node we have never exchanged packets with, "last seen" is the last
  // time anyone mentioned it — the only freshness we have for it.
  const mentionedAt = new Map();
  for (const [owner, view] of viewsByAddr) {
    for (const target of view.peers || []) {
      const key = normalizeAddr(target);
      mentionedAt.set(key, Math.max(mentionedAt.get(key) || 0, view.atMs || 0));
    }
    mentionedAt.set(owner, Math.max(mentionedAt.get(owner) || 0, view.atMs || 0));
  }

  const roleOrder = { [NodeRole.SELF]: 0, [NodeRole.DIRECT]: 1, [NodeRole.INDIRECT]: 2 };
  const nodes = [...addrs].map((addr) => {
    const peer = direct.get(addr);
    const view = viewsByAddr.get(addr);
    const role = addr === self ? NodeRole.SELF : peer ? NodeRole.DIRECT : NodeRole.INDIRECT;
    return {
      addr,
      label: addr === self ? selfNick || 'this node' : peer && peer.nick ? peer.nick : addr,
      role,
      lastSeenMs: (peer && peer.lastSeenMs) || (view && view.atMs) || mentionedAt.get(addr) || 0,
      degree: degrees.get(addr) || 0,
      // Peers this node advertised the last time it gossiped, -1 when it never has.
      advertised: view && view.peers ? view.peers.length : -1,
    };
  }).sort((a, b) => roleOrder[a.role] - roleOrder[b.role] || (a.addr < b.addr ? -1 : a.addr > b.addr ? 1 : 0));

  return { nodes, links, generatedAtMs: nowMs || Date.now() };
}

function nodeOf(topology, addr) {
  return topology.nodes.find((n) => n.addr === addr) || null;
}

function neighboursOf(topology, addr) {
  const out = new Set();
  for (const l of topology.links) {
    if (l.a === addr) out.add(l.b);
    else if (l.b === addr) out.add(l.a);
  }
  return [...out].sort();
}

/**
 * Groups of nodes that can reach each other through known links. More than one
 * group means the cluster is split: each half converges internally and silently
 * diverges from the other, which is the failure a KV store cannot report on its
 * own.
 */
function components(topology) {
  const remaining = new Set(topology.nodes.map((n) => n.addr));
  const adjacency = new Map();
  for (const link of topology.links) {
    if (link.a === link.b) continue;
    if (!adjacency.has(link.a)) adjacency.set(link.a, new Set());
    if (!adjacency.has(link.b)) adjacency.set(link.b, new Set());
    adjacency.get(link.a).add(link.b);
    adjacency.get(link.b).add(link.a);
  }
  const groups = [];
  while (remaining.size > 0) {
    const seed = remaining.values().next().value;
    remaining.delete(seed);
    const group = [];
    const queue = [seed];
    while (queue.length) {
      const current = queue.shift();
      group.push(current);
      for (const next of adjacency.get(current) || []) {
        if (remaining.delete(next)) queue.push(next);
      }
    }
    groups.push(group.sort());
  }
  return groups.sort((a, b) => b.length - a.length || (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
}

/** Links only one end has told us about; each is a peer relationship we cannot confirm. */
function unconfirmed(topology) {
  return topology.links.filter((l) => l.kind === LinkKind.OBSERVED);
}

/**
 * Places nodes for drawing: this node in the middle, peers on the first ring,
 * nodes we only heard about further out.
 *
 * Coordinates are normalised to [-1, 1] so the layout is independent of the
 * canvas, and the order is derived from the sorted address so a node does not
 * jump around between frames.
 */
function placeGraph(topology) {
  const result = new Map();
  const byRole = (role) => topology.nodes.filter((n) => n.role === role).map((n) => n.addr).sort();
  for (const addr of topology.nodes.filter((n) => n.role === NodeRole.SELF).map((n) => n.addr)) {
    result.set(addr, { x: 0, y: 0 });
  }

  const chunk = (addrs) => {
    if (!addrs.length) return [];
    const out = [];
    for (let i = 0; i < addrs.length; i += SHELL_CAPACITY) out.push(addrs.slice(i, i + SHELL_CAPACITY));
    return out;
  };

  // Peers first, then the nodes we only heard about: shells never mix the two,
  // so distance from the middle means "how far from us", not "how many fitted".
  const shells = [...chunk(byRole(NodeRole.DIRECT)), ...chunk(byRole(NodeRole.INDIRECT))];
  shells.forEach((shell, index) => {
    const radius = shells.length <= 1
      ? (INNER_RADIUS + 1) / 2
      : INNER_RADIUS + (1 - INNER_RADIUS) * index / (shells.length - 1);
    // Every other shell is rotated half a slot so a node never sits exactly
    // behind the one on the shell inside it.
    const offset = index % 2 === 0 ? 0 : Math.PI / shell.length;
    shell.forEach((addr, slot) => {
      const angle = -Math.PI / 2 + offset + (2 * Math.PI * slot) / shell.length;
      result.set(addr, { x: radius * Math.cos(angle), y: radius * Math.sin(angle) });
    });
  });
  return result;
}

module.exports = {
  NodeRole,
  LinkKind,
  buildTopology,
  nodeOf,
  neighboursOf,
  components,
  unconfirmed,
  placeGraph,
  SHELL_CAPACITY,
  INNER_RADIUS,
};
