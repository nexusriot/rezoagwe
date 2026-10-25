'use strict';

const { components, unconfirmed } = require('./topology');
const { splitHostPort } = require('../net/addr');

/** How much attention a finding deserves. */
const Severity = Object.freeze({ OK: 'ok', INFO: 'info', WARN: 'warn', ERROR: 'error' });

/**
 * How often one node id must change hands before it is reported. A node that
 * genuinely moved address rebinds once and stays; two nodes sharing an
 * identity rebind on every heartbeat either one sends.
 */
const ID_CONFLICT_FLIPS = 3;

/**
 * How long a send failure stays a warning.
 *
 * Gossip runs every few seconds, so a machine that has had nothing fail to
 * send in this long is one whose network came back. Past that the count is
 * history: it stays on the screen, as information, instead of holding a node
 * that works at "warning" forever.
 */
const SEND_ERROR_RECENT_WINDOW_MS = 120_000;

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

  for(const warning of d.replicationWarnings||[])add(Severity.WARN,'Replication limits differ',warning);
  if (d.persistence && d.persistence.enabled) {
    const p = d.persistence;
    const detail = `Mode: ${p.mode || "snapshot"}; durable: ${!!p.durable}; Last save: ${p.lastSuccessMs ? new Date(p.lastSuccessMs).toISOString() : 'never'}; pending: ${p.pending}; recovery: ${p.recoveryFile || 'none'}`;
    add(p.error ? Severity.ERROR : p.pending ? Severity.INFO : Severity.OK,
      p.error ? 'State is not safely saved' : p.pending ? 'State save pending' : 'State saved', `${p.error || ''} ${detail}`);
  }
  if (!d.running) {
    // A node that failed to *start* is not the same as one nobody started, and
    // the difference is the whole answer. Autostart runs before the window has
    // loaded, so the toast for a failed bind reaches nobody, and a toast is
    // gone in six seconds anyway — while "STOPPED" stays on screen for as long
    // as the app is open. The reason has to live somewhere that lasts.
    if (d.lastError) {
      add(Severity.ERROR, 'The node could not start',
        `${d.lastError}. A port already in use is the usual cause: another copy of this app, or a `
        + `rezoagwe-discovery on the same machine. Change the node port in Settings, or stop whatever `
        + 'holds it, and start the node again.');
    } else {
      add(Severity.INFO, 'Node stopped',
        'Nothing is gossiping. Start the node from the Peers tab or the header.');
    }
  }

  if (d.loadError) {
    add(Severity.ERROR, 'State file could not be read',
      `${d.dataFile} did not parse (${d.loadError}), so this node started with an empty store and a new `
      + 'identity. The original is retained for recovery, or saves are blocked if retention failed. Its earlier writes are '
      + 'outranked until anti-entropy refills it from a peer, and every chat line it has said before '
      + 'now reads as coming from a different author.');
  }

  if (m.authFailures > 0) {
    add(Severity.ERROR, `${m.authFailures} packet(s) failed authentication`,
      'Something on this network frames packets with a different key: check that every node shares the '
      + `pre-shared key and the cluster name "${d.cluster}" (key fingerprint ${d.keyFingerprint}).`);
  }

  for (const addr of d.impostors || []) {
    add(Severity.ERROR, `${addr} is using this node's identity`,
      `That peer announces node id ${d.nodeId}, which is this node's. A node generates its id `
      + 'once and keeps it, so the two are running from copies of one state file. Every version '
      + 'both write is stamped with the same id, so their writes to a shared key compare equal, '
      + 'neither replica accepts the other, and the values stay different for good. Stop one and '
      + 'delete its state file so it starts with a fresh identity.');
  }
  for (const c of d.idConflicts || []) {
    // One rebinding is what a node changing address looks like.
    if (c.flips < ID_CONFLICT_FLIPS) continue;
    add(Severity.ERROR, `node id ${c.id.slice(0, 8)} is claimed by ${c.addrs[0]} and ${c.addrs[1]}`,
      `The claim has moved between those addresses ${c.flips} times. A node that changed address `
      + 'would rebind once; repeated rebinding means both are live and sharing one identity, so '
      + 'their writes compare equal and any key they both set stays split permanently. Stop one '
      + 'and delete its state file.');
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

  if (m.chatUnsolicited > 0 || m.chatSelfAttributed > 0) {
    // Both are refusals, so nothing is broken here — but neither has a
    // legitimate cause. A backlog only ever answers a request this node made,
    // and no peer ever echoes this node its own lines. Left unchecked these
    // are exactly the frames that used to raise an author's sequence out of
    // reach and silence them here for good.
    add(Severity.WARN,
      `${m.chatUnsolicited + m.chatSelfAttributed} chat frame(s) refused as malformed repair`,
      `${m.chatUnsolicited} backlog line(s) nobody asked for and ${m.chatSelfAttributed} line(s) `
      + 'claiming to be written by this node. Both were dropped, and neither has a legitimate '
      + 'cause: a backlog only answers a request this node made, and no peer echoes a node its '
      + 'own lines. Some peer is running something that does not match the protocol.');
  }

  if ((d.chatGaps || []).length) {
    // Chat is the one payload with no digest behind it, so a line that never
    // arrived leaves nothing in the store to compare. Before the sequence
    // existed this was not a quiet failure, it was an invisible one: the
    // author's log said sent, every receiver simply never had it, and no
    // counter moved at either end.
    const lost = d.chatGaps.reduce((n, g) => n + g.lost, 0);
    const outstanding = d.chatGaps.reduce((n, g) => n + g.outstanding, 0);
    const worstGap = d.chatGaps.find((g) => g.lost > 0);
    const worst = worstGap ? (worstGap.nick || worstGap.addr) : '';
    if (lost > 0) {
      add(Severity.WARN, `${lost} chat line(s) lost`,
        'These will not arrive: the author no longer holds them, or never answered a request '
        + 'for them. Unlike a key, a chat line has no digest to repair it from a third party, '
        + "only its author can re-send it, and only while it is still in that node's ring."
        + (worst ? ` The earliest were from ${worst}.` : ''));
    } else if (outstanding > 0) {
      add(Severity.INFO, `${outstanding} chat line(s) not yet arrived`,
        "A gap in an author's sequence. Most are a reordering that closes on its own; the rest "
        + 'are asked for on the next gossip tick.');
    }
  }

  if (m.kvRefused > 0) {
    // The only condition here that no amount of gossip repairs: a peer holds
    // entries this store has declined, so the two stay divergent until a limit
    // moves. The anti-entropy sweep will offer the same entries every ten
    // seconds for as long as the node runs.
    add(Severity.ERROR, `${m.kvRefused} remote update(s) refused by this node's limits`,
      "This node refused remote updates its store would not hold \u2014 a value over the configured "
      + 'value limit, a key past the key limit, or an entry too large to replicate at all. They are '
      + 'not stale and will not arrive later: the peers that hold them stay divergent from this node '
      + 'until a limit is raised or keys are freed. Verify replicas names the buckets that differ.');
  }

  if (m.replayDrops > 0) {
    add(Severity.INFO, `${m.replayDrops} replayed packet(s) ignored`,
      'The same nonce arrived twice. Duplicated datagrams are normal on a lossy network; a steadily '
      + 'climbing count is not.');
  }

  if (m.sendErrors > 0) {
    const last = m.lastSendError;
    // The last failure decides what this says. An oversized frame and an
    // unroutable peer both arrive here as "send error", and the advice is
    // opposite: one is a payload this node built too big to send, the other
    // is the network. Blaming the route for the first sends the reader to
    // check an address that is perfectly fine.
    let detail = 'Datagrams could not leave this machine — usually a peer address that no longer '
      + 'routes, or a network interface dropping while the node stays up.';
    if (last && last.oversize) {
      detail = 'A frame too large for one datagram could not be sent. Nothing is wrong with the '
        + 'route: an entry this big travels over a stream instead, and that did not work.'
        + (d.streamListener
          ? " The peer's stream port is the thing to look at: a firewall that passes UDP and "
            + 'blocks TCP looks exactly like this.'
          : ' This node has no stream listener, which is why — see the check below.')
        + ' Until it does, that entry cannot reach that peer by any path, and the replicas stay '
        + 'divergent however long anti-entropy runs.';
    }
    if (last) detail += ` Last: ${last.cause} to ${last.addr}${last.message ? ` (${last.message})` : ''}.`;
    // Whether anything is wrong *now* is a separate question from whether
    // anything ever went wrong. The counter only climbs, so one blip — a
    // network interface bouncing, a peer that went away before its eviction —
    // left the node warning for the rest of its life while every packet since
    // had gone out fine. A warning that cannot clear is one the reader learns
    // to scroll past, which costs the real ones their only chance of being read.
    const recent = !last || !last.at || (nowMs - last.at * 1000) <= SEND_ERROR_RECENT_WINDOW_MS;
    if (recent) {
      add(Severity.WARN, `${m.sendErrors} send error(s)`, detail);
    } else {
      add(Severity.INFO, `${m.sendErrors} send error(s), none recent`,
        `Nothing has failed to leave this machine in the last `
        + `${Math.round(SEND_ERROR_RECENT_WINDOW_MS / 60000)}m, so this is history rather than a `
        + `fault to chase. Last: ${last.cause} to ${last.addr}`
        + `${last.message ? ` (${last.message})` : ''}.`);
    }
  }

  // Counted since the beginning and explained by nothing. The stream is the only
  // path for a value too big for a datagram and for a whole-store sync on join,
  // so a node whose streams fail is not slightly degraded: an oversized entry
  // cannot reach that peer by any path.
  if (m.streamErrors > 0) {
    add(Severity.WARN, `${m.streamErrors} stream (TCP) error(s)`,
      'A stream to a peer or a seed failed. Datagrams and streams share a port number but not '
      + 'a protocol, so a firewall that passes UDP and blocks TCP looks exactly like this. Until '
      + 'it works, a joiner falls back to the bounded datagram state sync, and any entry too '
      + 'large for one datagram cannot replicate at all.');
  }

  if (d.running && !d.streamListener) {
    add(Severity.WARN, `No stream listener on port ${d.configuredPort}`,
      'Another process holds the TCP port, so large state syncs fall back to datagrams and a big keyspace '
      + 'may take several gossip rounds to converge.'
      + (m.lastSendError && m.lastSendError.oversize
        ? ' It is also why the oversized frame above had nowhere to go: the stream is the only path '
          + 'for an entry that does not fit a datagram.'
        : ''));
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

  // The rendezvous service is the one component that sees where this node's
  // packets actually come from, so when it has told us, that outranks the
  // heuristics below: they can say "that address looks wrong" and only this can
  // say which address is right.
  if (d.advertiseMismatch) {
    const observed = splitHostPort(d.advertiseMismatch);
    const host = observed ? observed.host : d.advertiseMismatch;
    add(Severity.ERROR, 'Advertising an address the cluster does not see this node at',
      `Peers are told to reach this node at ${d.addr}, and the rendezvous service received its packets `
      + `from ${d.advertiseMismatch}. Every peer resolves what it is told, so they are reaching `
      + `somewhere else — usually their own loopback. Set the advertise host in Settings to ${host}, `
      + `which makes this node ${host}:${d.configuredPort || d.boundPort}.`);
  } else if (d.running && loopback) {
    add(Severity.WARN, 'Advertising a loopback address',
      `Peers are told to reach this node at ${d.addr}, which only resolves to this machine. Connect to a `
      + 'network, or set an advertise host in Settings.');
  }

  if (!d.advertiseMismatch && d.advertiseHost && d.localIpv4 !== d.advertiseHost && !loopback) {
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
      + 'Settings if deletes are frequent — it has to exceed the longest outage you expect a node '
      + 'to come back from. A node that was away longer still holds the value, and pushes it back '
      + 'once the tombstone is gone, silently undoing the delete.');
  }

  // The other half of that advice, which nothing said once it was taken. A
  // reclaimed tombstone is the only record that a key was deleted, so the
  // undoing it warns about leaves no trace at all: the delete is reversed on
  // every replica and the consistency check calls the result converged, because
  // it is. It cannot be detected after the fact — a node cannot tell a
  // resurrection from someone writing the key again — so the exposure is named
  // while it is configured, which is the only honest moment.
  if (d.tombstoneTtlSec > 0) {
    add(Severity.INFO, `Tombstones are reclaimed after ${d.tombstoneTtlSec}s`,
      `A delete is remembered for ${d.tombstoneTtlSec}s and then forgotten. A node that is away `
      + 'for longer comes back still holding the value, and the delete is undone on every replica '
      + 'with nothing to report it — the cluster converges on the resurrected value. Keep the age '
      + 'above the longest outage a node can return from, or set it to 0 and let tombstones '
      + 'accumulate.');
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
  lines.push(`replication     ${d.metrics.kvApplied} applied, ${d.metrics.kvRejectedStale} stale, `
    + `${d.metrics.kvRefused} refused by limits`);
  lines.push(`chat delivery   ${d.metrics.chatGapsFound} gaps, ${d.metrics.chatRecovered} recovered, `
    + `${d.metrics.chatLost} lost, ${d.metrics.chatDuplicates} duplicates`);
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

module.exports = { Severity, healthChecks, asReport, ID_CONFLICT_FLIPS, SEND_ERROR_RECENT_WINDOW_MS };
