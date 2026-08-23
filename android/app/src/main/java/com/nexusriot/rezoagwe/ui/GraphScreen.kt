package com.nexusriot.rezoagwe.ui

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.gestures.detectTransformGestures
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.CenterFocusStrong
import androidx.compose.material3.Card
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.PathEffect
import androidx.compose.ui.graphics.drawscope.DrawScope
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.text.TextMeasurer
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.drawText
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.rememberTextMeasurer
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.unit.toSize
import com.nexusriot.rezoagwe.core.GraphLayout
import com.nexusriot.rezoagwe.core.GraphNode
import com.nexusriot.rezoagwe.core.GraphPoint
import com.nexusriot.rezoagwe.core.LinkKind
import com.nexusriot.rezoagwe.core.NodeEngine
import com.nexusriot.rezoagwe.core.NodeRole
import com.nexusriot.rezoagwe.core.Topology
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/** Below this the node is drawn as fading: nothing has been heard from it lately. */
private const val STALE_AFTER_MS = 30_000L

/**
 * The cluster as a picture.
 *
 * A peer list says who this node talks to; it cannot show that two of those peers
 * do not talk to each other, or that the cluster has quietly split in two. Both
 * are visible here, drawn from the peer lists gossip already carries.
 */
@Composable
fun GraphScreen(node: NodeEngine) {
    val topology by node.topology.collectAsState()
    val scope = rememberCoroutineScope()
    var selected by remember { mutableStateOf<String?>(null) }
    var scale by remember { mutableFloatStateOf(1f) }
    var pan by remember { mutableStateOf(Offset.Zero) }
    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }

    // Ages are only true for an instant; without a tick the graph shows how stale
    // a peer was when the screen opened.
    LaunchedEffect(Unit) {
        while (true) {
            delay(1_000)
            now = System.currentTimeMillis()
        }
    }

    val positions = remember(topology) { GraphLayout.place(topology) }
    val components = remember(topology) { topology.components() }
    val measurer = rememberTextMeasurer()
    val colors = MaterialTheme.colorScheme

    Column(modifier = Modifier.fillMaxSize()) {
        GraphSummary(topology, components.size)

        Box(modifier = Modifier.weight(1f).fillMaxWidth()) {
            Canvas(
                modifier = Modifier
                    .fillMaxSize()
                    .pointerInput(Unit) {
                        detectTransformGestures { _, panChange, zoomChange, _ ->
                            scale = (scale * zoomChange).coerceIn(0.5f, 4f)
                            pan += panChange
                        }
                    }
                    .pointerInput(positions, scale, pan) {
                        detectTapGestures { tap ->
                            selected = nodeAt(tap, positions, size.toSize(), scale, pan)
                        }
                    },
            ) {
                drawGraph(
                    topology = topology,
                    positions = positions,
                    scale = scale,
                    pan = pan,
                    selected = selected,
                    nowMs = now,
                    measurer = measurer,
                    linkColor = colors.outline,
                    directColor = colors.primary,
                    labelColor = colors.onSurface,
                    mutedColor = colors.onSurfaceVariant,
                )
            }
            if (scale != 1f || pan != Offset.Zero) {
                IconButton(
                    onClick = {
                        scale = 1f
                        pan = Offset.Zero
                    },
                    modifier = Modifier.align(Alignment.TopEnd).padding(4.dp),
                ) {
                    Icon(Icons.Filled.CenterFocusStrong, contentDescription = "Reset the view")
                }
            }
            Legend(modifier = Modifier.align(Alignment.BottomStart).padding(8.dp))
        }

        selected?.let { addr ->
            NodeDetail(
                topology = topology,
                addr = addr,
                nowMs = now,
                onDismiss = { selected = null },
                onSync = {
                    // Dialing a peer blocks; never on the frame the tap arrived on.
                    scope.launch(Dispatchers.IO) { node.requestStateFrom(addr) }
                },
                onAdd = { scope.launch(Dispatchers.IO) { node.addPeer(addr) } },
                onForget = { scope.launch(Dispatchers.IO) { node.forgetPeer(addr) } },
            )
        }
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun GraphSummary(topology: Topology, components: Int) {
    val direct = topology.nodes.count { it.role == NodeRole.DIRECT }
    val indirect = topology.nodes.count { it.role == NodeRole.INDIRECT }
    Column(modifier = Modifier.padding(horizontal = 12.dp, vertical = 8.dp)) {
        FlowRow(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
            Text("${topology.nodes.size} nodes", style = MaterialTheme.typography.labelMedium)
            Text("${topology.links.size} links", style = MaterialTheme.typography.labelMedium)
            Text("$direct direct", style = MaterialTheme.typography.labelMedium)
            if (indirect > 0) {
                Text(
                    text = "$indirect via gossip",
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        if (components > 1) {
            Text(
                text = "Partitioned: $components groups that cannot reach each other.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.error,
            )
        } else if (topology.nodes.size <= 1) {
            Text(
                text = "This node alone. Peers appear here as they are learned.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun Legend(modifier: Modifier = Modifier) {
    FlowRow(modifier = modifier, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
        LegendItem("this node", MaterialTheme.colorScheme.primary)
        LegendItem("peer · colour per address", colorFor("peer-legend"))
        LegendItem("heard of only", MaterialTheme.colorScheme.outline)
    }
}

@Composable
private fun LegendItem(label: String, color: Color) {
    Row(verticalAlignment = Alignment.CenterVertically) {
        Canvas(modifier = Modifier.size(8.dp)) { drawCircle(color) }
        Text(
            text = " $label",
            style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun NodeDetail(
    topology: Topology,
    addr: String,
    nowMs: Long,
    onDismiss: () -> Unit,
    onSync: () -> Unit,
    onAdd: () -> Unit,
    onForget: () -> Unit,
) {
    val graphNode = topology.node(addr) ?: return
    val neighbours = topology.neighboursOf(addr)
    Card(modifier = Modifier.fillMaxWidth().padding(8.dp)) {
        Column(modifier = Modifier.padding(12.dp)) {
            Text(graphNode.label, fontWeight = FontWeight.Bold, color = colorFor(addr))
            Text(
                text = addr,
                style = MaterialTheme.typography.bodySmall,
                fontFamily = FontFamily.Monospace,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Text(
                text = when (graphNode.role) {
                    NodeRole.SELF -> "This device."
                    NodeRole.DIRECT -> "A peer of this node."
                    NodeRole.INDIRECT -> "Known only from gossip: no packets exchanged with it."
                },
                style = MaterialTheme.typography.bodySmall,
            )
            Text(
                text = "links ${graphNode.degree}" +
                    (if (graphNode.advertised >= 0) " · advertises ${graphNode.advertised} peers" else "") +
                    (if (graphNode.lastSeenMs > 0) " · heard ${formatUptime((nowMs - graphNode.lastSeenMs) / 1000)} ago" else ""),
                style = MaterialTheme.typography.labelSmall,
                fontFamily = FontFamily.Monospace,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            if (neighbours.isNotEmpty()) {
                Text(
                    text = "talks to " + neighbours.joinToString(", "),
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(top = 4.dp),
                )
            }
            Row(
                modifier = Modifier.padding(top = 8.dp),
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                when (graphNode.role) {
                    NodeRole.DIRECT -> {
                        OutlinedButton(onClick = onSync) { Text("Sync from") }
                        OutlinedButton(onClick = onForget) { Text("Forget") }
                    }
                    NodeRole.INDIRECT -> OutlinedButton(onClick = onAdd) { Text("Add as peer") }
                    NodeRole.SELF -> Unit
                }
                TextButton(onClick = onDismiss) { Text("Close") }
            }
        }
    }
}

/** Screen position of a normalised point, honouring the current zoom and pan. */
private fun place(x: Float, y: Float, size: Size, scale: Float, pan: Offset): Offset {
    val radius = (minOf(size.width, size.height) / 2f - 44f).coerceAtLeast(1f)
    return Offset(
        x = size.width / 2f + x * radius * scale + pan.x,
        y = size.height / 2f + y * radius * scale + pan.y,
    )
}

/** The node under a tap, or null. Generous radius: a fingertip is wider than a dot. */
private fun nodeAt(
    tap: Offset,
    positions: Map<String, GraphPoint>,
    size: Size,
    scale: Float,
    pan: Offset,
): String? = positions.entries
    .map { (addr, p) -> addr to (place(p.x, p.y, size, scale, pan) - tap).getDistance() }
    .filter { it.second <= 48f }
    .minByOrNull { it.second }
    ?.first

private fun DrawScope.drawGraph(
    topology: Topology,
    positions: Map<String, GraphPoint>,
    scale: Float,
    pan: Offset,
    selected: String?,
    nowMs: Long,
    measurer: TextMeasurer,
    linkColor: Color,
    directColor: Color,
    labelColor: Color,
    mutedColor: Color,
) {
    val dashed = PathEffect.dashPathEffect(floatArrayOf(8f, 8f))
    for (link in topology.links) {
        val a = positions[link.a] ?: continue
        val b = positions[link.b] ?: continue
        val from = place(a.x, a.y, size, scale, pan)
        val to = place(b.x, b.y, size, scale, pan)
        val highlighted = selected != null && (link.a == selected || link.b == selected)
        drawLine(
            color = when {
                highlighted -> directColor
                link.kind == LinkKind.DIRECT -> directColor.copy(alpha = 0.7f)
                link.kind == LinkKind.MUTUAL -> linkColor
                else -> linkColor.copy(alpha = 0.5f)
            },
            start = from,
            end = to,
            strokeWidth = if (highlighted) 4f else if (link.kind == LinkKind.OBSERVED) 2f else 3f,
            pathEffect = if (link.kind == LinkKind.OBSERVED) dashed else null,
        )
    }

    for (graphNode in topology.nodes) {
        val point = positions[graphNode.addr] ?: continue
        val at = place(point.x, point.y, size, scale, pan)
        val stale = graphNode.lastSeenMs > 0 && nowMs - graphNode.lastSeenMs > STALE_AFTER_MS
        val radius = when (graphNode.role) {
            NodeRole.SELF -> 20f
            NodeRole.DIRECT -> 16f
            NodeRole.INDIRECT -> 12f
        }
        val fill = when (graphNode.role) {
            NodeRole.SELF -> directColor
            NodeRole.DIRECT -> colorFor(graphNode.addr)
            NodeRole.INDIRECT -> mutedColor.copy(alpha = 0.35f)
        }
        drawCircle(color = fill.copy(alpha = if (stale) 0.4f else 1f), radius = radius, center = at)
        if (graphNode.role == NodeRole.INDIRECT) {
            drawCircle(color = mutedColor, radius = radius, center = at, style = Stroke(width = 2f, pathEffect = dashed))
        }
        if (graphNode.addr == selected) {
            drawCircle(color = directColor, radius = radius + 8f, center = at, style = Stroke(width = 3f))
        }
        val labelGap = if (graphNode.addr == selected) 14f else 4f
        drawLabel(graphNode, at, radius + labelGap, measurer, labelColor, mutedColor, stale)
    }
}

private fun DrawScope.drawLabel(
    graphNode: GraphNode,
    at: Offset,
    below: Float,
    measurer: TextMeasurer,
    labelColor: Color,
    mutedColor: Color,
    stale: Boolean,
) {
    val text = graphNode.label.take(16)
    val layout = measurer.measure(
        text = text,
        style = TextStyle(
            fontSize = 11.sp,
            fontWeight = if (graphNode.role == NodeRole.SELF) FontWeight.Bold else FontWeight.Normal,
            color = if (stale) mutedColor else labelColor,
        ),
    )
    drawText(
        textLayoutResult = layout,
        topLeft = Offset(at.x - layout.size.width / 2f, at.y + below),
    )
}
