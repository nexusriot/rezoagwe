package com.nexusriot.rezoagwe.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.WindowInsetsSides
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.only
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawing
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Chat
import androidx.compose.material.icons.filled.Hub
import androidx.compose.material.icons.filled.MonitorHeart
import androidx.compose.material.icons.filled.People
import androidx.compose.material.icons.filled.Router
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material.icons.filled.Timeline
import androidx.compose.material.icons.filled.VerticalSplit
import androidx.compose.material.icons.filled.VpnKey
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.NavigationRailItem
import androidx.compose.material3.Scaffold
import androidx.compose.material3.ScrollableTabRow
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Surface
import androidx.compose.material3.Tab
import androidx.compose.material3.Text
import androidx.compose.material3.VerticalDivider
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.nexusriot.rezoagwe.core.BootstrapServer
import com.nexusriot.rezoagwe.core.NodeEngine
import com.nexusriot.rezoagwe.core.NodeStatus
import com.nexusriot.rezoagwe.core.Runtime

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun RezoagweApp() {
    val engine by Runtime.engineFlow.collectAsState()
    val bootstrap by Runtime.bootstrapFlow.collectAsState()
    val error by Runtime.lastError.collectAsState()
    var screen by rememberSaveable { mutableStateOf(Screen.KEYS) }
    var split by rememberSaveable { mutableStateOf(true) }
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
    val configuration = LocalConfiguration.current
    val mode = paneModeFor(configuration.screenWidthDp, configuration.screenHeightDp)
    val twoPane = mode == PaneMode.EXPANDED && split

    Scaffold(
        snackbarHost = { SnackbarHost(snackbar) },
        // safeDrawing, not systemBars: it also covers the display cutout and the
        // keyboard, so the chat input rises with the keyboard instead of hiding
        // behind it.
        contentWindowInsets = WindowInsets.safeDrawing,
        topBar = {
            // The surface is outside the inset padding so its colour paints behind
            // the status bar; before this the header text ran under the clock.
            // fillMaxWidth, or the surface only paints as wide as its content and
            // leaves the status bar bare where the header text stops.
            Surface(tonalElevation = 3.dp, modifier = Modifier.fillMaxWidth()) {
                Column(
                    modifier = Modifier.windowInsetsPadding(
                        WindowInsets.safeDrawing.only(WindowInsetsSides.Top + WindowInsetsSides.Horizontal),
                    ),
                ) {
                    StatusHeader(
                        status = status,
                        compact = mode == PaneMode.COMPACT,
                        splitAvailable = mode == PaneMode.EXPANDED,
                        split = split,
                        onToggleSplit = { split = !split },
                    )
                    if (mode == PaneMode.COMPACT) {
                        ScrollableTabRow(selectedTabIndex = screen.ordinal, edgePadding = 0.dp) {
                            Screen.entries.forEach { entry ->
                                Tab(
                                    selected = screen == entry,
                                    onClick = { screen = entry },
                                    text = { Text(entry.title) },
                                )
                            }
                        }
                    }
                }
            }
        },
    ) { padding ->
        Row(modifier = Modifier.padding(padding).fillMaxSize()) {
            if (mode != PaneMode.COMPACT) {
                Rail(
                    current = screen,
                    companion = if (twoPane) companionOf(screen) else null,
                    onSelect = { screen = it },
                )
            }
            Box(modifier = Modifier.weight(1f)) {
                Pane(
                    screen = screen,
                    node = node,
                    rendezvous = rendezvous,
                    labelled = twoPane,
                )
            }
            if (twoPane) {
                VerticalDivider()
                val side = companionOf(screen)
                Box(modifier = Modifier.weight(1f)) {
                    Pane(
                        screen = side,
                        node = node,
                        rendezvous = rendezvous,
                        labelled = true,
                        onPromote = { screen = side },
                        onClose = { split = false },
                    )
                }
            }
        }
    }
}

/**
 * One screen, with a caption when it shares the window with another.
 *
 * Without the caption a two-pane layout is ambiguous: two lists of addresses side
 * by side do not say which is the peer table and which is the bootstrap roster.
 */
@Composable
private fun Pane(
    screen: Screen,
    node: NodeEngine,
    rendezvous: BootstrapServer,
    labelled: Boolean,
    onPromote: (() -> Unit)? = null,
    onClose: (() -> Unit)? = null,
) {
    Column(modifier = Modifier.fillMaxSize()) {
        if (labelled) {
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .background(MaterialTheme.colorScheme.surfaceVariant)
                    .padding(start = 12.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    text = screen.title.uppercase(),
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.weight(1f),
                )
                onPromote?.let {
                    IconButton(onClick = it) {
                        Icon(
                            imageVector = iconFor(screen),
                            contentDescription = "Show ${screen.title} in the main pane",
                        )
                    }
                }
                onClose?.let {
                    IconButton(onClick = it) {
                        Icon(Icons.Filled.VerticalSplit, contentDescription = "Close the second pane")
                    }
                }
            }
        }
        Surface(modifier = Modifier.fillMaxSize()) {
            when (screen) {
                Screen.KEYS -> KeysScreen(node)
                Screen.CHAT -> ChatScreen(node)
                Screen.PEERS -> PeersScreen(node)
                Screen.GRAPH -> GraphScreen(node)
                Screen.ACTIVITY -> ActivityScreen(node)
                Screen.DIAGNOSTICS -> DiagnosticsScreen(node, rendezvous)
                Screen.BOOTSTRAP -> BootstrapScreen(rendezvous)
                Screen.SETTINGS -> SettingsScreen()
            }
        }
    }
}

/**
 * The navigation for anything wider than a phone in portrait.
 *
 * Scrollable on purpose: eight destinations do not fit the height of a phone in
 * landscape, which is exactly where the rail is used to save vertical space.
 */
@Composable
private fun Rail(current: Screen, companion: Screen?, onSelect: (Screen) -> Unit) {
    Surface(tonalElevation = 2.dp) {
        Column(
            modifier = Modifier
                .fillMaxHeight()
                .width(84.dp)
                .verticalScroll(rememberScrollState())
                .padding(vertical = 8.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
            verticalArrangement = Arrangement.spacedBy(2.dp),
        ) {
            Screen.entries.forEach { entry ->
                NavigationRailItem(
                    selected = entry == current,
                    onClick = { onSelect(entry) },
                    icon = { Icon(iconFor(entry), contentDescription = entry.title) },
                    label = {
                        Text(
                            text = entry.title,
                            style = MaterialTheme.typography.labelSmall,
                            color = if (entry == companion) {
                                MaterialTheme.colorScheme.secondary
                            } else {
                                Color.Unspecified
                            },
                        )
                    },
                )
            }
        }
    }
}

fun iconFor(screen: Screen): ImageVector = when (screen) {
    Screen.KEYS -> Icons.Filled.VpnKey
    Screen.CHAT -> Icons.AutoMirrored.Filled.Chat
    Screen.PEERS -> Icons.Filled.People
    Screen.GRAPH -> Icons.Filled.Hub
    Screen.ACTIVITY -> Icons.Filled.Timeline
    Screen.DIAGNOSTICS -> Icons.Filled.MonitorHeart
    Screen.BOOTSTRAP -> Icons.Filled.Router
    Screen.SETTINGS -> Icons.Filled.Settings
}

/**
 * The line that answers "is this node actually part of a cluster right now?".
 *
 * The counters are chips in a flow row rather than one concatenated string: a
 * long address on a narrow screen wraps the row instead of pushing the rest of
 * the line off the display.
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun StatusHeader(
    status: NodeStatus,
    compact: Boolean,
    splitAvailable: Boolean,
    split: Boolean,
    onToggleSplit: () -> Unit,
) {
    val (label, color) = when {
        !status.running -> "STOPPED" to MaterialTheme.colorScheme.outline
        status.connected -> "CONNECTED" to MaterialTheme.colorScheme.primary
        else -> "DEGRADED" to MaterialTheme.colorScheme.error
    }

    val chips: @Composable () -> Unit = {
        FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
            Chip("peers", status.peers.toString(), if (status.running && status.peers == 0) color else null)
            Chip("keys", status.keys.toString(), null)
            if (status.tombstones > 0) Chip("tombs", status.tombstones.toString(), null)
            Chip("up", formatUptime(status.uptimeSec), null)
        }
    }

    Column(modifier = Modifier.padding(horizontal = 12.dp, vertical = 6.dp)) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Box(
                modifier = Modifier
                    .size(8.dp)
                    .background(color, CircleShape),
            )
            Text(
                text = label,
                color = color,
                fontWeight = FontWeight.Bold,
                style = MaterialTheme.typography.labelLarge,
                modifier = Modifier.padding(start = 6.dp, end = 10.dp),
            )
            Text(
                text = status.addr,
                style = MaterialTheme.typography.labelLarge,
                fontFamily = FontFamily.Monospace,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.weight(1f, fill = false),
            )
            if (!compact) {
                Box(modifier = Modifier.padding(start = 12.dp)) { chips() }
            }
            if (splitAvailable) {
                Box(modifier = Modifier.weight(1f)) {}
                IconButton(onClick = onToggleSplit) {
                    Icon(
                        imageVector = Icons.Filled.VerticalSplit,
                        contentDescription = if (split) "Use one pane" else "Use two panes",
                        tint = if (split) {
                            MaterialTheme.colorScheme.primary
                        } else {
                            MaterialTheme.colorScheme.onSurfaceVariant
                        },
                    )
                }
            }
        }
        if (compact) chips()
    }
}

@Composable
private fun Chip(label: String, value: String, tint: Color?) {
    Row {
        Text(
            text = label,
            style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.colorScheme.outline,
        )
        Text(
            text = " $value",
            style = MaterialTheme.typography.labelSmall,
            fontFamily = FontFamily.Monospace,
            fontWeight = FontWeight.Bold,
            color = tint ?: MaterialTheme.colorScheme.onSurfaceVariant,
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
