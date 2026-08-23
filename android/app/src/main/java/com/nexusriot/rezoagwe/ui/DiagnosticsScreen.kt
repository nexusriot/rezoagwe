package com.nexusriot.rezoagwe.ui

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.provider.Settings
import android.widget.Toast
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Card
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.nexusriot.rezoagwe.core.BootstrapServer
import com.nexusriot.rezoagwe.core.HealthCheck
import com.nexusriot.rezoagwe.core.NodeEngine
import com.nexusriot.rezoagwe.core.PeerDiagnostics
import com.nexusriot.rezoagwe.core.Runtime
import com.nexusriot.rezoagwe.core.Severity
import com.nexusriot.rezoagwe.core.asReport
import com.nexusriot.rezoagwe.core.deviceReport
import com.nexusriot.rezoagwe.core.healthChecks
import kotlinx.coroutines.delay

/** How often the snapshot is retaken. Long enough not to burn battery, short enough to feel live. */
private const val REFRESH_MS = 2_000L

/**
 * Everything about this node in one place, starting with what is wrong.
 *
 * The counters on the Activity tab say what happened; this says what it means —
 * which mismatch drops those packets, which peer never answers, whether the
 * cluster is split, and whether Android is about to stop the timers.
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun DiagnosticsScreen(node: NodeEngine, rendezvous: BootstrapServer) {
    val context = LocalContext.current
    val settings by Runtime.settings.collectAsState()
    val bootstrapStatus by rendezvous.status.collectAsState()
    var generation by remember { mutableIntStateOf(0) }
    var now by remember { mutableStateOf(System.currentTimeMillis()) }

    LaunchedEffect(Unit) {
        while (true) {
            delay(REFRESH_MS)
            now = System.currentTimeMillis()
            generation++
        }
    }

    val diagnostics = remember(generation) { node.diagnostics() }
    val device = remember(generation) { deviceReport(context) }
    val checks = remember(generation) { healthChecks(diagnostics, now) }

    Column(
        modifier = Modifier
            .fillMaxSize()
            .verticalScroll(rememberScrollState())
            .padding(12.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        Section("Health") {
            checks.forEach { CheckRow(it) }
        }

        Section("Identity") {
            Line("address", diagnostics.addr)
            Line("nickname", diagnostics.nick)
            Line("node id", diagnostics.nodeId)
            Line("cluster", diagnostics.cluster)
            Line("key fingerprint", diagnostics.keyFingerprint + if (diagnostics.pskSet) " (psk set)" else " (no psk)")
            Text(
                text = "Two nodes talk only when this fingerprint matches. Compare it with another " +
                    "device before suspecting the network.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(top = 4.dp),
            )
        }

        Section("Sockets") {
            Line("state", if (diagnostics.running) "running, up ${formatUptime(diagnostics.uptimeSec)}" else "stopped")
            Line("port", "configured ${diagnostics.configuredPort}, bound ${diagnostics.boundPort}")
            Line("stream listener", if (diagnostics.streamListener) "yes" else "no")
            Line("advertising", diagnostics.advertiseHost.ifEmpty { "${diagnostics.localIpv4} (auto)" })
            Line("seeds", diagnostics.seeds.joinToString(", ").ifEmpty { "(none)" })
            Line(
                "intervals",
                "gossip ${diagnostics.gossipIntervalMs / 1000}s · heartbeat ${diagnostics.heartbeatIntervalMs / 1000}s · " +
                    "evict ${diagnostics.evictThresholdMs / 1000}s · sweep ${diagnostics.sweepIntervalMs / 1000}s",
            )
            Line(
                "bootstrap role",
                if (bootstrapStatus.running) {
                    "listening on ${bootstrapStatus.port}, ${bootstrapStatus.nodes} registered"
                } else {
                    "stopped"
                },
            )
        }

        Section("Device") {
            Line("model", "${device.model} · Android ${device.androidRelease} (API ${device.sdk})")
            Line(
                "network",
                buildString {
                    append(device.transport)
                    if (device.metered) append(" · metered")
                    if (device.vpn) append(" · VPN")
                    append(if (device.validated) " · validated" else " · unvalidated")
                },
            )
            device.interfaces.forEach { nic ->
                Line(
                    nic.name,
                    (nic.ipv4 + nic.ipv6).joinToString(", ") + if (!nic.up) " (down)" else "",
                )
            }
            Line("battery optimisation", if (device.ignoringBatteryOptimizations) "exempt" else "applies")
            if (device.powerSaveMode) Line("power saver", "on")
            if (!device.ignoringBatteryOptimizations) {
                Text(
                    text = "Doze can freeze the gossip timers while the screen is off, and peers evict " +
                        "this node ${diagnostics.evictThresholdMs / 1000}s later. Exempting the app keeps it in the cluster.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(top = 4.dp),
                )
                OutlinedButton(
                    onClick = { openBatterySettings(context) },
                    modifier = Modifier.padding(top = 4.dp),
                ) { Text("Battery settings") }
            }
        }

        Section("Traffic") {
            val m = diagnostics.metrics
            Line("packets", "${m.packetsSent} sent · ${m.packetsReceived} received")
            Line("bytes", "${formatBytes(m.bytesSent)} sent · ${formatBytes(m.bytesReceived)} received")
            Line(
                "rate",
                "%.1f/s out · %.1f/s in · %s/s out · %s/s in".format(
                    m.rates.packetsSent,
                    m.rates.packetsReceived,
                    formatBytes(m.rates.bytesSent.toLong()),
                    formatBytes(m.rates.bytesReceived.toLong()),
                ),
            )
            Line("errors", "${m.sendErrors} send · ${m.streamErrors} stream")
            Line(
                "dropped",
                "${m.authFailures} auth · ${m.replayDrops} replay · ${m.skewDrops} skew · ${m.malformedDrops} malformed",
            )
            Line("anti-entropy", "${m.aeRounds} rounds · ${m.aePushed} pushed · ${m.aePulled} pulled")
            Line("state sync", "${m.stateSyncOut} served · ${m.stateSyncIn} received")
            if (m.kinds.isNotEmpty()) {
                HorizontalDivider(modifier = Modifier.padding(vertical = 6.dp))
                Text("By message kind", style = MaterialTheme.typography.labelMedium, fontWeight = FontWeight.Bold)
                m.kinds.forEach { kind -> Line(kind.kind, "${kind.sent} sent · ${kind.received} received") }
            }
        }

        Section("Store") {
            val s = diagnostics.store
            Line("keys", s.keys.toString())
            Line("tombstones", "${s.tombstones}${if (diagnostics.tombstoneTtlSec > 0) ", GC after ${diagnostics.tombstoneTtlSec}s" else ", kept forever"}")
            Line("value bytes", formatBytes(s.valueBytes))
            Line("lamport clock", s.clock.toString())
            Line("keys with history", s.historyKeys.toString())
            if (s.largestKey.isNotEmpty()) Line("largest value", "${s.largestKey} (${formatBytes(s.largestValueBytes.toLong())})")
            Line("digest cursor", s.digestCursor.ifEmpty { "(start of keyspace)" })
            Line("buffers", "${diagnostics.chatLines} chat lines · ${diagnostics.activityLines} activity lines")
        }

        Section("Peers (${diagnostics.peers.size})") {
            if (diagnostics.peers.isEmpty()) {
                Text(
                    text = "No peers. Nothing replicates until one is learned.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            diagnostics.peers.forEach { PeerRow(it, now) }
        }

        if (diagnostics.strangers.isNotEmpty()) {
            Section("Unknown sources (${diagnostics.strangers.size})") {
                Text(
                    text = "These addresses sent packets without being peers — the sign of a node whose " +
                        "advertised address is not the one it sends from.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                diagnostics.strangers.forEach { PeerRow(it, now) }
            }
        }

        Section("Topology") {
            val components = diagnostics.topology.components()
            Line("nodes", diagnostics.topology.nodes.size.toString())
            Line("links", diagnostics.topology.links.size.toString())
            Line("groups", components.size.toString())
            components.forEachIndexed { index, group ->
                Line("group ${index + 1}", "${group.size}: " + group.joinToString(", "))
            }
            diagnostics.topology.unconfirmed().forEach {
                Line("unconfirmed", "${it.a} → ${it.b}")
            }
        }

        FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            OutlinedButton(onClick = { copyReport(context, diagnostics.asReport(now)) }) { Text("Copy report") }
            OutlinedButton(onClick = { shareReport(context, diagnostics.asReport(now)) }) { Text("Share report") }
            OutlinedButton(onClick = { generation++ }) { Text("Refresh") }
        }

        Text(
            text = "Cluster \"${settings.cluster}\", refreshed every ${REFRESH_MS / 1000}s.",
            style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.colorScheme.outline,
        )
    }
}

@Composable
private fun Section(title: String, content: @Composable () -> Unit) {
    Card(modifier = Modifier.fillMaxWidth()) {
        Column(modifier = Modifier.padding(12.dp)) {
            Text(title, fontWeight = FontWeight.Bold)
            content()
        }
    }
}

@Composable
private fun Line(label: String, value: String) {
    Row(modifier = Modifier.fillMaxWidth().padding(top = 2.dp)) {
        Text(
            text = label,
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.widthIn(min = 116.dp),
        )
        Text(
            text = value,
            style = MaterialTheme.typography.bodySmall,
            fontFamily = FontFamily.Monospace,
            modifier = Modifier.weight(1f),
        )
    }
}

@Composable
private fun CheckRow(check: HealthCheck) {
    val tint = colorFor(check.severity)
    Column(modifier = Modifier.padding(vertical = 4.dp)) {
        Text(
            text = "${symbolFor(check.severity)} ${check.title}",
            style = MaterialTheme.typography.bodyMedium,
            fontWeight = FontWeight.Bold,
            color = tint,
        )
        Text(
            text = check.detail,
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun PeerRow(peer: PeerDiagnostics, nowMs: Long) {
    Column(modifier = Modifier.padding(vertical = 4.dp)) {
        Text(
            text = peer.nick.ifEmpty { peer.addr },
            fontWeight = FontWeight.Bold,
            style = MaterialTheme.typography.bodyMedium,
            color = colorFor(peer.addr),
        )
        if (peer.nick.isNotEmpty()) {
            Text(
                text = peer.addr,
                style = MaterialTheme.typography.labelSmall,
                fontFamily = FontFamily.Monospace,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Text(
            text = "in ${peer.packetsIn} pkt / ${formatBytes(peer.bytesIn)}  ·  " +
                "out ${peer.packetsOut} pkt / ${formatBytes(peer.bytesOut)}" +
                (if (peer.rejected > 0) "  ·  ${peer.rejected} rejected" else "") +
                (if (peer.sendErrors > 0) "  ·  ${peer.sendErrors} send errors" else ""),
            style = MaterialTheme.typography.labelSmall,
            fontFamily = FontFamily.Monospace,
        )
        if (peer.known) {
            Text(
                text = (if (peer.lastSeenMs > 0) "seen ${formatUptime((nowMs - peer.lastSeenMs) / 1000)} ago" else "never heard from") +
                    (if (peer.firstSeenMs > 0) "  ·  known for ${formatUptime((nowMs - peer.firstSeenMs) / 1000)}" else "") +
                    (if (peer.advertisedPeers >= 0) "  ·  advertises ${peer.advertisedPeers} peers" else "  ·  never gossiped"),
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

@Composable
private fun colorFor(severity: Severity): Color = when (severity) {
    Severity.OK -> MaterialTheme.colorScheme.primary
    Severity.INFO -> MaterialTheme.colorScheme.onSurfaceVariant
    Severity.WARN -> MaterialTheme.colorScheme.secondary
    Severity.ERROR -> MaterialTheme.colorScheme.error
}

private fun symbolFor(severity: Severity): String = when (severity) {
    Severity.OK -> "✓"
    Severity.INFO -> "·"
    Severity.WARN -> "!"
    Severity.ERROR -> "✗"
}

fun formatBytes(bytes: Long): String = when {
    bytes < 1024 -> "${bytes}B"
    bytes < 1024 * 1024 -> "%.1fkB".format(bytes / 1024.0)
    else -> "%.1fMB".format(bytes / (1024.0 * 1024))
}

private fun copyReport(context: Context, report: String) {
    val clipboard = context.getSystemService(Context.CLIPBOARD_SERVICE) as? ClipboardManager ?: return
    clipboard.setPrimaryClip(ClipData.newPlainText("rezoagwe diagnostics", report))
    Toast.makeText(context, "Diagnostics copied", Toast.LENGTH_SHORT).show()
}

private fun shareReport(context: Context, report: String) {
    val intent = Intent(Intent.ACTION_SEND).apply {
        type = "text/plain"
        putExtra(Intent.EXTRA_SUBJECT, "rezoagwe diagnostics")
        putExtra(Intent.EXTRA_TEXT, report)
    }
    context.startActivity(Intent.createChooser(intent, "Share diagnostics"))
}

private fun openBatterySettings(context: Context) {
    // The per-app prompt needs a permission Play discourages; the settings list is
    // the honest route and lands the user in the right place.
    context.startActivity(Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS))
}
