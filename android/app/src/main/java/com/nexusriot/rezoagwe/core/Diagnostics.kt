package com.nexusriot.rezoagwe.core

/** How much attention a finding deserves. */
enum class Severity { OK, INFO, WARN, ERROR }

/**
 * One thing worth knowing about this node's health, phrased as what to do about
 * it. A counter on its own says a packet was dropped; a check says which
 * misconfiguration drops packets that way.
 */
data class HealthCheck(val severity: Severity, val title: String, val detail: String)

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
fun healthChecks(d: NodeDiagnostics, nowMs: Long): List<HealthCheck> {
    val checks = mutableListOf<HealthCheck>()
    val m = d.metrics

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
                last == null ->
                    "Datagrams could not leave the device — usually a peer address that no longer " +
                        "routes, or Wi-Fi dropping while the node stays up."
                // Worth its own wording: the packets are not lost on the network, they
                // were never handed to it, and no amount of looking at Wi-Fi will show that.
                last.onMainThread ->
                    "Datagrams were sent from the UI thread, which Android refuses — this is a bug " +
                        "in the app, not the network. The writes survive locally and reach peers " +
                        "only on the next anti-entropy round. Last: $last."
                else ->
                    "Datagrams could not leave the device — usually a peer address that no longer " +
                        "routes, or Wi-Fi dropping while the node stays up. Last: $last."
            },
        )
    }

    if (d.running && !d.streamListener) {
        checks += HealthCheck(
            Severity.WARN,
            "No stream listener on port ${d.configuredPort}",
            "Another app holds the TCP port, so large state syncs fall back to datagrams and a big " +
                "keyspace may take several gossip rounds to converge.",
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
                "The seeds ${d.seeds.joinToString(", ")} answered nothing. Check the bootstrap is running, " +
                    "that this device is on the same network, and that the cluster name and key match.",
            )
        }
    }

    val loopback = d.addr.startsWith("127.") || d.addr.startsWith("localhost:")
    if (d.running && loopback) {
        checks += HealthCheck(
            Severity.WARN,
            "Advertising a loopback address",
            "Peers are told to reach this node at ${d.addr}, which only resolves to this device. " +
                "Connect to a network, or set an advertise host in Settings.",
        )
    }

    if (d.advertiseHost.isNotEmpty() && d.localIpv4 != d.advertiseHost && !loopback) {
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
            "Deletes replicate as tombstones and are never reclaimed while the GC age is 0. Set one in " +
                "Settings if deletes are frequent.",
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
