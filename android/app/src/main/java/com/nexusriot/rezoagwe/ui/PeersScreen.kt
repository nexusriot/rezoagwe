package com.nexusriot.rezoagwe.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.nexusriot.rezoagwe.core.NodeEngine
import com.nexusriot.rezoagwe.core.Runtime
import com.nexusriot.rezoagwe.service.NodeService
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch

@Composable
fun PeersScreen(node: NodeEngine) {
    val peers by node.peerList.collectAsState()
    val status by node.status.collectAsState()
    val settings by Runtime.settings.collectAsState()
    val context = LocalContext.current
    val scope = rememberCoroutineScope()

    Column(modifier = Modifier.fillMaxSize()) {
        Card(modifier = Modifier
            .fillMaxWidth()
            .padding(12.dp)) {
            Column(modifier = Modifier.padding(12.dp)) {
                Text("Node", fontWeight = FontWeight.Bold)
                Text(
                    text = "id ${status.nodeId.take(8)}  ·  cluster ${status.cluster}",
                    style = MaterialTheme.typography.bodySmall,
                    fontFamily = FontFamily.Monospace,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Text(
                    text = "seeds " + settings.seedList().joinToString(", ").ifEmpty { "(none)" },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Row(
                    modifier = Modifier.padding(top = 8.dp),
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    if (status.running) {
                        // Stopping sends a goodbye to every peer, so it belongs off
                        // the UI thread: Android refuses a datagram sent from there,
                        // and the goodbye was being dropped.
                        OutlinedButton(onClick = {
                            scope.launch(Dispatchers.IO) { Runtime.stopNode(context) }
                        }) { Text("Stop node") }
                    } else {
                        Button(onClick = {
                            scope.launch(Dispatchers.IO) { Runtime.startNode(context) }
                            // The service is what keeps the node gossiping once the
                            // screen goes away.
                            NodeService.ensureRunning(context)
                        }) { Text("Start node") }
                    }
                }
            }
        }

        if (peers.isEmpty()) {
            Text(
                text = if (status.running) {
                    "No peers yet. Check that a bootstrap seed is reachable, or that another node is on this network."
                } else {
                    "The node is stopped."
                },
                modifier = Modifier.padding(16.dp),
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }

        LazyColumn(modifier = Modifier.fillMaxSize()) {
            items(peers, key = { it.addr }) { peer ->
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(horizontal = 12.dp, vertical = 10.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Column(modifier = Modifier.weight(1f)) {
                        Text(
                            text = peer.nick.ifEmpty { peer.addr },
                            fontWeight = FontWeight.Bold,
                            color = colorFor(peer.addr),
                        )
                        Text(
                            text = peer.addr,
                            style = MaterialTheme.typography.bodySmall,
                            fontFamily = FontFamily.Monospace,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                    val age = (System.currentTimeMillis() - peer.lastSeenMs) / 1000
                    Text(
                        text = "seen ${formatUptime(age)} ago",
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.outline,
                    )
                }
                HorizontalDivider()
            }
        }
    }
}
