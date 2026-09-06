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
import com.nexusriot.rezoagwe.core.BootstrapServer
import com.nexusriot.rezoagwe.core.NodeEngine
import com.nexusriot.rezoagwe.core.Runtime
import com.nexusriot.rezoagwe.service.NodeService
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch

/**
 * The rendezvous role. Running it here makes this phone the meeting point a LAN
 * cluster forms around — it never sees KV data or chat, only addresses.
 */
@Composable
fun BootstrapScreen(server: BootstrapServer) {
    val roster by server.roster.collectAsState()
    val status by server.status.collectAsState()
    val context = LocalContext.current
    val scope = rememberCoroutineScope()

    Column(modifier = Modifier.fillMaxSize()) {
        Card(modifier = Modifier
            .fillMaxWidth()
            .padding(12.dp)) {
            Column(modifier = Modifier.padding(12.dp)) {
                Text("Bootstrap service", fontWeight = FontWeight.Bold)
                Text(
                    text = if (status.running) {
                        "listening on udp+tcp :${status.port} · ${status.nodes} nodes · up ${formatUptime(status.uptimeSec)}"
                    } else {
                        "stopped · port ${status.port}"
                    },
                    style = MaterialTheme.typography.bodySmall,
                    fontFamily = FontFamily.Monospace,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Text(
                    text = "Point other nodes at ${NodeEngine.localIpv4()}:${status.port}",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(top = 4.dp),
                )
                Row(
                    modifier = Modifier.padding(top = 8.dp),
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    if (status.running) {
                        OutlinedButton(onClick = {
                            scope.launch(Dispatchers.IO) { Runtime.stopBootstrap(context) }
                        }) { Text("Stop") }
                    } else {
                        // Binding the rendezvous sockets is I/O; it does not belong
                        // on the frame the button was pressed on.
                        Button(onClick = {
                            scope.launch(Dispatchers.IO) { Runtime.startBootstrap(context) }
                            NodeService.ensureRunning(context)
                        }) { Text("Start") }
                    }
                }
            }
        }

        if (roster.isEmpty()) {
            Text(
                text = "No nodes registered.",
                modifier = Modifier.padding(16.dp),
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }

        LazyColumn(modifier = Modifier.fillMaxSize()) {
            items(roster, key = { it.addr }) { entry ->
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(horizontal = 12.dp, vertical = 10.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Column(modifier = Modifier.weight(1f)) {
                        Text(entry.nick.ifEmpty { entry.addr }, fontWeight = FontWeight.Bold)
                        Text(
                            text = entry.addr,
                            style = MaterialTheme.typography.bodySmall,
                            fontFamily = FontFamily.Monospace,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                    // A frozen "seen 0s ago" under a stopped service reads as a live
                    // roster: nothing is checking in, so nothing is ageing either.
                    val age = System.currentTimeMillis() / 1000 - entry.lastSeen
                    Text(
                        text = if (status.running) "seen ${formatUptime(age)} ago" else "registered",
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.outline,
                    )
                }
                HorizontalDivider()
            }
        }
    }
}
