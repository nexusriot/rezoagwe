'use strict';

const { components, unconfirmed } = require('./topology');

/** How much attention a finding deserves. */
const Severity = Object.freeze({ OK: 'ok', INFO: 'info', WARN: 'warn', ERROR: 'error' });

/**
 * Turns a diagnostics snapshot into the short list of things actually wrong.
 *
 * Pure on purpose: every rule here is a claim about the protocol ("auth failures
 * mean a key mismatch"), and a claim like that is worth a test.
 */
function healthChecks(d, nowMs) {
  const checks = [];
  const m = d.metrics;
  const add = (severity, title, detail) => checks.push({ severity, title, detail });

  if (!d.running) {
    add(Severity.INFO, 'Node stopped',
      'Nothing is gossiping. Start the node from the Peers tab or the header.');
  }

  if (d.loadError) {
    add(Severity.ERROR, 'State file could not be read',
      `${d.dataFile} did not parse (${d.loadError}), so this node started with an empty store and a new `
      + 'identity. Its earlier writes will be outranked until anti-entropy refills it from a peer.');
  }

  if (m.authFailures > 0) {
    add(Severity.ERROR, `${m.authFailures} packet(s) failed authentication`,
      'Something on this network frames packets with a different key: check that every node shares the '
      + `pre-shared key and the cluster name "${d.cluster}" (key fingerprint ${d.keyFingerprint}).`);
  }

  if (m.skewDrops > 0) {
    add(Severity.ERROR, `${m.skewDrops} packet(s) dropped for clock skew`,
      "A peer's clock differs from this machine's by more than 30s. Packets outside that window are "
      + 'refused as replays, so the two cannot talk until a clock is corrected.');
  }

  if (m.malformedDrops > 0) {
    add(Severity.WARN, `${m.malformedDrops} malformed packet(s)`,
      'Authenticated frames whose body did not parse. Usually a peer running an older or newer protocol '
      + 'version.');
  }

  if (m.replayDrops > 0) {
    add(Severity.INFO, `${m.replayDrops} replayed packet(s) ignored`,
      'The same nonce arrived twice. Duplicated datagrams are normal on a lossy network; a steadily '
      + 'climbing count is not.');
  }

  if (m.sendErrors > 0) {
    const last = m.lastSendError;
    add(Severity.WARN, `${m.sendErrors} send error(s)`,
      'Datagrams could not leave this machine — usually a peer address that no longer routes, or a '
      + 'network interface dropping while the node stays up.'
      + (last ? ` Last: ${last.cause} to ${last.addr}${last.message ? ` (${last.message})` : ''}.` : ''));
  }

  if (d.running && !d.streamListener) {
    add(Severity.WARN, `No stream listener on port ${d.configuredPort}`,
      'Another process holds the TCP port, so large state syncs fall back to datagrams and a big keyspace '
      + 'may take several gossip rounds to converge.');
  }

  // A node seconds old has not failed to do anything yet: a peer answers on its
  // own heartbeat, and a seed replies when it feels like it. Flagging either
  // before then turns the first moments of every start into a red screen.
  const settled = d.uptimeSec >= Math.max(6, (d.heartbeatIntervalMs / 1000) * 2);

  if (d.running && settled && d.peers.length === 0) {
    if (d.seeds.length === 0) {
      add(Severity.WARN, 'No peers and no seeds',
        'A node with no bootstrap seed only ever learns peers that contact it first. Add a seed in '
        + 'Settings, or run the Bootstrap role here and point the others at this machine.');
    } else {
      add(Severity.WARN, `No peers learned from ${d.seeds.length} seed(s)`,
        `The seeds ${d.seeds.join(', ')} answered nothing. Check the bootstrap is running, that this `
        + 'machine is on the same network, and that the cluster name and key match.');
    }
  }

  const loopback = d.addr.startsWith('127.') || d.addr.startsWith('localhost:');
  if (d.running && loopback) {
    add(Severity.WARN, 'Advertising a loopback address',
      `Peers are told to reach this node at ${d.addr}, which only resolves to this machine. Connect to a `
      + 'network, or set an advertise host in Settings.');
  }

  if (d.advertiseHost && d.localIpv4 !== d.advertiseHost && !loopback) {
    add(Severity.INFO, 'Advertise host is overridden',
      `Peers are told ${d.advertiseHost}, while this machine's own address is ${d.localIpv4}. That is `
      + 'right behind a port forward and wrong on a plain LAN.');
  }

  if (d.strangers.length > 0) {
    add(Severity.INFO, `${d.strangers.length} address(es) sent packets without being a peer`,
      `Traffic from ${d.strangers.slice(0, 3).map((s) => s.addr).join(', ')} — a node whose advertised `
      + 'address differs from the one its packets come from, which is what a NAT or a wrong advertise '
      + 'host looks like.');
  }

  const stale = d.peers.filter(
    (p) => d.evictThresholdMs > 0 && p.lastSeenMs > 0 && nowMs - p.lastSeenMs > d.evictThresholdMs / 2,
  );
  if (stale.length > 0) {
    add(Severity.WARN, `${stale.length} peer(s) going stale`,
      `No packet from ${stale.slice(0, 3).map((p) => p.nick || p.addr).join(', ')} for over half the `
      + 'eviction window. They will be dropped from the cluster shortly.');
  }

  const silent = settled ? d.peers.filter((p) => p.packetsIn === 0 && p.packetsOut > 0) : [];
  if (silent.length > 0) {
    add(Severity.ERROR, `${silent.length} peer(s) never answered`,
      `This node sends to ${silent.slice(0, 3).map((p) => p.addr).join(', ')} and receives nothing back. `
      + 'One-way UDP like this is a firewall or a wrong advertised port, not packet loss.');
  }

  const groups = components(d.topology);
  if (groups.length > 1) {
    add(Severity.ERROR, `Cluster looks partitioned into ${groups.length} groups`,
      `Known nodes split as ${groups.map((g) => g.length).join(' | ')}. Each group converges on its own `
      + 'and diverges from the others until a link is restored.');
  }

  const oneSided = unconfirmed(d.topology);
  if (oneSided.length > 0) {
    add(Severity.INFO, `${oneSided.length} link(s) claimed by one end only`,
      "Gossip carries each node's own peer list, so a link either end has not yet reported stays "
      + 'unconfirmed. Normal shortly after a node joins.');
  }

  if (d.tombstoneTtlSec === 0 && d.store.tombstones > 0) {
    add(Severity.INFO, `${d.store.tombstones} tombstone(s) kept forever`,
      'Deletes replicate as tombstones and are never reclaimed while the GC age is 0. Set one in '
      + 'Settings if deletes are frequent.');
  }

  if (!d.pskSet) {
    add(Severity.INFO, 'No pre-shared key',
      'The framing key comes from the cluster name alone. That keeps two clusters on one network apart '
      + 'but provides no secrecy: anyone on the network can read and write.');
  }

  // A stopped node already says so; a second line claiming health would only
  // dilute it.
  if (d.running && !checks.some((c) => c.severity === Severity.WARN || c.severity === Severity.ERROR)) {
    checks.unshift({
      severity: Severity.OK,
      title: 'Node looks healthy',
      detail: `${d.peers.length} peer(s), ${d.store.keys} key(s), no dropped packets.`,
    });
  }
  return checks;
}

/** The diagnostics snapshot as text, for pasting into a bug report. */
function asReport(d, nowMs) {
  const lines = [];
  const age = (ms) => (ms === 0 ? 'never' : `${Math.floor((nowMs - ms) / 1000)}s ago`);
  lines.push('rezoagwe node diagnostics');
  lines.push(`addr            ${d.addr}`);
  lines.push(`nick            ${d.nick}`);
  lines.push(`node id         ${d.nodeId}`);
  lines.push(`cluster         ${d.cluster} (key ${d.keyFingerprint}${d.pskSet ? ', psk set' : ', no psk'})`);
  lines.push(`running         ${d.running}${d.running ? `, up ${d.uptimeSec}s` : ''}`);
  lines.push(`port            configured ${d.configuredPort}, bound ${d.boundPort}, streams ${d.streamListener}`);
  lines.push(`addresses       local ${d.localIpv4}, advertising ${d.advertiseHost || '(auto)'}`);
  lines.push(`seeds           ${d.seeds.join(', ') || '(none)'}`);
  lines.push(`intervals       gossip ${d.gossipIntervalMs}ms, heartbeat ${d.heartbeatIntervalMs}ms, `
    + `evict ${d.evictThresholdMs}ms`);
  lines.push(`store           ${d.store.keys} keys, ${d.store.tombstones} tombstones, `
    + `${d.store.valueBytes} value bytes, clock ${d.store.clock}`);
  lines.push(`traffic         ${d.metrics.packetsSent} sent / ${d.metrics.packetsReceived} received, `
    + `${d.metrics.bytesSent}B / ${d.metrics.bytesReceived}B`);
  lines.push(`drops           auth ${d.metrics.authFailures}, replay ${d.metrics.replayDrops}, `
    + `skew ${d.metrics.skewDrops}, malformed ${d.metrics.malformedDrops}`);
  lines.push(`anti-entropy    ${d.metrics.aeRounds} rounds, ${d.metrics.aePushed} pushed, `
    + `${d.metrics.aePulled} pulled`);
  lines.push('');
  lines.push(`peers (${d.peers.length})`);
  for (const p of d.peers) {
    lines.push(`  ${p.addr}  ${p.nick || '-'}  in ${p.packetsIn}/${p.bytesIn}B  `
      + `out ${p.packetsOut}/${p.bytesOut}B  seen ${age(p.lastSeenMs)}  advertises ${p.advertisedPeers}`);
  }
  if (d.strangers.length) {
    lines.push('');
    lines.push(`unknown sources (${d.strangers.length})`);
    for (const p of d.strangers) {
      lines.push(`  ${p.addr}  in ${p.packetsIn}/${p.bytesIn}B  rejected ${p.rejected}`);
    }
  }
  lines.push('');
  lines.push(`topology: ${d.topology.nodes.length} nodes, ${d.topology.links.length} links, `
    + `${components(d.topology).length} component(s)`);
  for (const link of d.topology.links) lines.push(`  ${link.a} -- ${link.b} (${link.kind})`);
  lines.push('');
  lines.push('checks');
  for (const c of healthChecks(d, nowMs)) lines.push(`  [${c.severity.toUpperCase()}] ${c.title}: ${c.detail}`);
  return lines.join('\n');
}

module.exports = { Severity, healthChecks, asReport };
