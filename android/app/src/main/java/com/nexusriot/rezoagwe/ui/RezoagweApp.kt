package com.nexusriot.rezoagwe.ui

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.ScrollableTabRow
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Surface
import androidx.compose.material3.Tab
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.nexusriot.rezoagwe.core.NodeStatus
import com.nexusriot.rezoagwe.core.Runtime

private val TABS = listOf("Keys", "Chat", "Peers", "Activity", "Bootstrap", "Settings")

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun RezoagweApp() {
    val engine by Runtime.engineFlow.collectAsState()
    val bootstrap by Runtime.bootstrapFlow.collectAsState()
    val error by Runtime.lastError.collectAsState()
    var tab by rememberSaveable { mutableIntStateOf(0) }
    val snackbar = remember { SnackbarHostState() }

    LaunchedEffect(error) {
        error?.let {
            snackbar.showSnackbar(it)
            Runtime.clearError()
        }
    }

    val node = engine
    val rendezvous = bootstrap
    if (node == null || rendezvous == null) return

    val status by node.status.collectAsState()

    Scaffold(
        snackbarHost = { SnackbarHost(snackbar) },
        topBar = {
            Column {
                StatusBar(status)
                ScrollableTabRow(selectedTabIndex = tab, edgePadding = 0.dp) {
                    TABS.forEachIndexed { index, title ->
                        Tab(
                            selected = tab == index,
                            onClick = { tab = index },
                            text = { Text(title) },
                        )
                    }
                }
            }
        },
    ) { padding ->
        Surface(modifier = Modifier.padding(padding)) {
            when (tab) {
                0 -> KeysScreen(node)
                1 -> ChatScreen(node)
                2 -> PeersScreen(node)
                3 -> ActivityScreen(node)
                4 -> BootstrapScreen(rendezvous)
                else -> SettingsScreen()
            }
        }
    }
}

/** The one line that answers "is this node actually part of a cluster right now?". */
@Composable
private fun StatusBar(status: NodeStatus) {
    val (label, color) = when {
        !status.running -> "STOPPED" to MaterialTheme.colorScheme.outline
        status.connected -> "CONNECTED" to MaterialTheme.colorScheme.primary
        else -> "DEGRADED" to MaterialTheme.colorScheme.error
    }
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(horizontal = 12.dp, vertical = 6.dp),
    ) {
        Text(
            text = label,
            color = color,
            fontWeight = FontWeight.Bold,
            style = MaterialTheme.typography.labelLarge,
        )
        Text(
            text = "  ${status.addr}  ·  peers ${status.peers}  ·  keys ${status.keys}" +
                (if (status.tombstones > 0) "  ·  tombs ${status.tombstones}" else "") +
                "  ·  up ${formatUptime(status.uptimeSec)}",
            style = MaterialTheme.typography.labelLarge,
            fontFamily = FontFamily.Monospace,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

fun formatUptime(seconds: Long): String {
    val h = seconds / 3600
    val m = (seconds % 3600) / 60
    val s = seconds % 60
    return when {
        h > 0 -> "%dh%02dm%02ds".format(h, m, s)
        m > 0 -> "%dm%02ds".format(m, s)
        else -> "${s}s"
    }
}

/** Colour a sender consistently, so the same peer keeps the same tint across screens. */
fun colorFor(seed: String): Color {
    if (seed.isEmpty()) return Color.Gray
    var h = 0
    for (c in seed) h = h * 131 + c.code
    val palette = listOf(
        Color(0xFFE57373), Color(0xFF81C784), Color(0xFF64B5F6), Color(0xFFBA68C8),
        Color(0xFF4DD0E1), Color(0xFFFFB74D), Color(0xFFAED581), Color(0xFFFFF176),
        Color(0xFF4FC3F7), Color(0xFF9575CD), Color(0xFFF06292), Color(0xFF4DB6AC),
    )
    return palette[Math.floorMod(h, palette.size)]
}
