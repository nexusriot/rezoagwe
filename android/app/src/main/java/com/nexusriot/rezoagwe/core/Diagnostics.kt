package com.nexusriot.rezoagwe.core

/** How much attention a finding deserves. */
enum class Severity { OK, INFO, WARN, ERROR }

/**
 * One thing worth knowing about this node's health, phrased as what to do about
 * it. A counter on its own says a packet was dropped; a check says which
 * misconfiguration drops packets that way.
 */
data class HealthCheck(val severity: Severity, val title: String, val detail: String)

/**
 * How often one node id must change hands before it is reported. A node that
 * genuinely moved address rebinds once and stays; two nodes sharing an
 * identity rebind on every heartbeat either one sends.
 */
const val ID_CONFLICT_FLIPS = 3L

/** What this node has seen claim one node id. */
data class IdBinding(
    val addr: String,
    val other: String = "",
    val flips: Long = 0,
    val at: Long = 0,
)

/** One node id claimed by two different addresses. */
data class IdConflict(
    val id: String,
    val addrs: List<String>,
    val flips: Long,
    val at: Long,
)

/** Everything known about one peer, or about an address that talks to us without being one. */
data class PeerDiagnostics(
    val addr: String,
    val nick: String = "",
    val known: Boolean = true,
    val firstSeenMs: Long = 0,
    val lastSeenMs: Long = 0,
    val packetsOut: Long = 0,
    val packetsIn: Long = 0,
    val bytesOut: Long = 0,
    val bytesIn: Long = 0,
    val rejected: Long = 0,
    val sendErrors: Long = 0,
    /** Peers this one advertised when it last gossiped, -1 when it never has. */
    val advertisedPeers: Int = -1,
    val lastGossipMs: Long = 0,
)

data class StoreDiagnostics(
    val keys: Int = 0,
    val tombstones: Int = 0,
    val valueBytes: Long = 0,
    val clock: Long = 0,
    val historyKeys: Int = 0,
    val largestKey: String = "",
    val largestValueBytes: Int = 0,
    /** How far the per-peer anti-entropy sweeps have walked, as one readable line. */
    val digestCursor: String = "",
)

/**
 * A full picture of one node at one instant: identity, sockets, configuration,
 * per-peer traffic, store shape and the counters.
 *
 * Gathered in one place so a report can be read — or exported and sent — without
 * walking six screens, and so [healthChecks] can reason over all of it at once.
 */
data class NodeDiagnostics(
    val addr: String = "",
    val nick: String = "",
    val nodeId: String = "",
    val cluster: String = "",
    val keyFingerprint: String = "",
    val pskSet: Boolean = false,
    val configuredPort: Int = 0,
    val boundPort: Int = 0,
    val streamListener: Boolean = false,
    val advertiseHost: String = "",
    val localIpv4: String = "",
    /**
     * The address a rendezvous service saw this node's packets arrive from, when
     * it names a different host from the advertised one. The rendezvous is the
     * only component that can see this, so when it is set it outranks the
     * loopback heuristic below: that can say an address looks wrong, this says
     * which one is right.
     */
    val advertiseMismatch: String = "",
    /** The state file, and why it could not be read. */
    val dataFile: String = "",
    val loadError: String = "",
    val running: Boolean = false,
    val startedAtMs: Long = 0,
    val uptimeSec: Long = 0,
    val gossipIntervalMs: Long = 0,
    val heartbeatIntervalMs: Long = 0,
    val evictThresholdMs: Long = 0,
    val sweepIntervalMs: Long = 0,
    val tombstoneTtlSec: Long = 0,
    val seeds: List<String> = emptyList(),
    val peers: List<PeerDiagnostics> = emptyList(),
    /** Addresses that sent us packets without being peers — a NAT or an advertise-host mismatch. */
    val strangers: List<PeerDiagnostics> = emptyList(),
    /** Each author's chat delivery record. Chat is the one payload with no
     *  digest behind it, so a missing line leaves no trace in the store. */
    val chatGaps: List<ChatGapStat> = emptyList(),
    /** Node ids seen claimed by more than one address. */
    val idConflicts: List<IdConflict> = emptyList(),
    /** Peer addresses seen claiming this node's own id. */
    val impostors: List<String> = emptyList(),
    val store: StoreDiagnostics = StoreDiagnostics(),
    val metrics: MetricsSnapshot = MetricsSnapshot(),
    val topology: Topology = Topology(),
    val chatLines: Int = 0,
    val activityLines: Int = 0,
)

/**
 * Turns a diagnostics snapshot into the short list of things actually wrong.
 *
 * Pure on purpose: every rule here is a claim about the protocol ("auth failures
 * mean a key mismatch"), and a claim like that is worth a test.
 */
fun healthChecks(
    d: NodeDiagnostics,
    nowMs: Long,
    device: DeviceReport? = null,
): List<HealthCheck> {
    val checks = mutableListOf<HealthCheck>()
    val m = d.metrics

    // The phone's own state comes first, because when it is the cause every
    // check below it is a guess. `deviceReport` was collected for exactly this
    // — "the usual causes are not in the protocol at all" — and then read by
    // nothing: with Wi-Fi off the Device card said `network none` while Health
    // blamed "a peer address that no longer routes, or Wi-Fi dropping" and sent
    // the user to check a bootstrap that was fine.
    val offline = device != null && device.transport == "none"
    if (offline) {
        checks += HealthCheck(
            Severity.ERROR,
            "This device has no network",
            "Android reports no active connection, so nothing this node sends can leave the " +
                "device and no peer can reach it. The send errors and missing peers below follow " +
                "from that, not from the cluster's configuration. Reconnect to Wi-Fi.",
        )
    }
    if (device != null && device.powerSaveMode) {
        checks += HealthCheck(
            Severity.WARN,
            "Battery saver is on",
            "Power saving can stretch or freeze the gossip timers while the screen is off, and " +
                "peers evict this node ${d.evictThresholdMs / 1000}s after its last packet.",
        )
    }

    if (!d.running) {
        checks += HealthCheck(
            Severity.INFO,
            "Node stopped",
            "Nothing is gossiping. Start the node on the Peers tab.",
        )
    }

    if (m.authFailures > 0) {
        checks += HealthCheck(
            Severity.ERROR,
            "${m.authFailures} packet(s) failed authentication",
            "Something on this network frames packets with a different key: check that every node " +
                "shares the pre-shared key and the cluster name \"${d.cluster}\" " +
                "(key fingerprint ${d.keyFingerprint}).",
        )
    }

    for (peerAddr in d.impostors) {
        checks += HealthCheck(
            Severity.ERROR,
            "$peerAddr is using this node's identity",
            "That peer announces node id ${d.nodeId}, which is this node's. A node generates its " +
                "id once and keeps it, so the two are running from copies of one state file. Every " +
                "version both write is stamped with the same id, so their writes to a shared key " +
                "compare equal, neither replica accepts the other, and the values stay different " +
                "for good. Stop one and delete its state file so it starts with a fresh identity.",
        )
    }

    for (c in d.idConflicts) {
        // One rebinding is what a node changing address looks like.
        if (c.flips < ID_CONFLICT_FLIPS) continue
        checks += HealthCheck(
            Severity.ERROR,
            "node id ${c.id.take(8)} is claimed by ${c.addrs[0]} and ${c.addrs[1]}",
            "The claim has moved between those addresses ${c.flips} times. A node that changed " +
                "address would rebind once; repeated rebinding means both are live and sharing one " +
                "identity, so their writes compare equal and any key they both set stays split " +
                "permanently. Stop one and delete its state file.",
        )
    }

    if (m.skewDrops > 0) {
        checks += HealthCheck(
            Severity.ERROR,
            "${m.skewDrops} packet(s) dropped for clock skew",
            "A peer's clock differs from this device's by more than 30s. Packets outside that window " +
                "are refused as replays, so the two cannot talk until a clock is corrected.",
        )
    }

    if (m.malformedDrops > 0) {
        checks += HealthCheck(
            Severity.WARN,
            "${m.malformedDrops} malformed packet(s)",
            "Authenticated frames whose body did not parse. Usually a peer running an older or newer " +
                "protocol version.",
        )
    }

    if (m.chatUnsolicited > 0 || m.chatSelfAttributed > 0) {
        // Both are refusals, so nothing is broken here — but neither has a
        // legitimate cause. A backlog only ever answers a request this node
        // made, and no peer ever echoes this node its own lines. Left
        // unchecked these are exactly the frames that used to raise an
        // author's sequence out of reach and silence them here for good.
        checks += HealthCheck(
            Severity.WARN,
            "${m.chatUnsolicited + m.chatSelfAttributed} chat frame(s) refused as malformed repair",
            "${m.chatUnsolicited} backlog line(s) nobody asked for and ${m.chatSelfAttributed} " +
                "line(s) claiming to be written by this node. Both were dropped, and neither has " +
                "a legitimate cause: a backlog only answers a request this node made, and no peer " +
                "echoes a node its own lines. Some peer is running something that does not match " +
                "the protocol.",
        )
    }

    if (d.chatGaps.isNotEmpty()) {
        // Chat is the one payload with no digest behind it, so a line that
        // never arrived leaves nothing in the store to compare. Before the
        // sequence existed this was not a quiet failure, it was an invisible
        // one: the author's log said sent, every receiver simply never had it,
        // and no counter moved at either end.
        val lost = d.chatGaps.sumOf { it.lost }
        val outstanding = d.chatGaps.sumOf { it.outstanding.toLong() }
        val worst = d.chatGaps.firstOrNull { it.lost > 0 }?.let { g -> g.nick.ifEmpty { g.addr } }
        when {
            lost > 0 -> checks += HealthCheck(
                Severity.WARN,
                "$lost chat line(s) lost",
                "These will not arrive: the author no longer holds them, or never answered a " +
                    "request for them. Unlike a key, a chat line has no digest to repair it from " +
                    "a third party, only its author can re-send it, and only while it is still " +
                    "in that node's ring." +
                    (if (worst != null) " The earliest were from $worst." else ""),
            )
            outstanding > 0 -> checks += HealthCheck(
                Severity.INFO,
                "$outstanding chat line(s) not yet arrived",
                "A gap in an author's sequence. Most are a reordering that closes on its own; " +
                    "the rest are asked for on the next gossip tick.",
            )
        }
    }

    if (m.kvRefused > 0) {
        // The only condition here that no amount of gossip repairs: a peer
        // holds entries this store has declined, so the two stay divergent
        // until a limit moves. The anti-entropy sweep will offer the same
        // entries every ten seconds for as long as the node runs.
        checks += HealthCheck(
            Severity.ERROR,
            "${m.kvRefused} remote update(s) refused by this node's limits",
            "This node refused remote updates its store would not hold — a value over the " +
                "configured value limit, a key past the key limit, or an entry too large to " +
                "replicate at all. They are not stale and will not arrive later: the peers that " +
                "hold them stay divergent from this node until a limit is raised or keys are " +
                "freed. Verify replicas names the buckets that differ.",
        )
    }

    if (m.replayDrops > 0) {
        checks += HealthCheck(
            Severity.INFO,
            "${m.replayDrops} replayed packet(s) ignored",
            "The same nonce arrived twice. Duplicated datagrams are normal on a lossy network; a " +
                "steadily climbing count is not.",
        )
    }

    if (m.sendErrors > 0) {
        val last = m.lastSendError
        checks += HealthCheck(
            if (last?.onMainThread == true) Severity.ERROR else Severity.WARN,
            "${m.sendErrors} send error(s)",
            when {
                offline ->
                    "This device has no network, so nothing could leave it. Nothing here points at " +
                        "a peer address or at the cluster's configuration." +
                        (if (last == null) "" else " Last: $last.")
                last == null ->
                    "Datagrams could not leave the device — usually a peer address that no longer " +
                        "routes, or Wi-Fi dropping while the node stays up."
                // Worth its own wording: the packets are not lost on the network, they
                // were never handed to it, and no amount of looking at Wi-Fi will show that.
                last.onMainThread ->
                    "Datagrams were sent from the UI thread, which Android refuses — this is a bug " +
                        "in the app, not the network. The writes survive locally and reach peers " +
                        "only on the next anti-entropy round. Last: $last."
                // Also worth its own wording: an oversized frame and an
                // unroutable peer both arrive here as "send error", and the
                // advice is opposite. Blaming the route for the first sends
                // the reader to check an address that is perfectly fine.
                last.oversize ->
                    "A frame too large for one datagram could not be sent. Nothing is wrong with " +
                        "the route: an entry this big travels over a stream instead, and that did " +
                        "not work. " +
                        (
                            if (d.streamListener) {
                                "The peer's stream port is the thing to look at: a firewall that " +
                                    "passes UDP and blocks TCP looks exactly like this."
                            } else {
                                "This device has no stream listener, which is why — see the check below."
                            }
                            ) +
                        " Until it does, that entry cannot reach that peer by any path, and the " +
                        "replicas stay divergent however long anti-entropy runs. Last: $last."
                else ->
                    "Datagrams could not leave the device — usually a peer address that no longer " +
                        "routes, or Wi-Fi dropping while the node stays up. Last: $last."
            },
        )
    }

    // Counted since the beginning and explained by nothing: on a tablet with a
    // wrong key, five stream errors against an unreachable seed raised not one
    // word. The stream is the only path for a value too big for a datagram and
    // for a whole-store sync on join, so a node whose streams fail is not
    // slightly degraded.
    if (m.streamErrors > 0) {
        checks += HealthCheck(
            Severity.WARN,
            "${m.streamErrors} stream (TCP) error(s)",
            "A stream to a peer or a seed failed. Datagrams and streams share a port number but " +
                "not a protocol, so a firewall that passes UDP and blocks TCP looks exactly like " +
                "this. Until it works, a joiner falls back to the bounded datagram state sync, and " +
                "any entry too large for one datagram cannot replicate at all.",
        )
    }

    if (d.running && !d.streamListener) {
        checks += HealthCheck(
            Severity.WARN,
            "No stream listener on port ${d.configuredPort}",
            "Another app holds the TCP port, so large state syncs fall back to datagrams and a big " +
                "keyspace may take several gossip rounds to converge." +
                if (m.lastSendError?.oversize == true) {
                    " It is also why the oversized frame above had nowhere to go: the stream is the " +
                        "only path for an entry that does not fit a datagram."
                } else {
                    ""
                },
        )
    }

    if (d.running && d.peers.isEmpty()) {
        checks += if (d.seeds.isEmpty()) {
            HealthCheck(
                Severity.WARN,
                "No peers and no seeds",
                "A node with no bootstrap seed only ever learns peers that contact it first. Add a " +
                    "seed in Settings, or run the Bootstrap role here and point the others at this device.",
            )
        } else {
            HealthCheck(
                Severity.WARN,
                "No peers learned from ${d.seeds.size} seed(s)",
                if (offline) {
                    "The seeds ${d.seeds.joinToString(", ")} could not be reached because this device " +
                        "has no network. There is nothing to check about the bootstrap or the key " +
                        "until it is back."
                } else {
                    "The seeds ${d.seeds.joinToString(", ")} answered nothing. Check the bootstrap is " +
                        "running, that this device is on the same network, and that the cluster name " +
                        "and key match."
                },
            )
        }
    }

    // Reported before anything else that could be a *consequence* of it: a node
    // that started empty will also look like one that has fallen behind.
    if (d.loadError.isNotEmpty()) {
        checks += HealthCheck(
            Severity.ERROR,
            "State file could not be read",
            "${d.dataFile} did not parse (${d.loadError}), so this node started with an empty store " +
                "and a new identity, and has already rewritten that file with the new state. Its " +
                "earlier writes are outranked until anti-entropy refills it from a peer, and every " +
                "chat line it has said before now reads as coming from a different author.",
        )
    }

    val loopback = d.addr.startsWith("127.") || d.addr.startsWith("localhost:")

    // The rendezvous service is the one component that sees where this node's
    // packets actually come from, so when it has told us, that outranks the
    // heuristics below: they can say "that address looks wrong" and only this
    // can say which address is right.
    //
    // Except with the radio off. A device with no network advertises loopback
    // because it has nowhere else to advertise, and the address a rendezvous
    // saw before the outage describes where this node *used to be*. Reported
    // then, it tells someone whose Wi-Fi has dropped to hard-code the address
    // they had — advice that is wrong now and still wrong on the next network.
    // The loopback rule below says the useful thing: reconnect.
    if (d.advertiseMismatch.isNotEmpty() && !offline) {
        val suggestion = NodeEngine.hostOf(d.advertiseMismatch)
            .takeIf { it.isNotEmpty() }
            ?.let { "$it:${d.configuredPort}" }
            ?: d.advertiseMismatch
        checks += HealthCheck(
            Severity.ERROR,
            "Advertising an address the cluster does not see this node at",
            "Peers are told to reach this node at ${d.addr}, and the rendezvous service received " +
                "its packets from ${d.advertiseMismatch}. Every peer resolves what it is told, so " +
                "they are reaching somewhere else — usually their own loopback. Set the advertise " +
                "host in Settings to ${NodeEngine.hostOf(d.advertiseMismatch)}, which makes this " +
                "node $suggestion.",
        )
    } else if (d.running && loopback) {
        checks += HealthCheck(
            Severity.WARN,
            "Advertising a loopback address",
            "Peers are told to reach this node at ${d.addr}, which only resolves to this device. " +
                "Connect to a network, or set an advertise host in Settings.",
        )
    }

    if (d.advertiseMismatch.isEmpty() &&
        d.advertiseHost.isNotEmpty() && d.localIpv4 != d.advertiseHost && !loopback
    ) {
        checks += HealthCheck(
            Severity.INFO,
            "Advertise host is overridden",
            "Peers are told ${d.advertiseHost}, while this device's own address is ${d.localIpv4}. " +
                "That is right behind a port forward and wrong on a plain LAN.",
        )
    }

    if (d.strangers.isNotEmpty()) {
        checks += HealthCheck(
            Severity.INFO,
            "${d.strangers.size} address(es) sent packets without being a peer",
            "Traffic from ${d.strangers.take(3).joinToString(", ") { it.addr }} — a node whose advertised " +
                "address differs from the one its packets come from, which is what a NAT or a wrong " +
                "advertise host looks like.",
        )
    }

    val stale = d.peers.filter {
        d.evictThresholdMs > 0 && it.lastSeenMs > 0 && nowMs - it.lastSeenMs > d.evictThresholdMs / 2
    }
    if (stale.isNotEmpty()) {
        checks += HealthCheck(
            Severity.WARN,
            "${stale.size} peer(s) going stale",
            "No packet from ${stale.take(3).joinToString(", ") { it.nick.ifEmpty { it.addr } }} for over half " +
                "the eviction window. They will be dropped from the cluster shortly.",
        )
    }

    val silent = d.peers.filter { it.packetsIn == 0L && it.packetsOut > 0 }
    if (silent.isNotEmpty()) {
        checks += HealthCheck(
            Severity.ERROR,
            "${silent.size} peer(s) never answered",
            "This node sends to ${silent.take(3).joinToString(", ") { it.addr }} and receives nothing back. " +
                "One-way UDP like this is a firewall or a wrong advertised port, not packet loss.",
        )
    }

    val components = d.topology.components()
    if (components.size > 1) {
        checks += HealthCheck(
            Severity.ERROR,
            "Cluster looks partitioned into ${components.size} groups",
            "Known nodes split as " + components.joinToString(" | ") { "${it.size}" } +
                ". Each group converges on its own and diverges from the others until a link is restored.",
        )
    }

    val unconfirmed = d.topology.unconfirmed()
    if (unconfirmed.isNotEmpty()) {
        checks += HealthCheck(
            Severity.INFO,
            "${unconfirmed.size} link(s) claimed by one end only",
            "Gossip carries each node's own peer list, so a link either end has not yet reported stays " +
                "unconfirmed. Normal shortly after a node joins.",
        )
    }

    if (d.tombstoneTtlSec == 0L && d.store.tombstones > 0) {
        checks += HealthCheck(
            Severity.INFO,
            "${d.store.tombstones} tombstone(s) kept forever",
            "Deletes replicate as tombstones and are never reclaimed while the GC age is 0. Set one " +
                "in Settings if deletes are frequent — it has to exceed the longest outage you " +
                "expect a node to come back from. A node that was away longer still holds the " +
                "value, and pushes it back once the tombstone is gone, silently undoing the delete.",
        )
    }

    // The other half of that advice, which nothing said once it was taken. A
    // reclaimed tombstone is the only record that a key was deleted, so the
    // undoing it warns about leaves no trace at all: the delete is reversed on
    // every replica and the consistency check calls the result converged,
    // because it is. It cannot be detected after the fact — a node cannot tell
    // a resurrection from someone writing the key again — so the exposure is
    // named while it is configured, which is the only honest moment.
    if (d.tombstoneTtlSec > 0) {
        checks += HealthCheck(
            Severity.INFO,
            "Tombstones are reclaimed after ${d.tombstoneTtlSec}s",
            "A delete is remembered for ${d.tombstoneTtlSec}s and then forgotten. A node that is " +
                "away for longer comes back still holding the value, and the delete is undone on " +
                "every replica with nothing to report it — the cluster converges on the " +
                "resurrected value. Keep the age above the longest outage a node can return from, " +
                "or set it to 0 and let tombstones accumulate.",
        )
    }

    if (!d.pskSet) {
        checks += HealthCheck(
            Severity.INFO,
            "No pre-shared key",
            "The framing key comes from the cluster name alone. That keeps two clusters on one network " +
                "apart but provides no secrecy: anyone on the network can read and write.",
        )
    }

    // A stopped node already says so; a second line claiming health would only
    // dilute it.
    if (d.running && checks.none { it.severity == Severity.WARN || it.severity == Severity.ERROR }) {
        checks.add(
            0,
            HealthCheck(
                Severity.OK,
                "Node looks healthy",
                "${d.peers.size} peer(s), ${d.store.keys} key(s), no dropped packets.",
            ),
        )
    }
    return checks
}

/** The diagnostics snapshot as text, for pasting into a bug report. */
fun NodeDiagnostics.asReport(nowMs: Long): String = buildString {
    appendLine("rezoagwe node diagnostics")
    appendLine("addr            $addr")
    appendLine("nick            $nick")
    appendLine("node id         $nodeId")
    appendLine("cluster         $cluster (key ${keyFingerprint}${if (pskSet) ", psk set" else ", no psk"})")
    appendLine("running         $running${if (running) ", up ${uptimeSec}s" else ""}")
    appendLine("port            configured $configuredPort, bound $boundPort, streams $streamListener")
    appendLine("addresses       local $localIpv4, advertising ${advertiseHost.ifEmpty { "(auto)" }}")
    appendLine("seeds           ${seeds.joinToString(", ").ifEmpty { "(none)" }}")
    appendLine("intervals       gossip ${gossipIntervalMs}ms, heartbeat ${heartbeatIntervalMs}ms, evict ${evictThresholdMs}ms")
    appendLine("store           ${store.keys} keys, ${store.tombstones} tombstones, ${store.valueBytes} value bytes, clock ${store.clock}")
    appendLine("traffic         ${metrics.packetsSent} sent / ${metrics.packetsReceived} received, ${metrics.bytesSent}B / ${metrics.bytesReceived}B")
    appendLine("drops           auth ${metrics.authFailures}, replay ${metrics.replayDrops}, skew ${metrics.skewDrops}, malformed ${metrics.malformedDrops}")
    appendLine("replication     ${metrics.kvApplied} applied, ${metrics.kvRejectedStale} stale, ${metrics.kvRefused} refused by limits")
    appendLine("chat delivery   ${metrics.chatGapsFound} gaps, ${metrics.chatRecovered} recovered, ${metrics.chatLost} lost, ${metrics.chatDuplicates} duplicates")
    appendLine("anti-entropy    ${metrics.aeRounds} rounds, ${metrics.aePushed} pushed, ${metrics.aePulled} pulled")
    appendLine()
    appendLine("peers (${peers.size})")
    for (p in peers) {
        appendLine(
            "  ${p.addr}  ${p.nick.ifEmpty { "-" }}  in ${p.packetsIn}/${p.bytesIn}B  out ${p.packetsOut}/${p.bytesOut}B  " +
                "seen ${if (p.lastSeenMs == 0L) "never" else "${(nowMs - p.lastSeenMs) / 1000}s ago"}  advertises ${p.advertisedPeers}",
        )
    }
    if (strangers.isNotEmpty()) {
        appendLine()
        appendLine("unknown sources (${strangers.size})")
        for (p in strangers) appendLine("  ${p.addr}  in ${p.packetsIn}/${p.bytesIn}B  rejected ${p.rejected}")
    }
    appendLine()
    appendLine("topology: ${topology.nodes.size} nodes, ${topology.links.size} links, ${topology.components().size} component(s)")
    for (link in topology.links) appendLine("  ${link.a} -- ${link.b} (${link.kind.name.lowercase()})")
    appendLine()
    appendLine("checks")
    for (c in healthChecks(this@asReport, nowMs)) appendLine("  [${c.severity.name}] ${c.title}: ${c.detail}")
}
