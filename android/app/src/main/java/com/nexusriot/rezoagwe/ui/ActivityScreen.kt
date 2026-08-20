package com.nexusriot.rezoagwe.ui

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.Card
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.nexusriot.rezoagwe.core.MetricsSnapshot
import com.nexusriot.rezoagwe.core.NodeEngine

/**
 * Replication as it happens. Everything here is otherwise invisible: an update
 * rejected as stale looks exactly like nothing happening, which is when a
 * replication bug hides.
 */
@Composable
fun ActivityScreen(node: NodeEngine) {
    val activity by node.activity.collectAsState()
    val metrics by node.metricsFlow.collectAsState()

    Column(modifier = Modifier.fillMaxSize()) {
        MetricsCard(metrics)
        Text(
            text = "Activity",
            style = MaterialTheme.typography.titleSmall,
            modifier = Modifier.padding(start = 12.dp, top = 8.dp),
        )
        if (activity.isEmpty()) {
            Text(
                text = "Nothing replicated yet.",
                modifier = Modifier.padding(12.dp),
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        LazyColumn(modifier = Modifier.fillMaxSize()) {
            items(activity.reversed()) { line ->
                Text(
                    text = line,
                    style = MaterialTheme.typography.bodySmall,
                    fontFamily = FontFamily.Monospace,
                    modifier = Modifier.padding(horizontal = 12.dp, vertical = 2.dp),
                )
            }
        }
    }
}

@Composable
private fun MetricsCard(m: MetricsSnapshot) {
    Card(modifier = Modifier
        .fillMaxWidth()
        .padding(12.dp)) {
        Column(modifier = Modifier.padding(12.dp)) {
            Text("Replication", fontWeight = FontWeight.Bold)
            MetricRow("local writes", m.kvLocalWrites)
            MetricRow("remote applied", m.kvApplied)
            MetricRow("stale rejected", m.kvRejectedStale)
            MetricRow("guarded writes refused", m.kvCasFailures)
            MetricRow("expired", m.kvExpired)
            MetricRow("tombstones reclaimed", m.kvGced)

            Text("Anti-entropy", fontWeight = FontWeight.Bold, modifier = Modifier.padding(top = 8.dp))
            MetricRow("digests sent", m.aeRounds)
            MetricRow("entries pushed", m.aePushed)
            MetricRow("entries pulled", m.aePulled)
            MetricRow("snapshots served", m.stateSyncOut)
            MetricRow("snapshots received", m.stateSyncIn)

            Text("Traffic", fontWeight = FontWeight.Bold, modifier = Modifier.padding(top = 8.dp))
            MetricRow("packets sent", m.packetsSent)
            MetricRow("packets received", m.packetsReceived)
            MetricRow("send errors", m.sendErrors)
            MetricRow("failed authentication", m.authFailures)
            MetricRow("replays", m.replayDrops)
            MetricRow("clock skew", m.skewDrops)
            MetricRow("malformed", m.malformedDrops)
        }
    }
}

@Composable
private fun MetricRow(label: String, value: Long) {
    Row(modifier = Modifier.fillMaxWidth()) {
        Text(
            text = label,
            style = MaterialTheme.typography.bodySmall,
            modifier = Modifier.weight(1f),
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Text(
            text = value.toString(),
            style = MaterialTheme.typography.bodySmall,
            fontFamily = FontFamily.Monospace,
        )
    }
}
