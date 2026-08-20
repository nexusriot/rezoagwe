package com.nexusriot.rezoagwe.ui

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import com.nexusriot.rezoagwe.core.NodeEngine
import com.nexusriot.rezoagwe.core.Runtime

@Composable
fun SettingsScreen() {
    val context = LocalContext.current
    val saved by Runtime.settings.collectAsState()

    var nick by remember(saved) { mutableStateOf(saved.nick) }
    var port by remember(saved) { mutableStateOf(saved.port.toString()) }
    var advertiseHost by remember(saved) { mutableStateOf(saved.advertiseHost) }
    var seeds by remember(saved) { mutableStateOf(saved.seeds) }
    var psk by remember(saved) { mutableStateOf(saved.psk) }
    var cluster by remember(saved) { mutableStateOf(saved.cluster) }
    var bootstrapPort by remember(saved) { mutableStateOf(saved.bootstrapPort.toString()) }
    var tombstoneTtl by remember(saved) { mutableStateOf(saved.tombstoneTtlSec.toString()) }

    Column(
        modifier = Modifier
            .fillMaxSize()
            .verticalScroll(rememberScrollState())
            .padding(16.dp),
    ) {
        Field(nick, { nick = it }, "Nickname")
        Field(port, { port = it.filter(Char::isDigit) }, "Node port", numeric = true)
        Field(
            value = advertiseHost,
            onChange = { advertiseHost = it },
            label = "Advertise host (blank = ${NodeEngine.localIpv4()})",
        )
        Field(seeds, { seeds = it }, "Bootstrap seeds, comma separated")
        Field(cluster, { cluster = it }, "Cluster name")
        Field(psk, { psk = it }, "Pre-shared key", secret = true)
        Field(bootstrapPort, { bootstrapPort = it.filter(Char::isDigit) }, "Bootstrap port", numeric = true)
        Field(
            value = tombstoneTtl,
            onChange = { tombstoneTtl = it.filter(Char::isDigit) },
            label = "Tombstone GC after (seconds, 0 = never)",
            numeric = true,
        )

        Text(
            text = "Every packet is authenticated. Without a pre-shared key the framing key comes " +
                "from the cluster name alone, which keeps two clusters on one network apart but " +
                "provides no secrecy. Nodes only talk to peers with the same key and cluster.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.padding(vertical = 12.dp),
        )

        Button(
            modifier = Modifier.fillMaxWidth(),
            onClick = {
                Runtime.applySettings(
                    context,
                    saved.copy(
                        nick = nick.ifBlank { "phone" },
                        port = port.toIntOrNull() ?: saved.port,
                        advertiseHost = advertiseHost.trim(),
                        seeds = seeds.trim(),
                        psk = psk,
                        cluster = cluster.ifBlank { saved.cluster },
                        bootstrapPort = bootstrapPort.toIntOrNull() ?: saved.bootstrapPort,
                        tombstoneTtlSec = tombstoneTtl.toLongOrNull() ?: 0,
                    ),
                )
            },
        ) { Text("Apply") }

        Text(
            text = "Changing the port, cluster or key restarts whichever role is running: those are " +
                "baked into the socket and the codec.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.padding(top = 8.dp),
        )
    }
}

@Composable
private fun Field(
    value: String,
    onChange: (String) -> Unit,
    label: String,
    numeric: Boolean = false,
    secret: Boolean = false,
) {
    OutlinedTextField(
        value = value,
        onValueChange = onChange,
        label = { Text(label) },
        singleLine = true,
        keyboardOptions = if (numeric) {
            KeyboardOptions(keyboardType = KeyboardType.Number)
        } else {
            KeyboardOptions.Default
        },
        visualTransformation = if (secret) PasswordVisualTransformation() else androidx.compose.ui.text.input.VisualTransformation.None,
        modifier = Modifier
            .fillMaxWidth()
            .padding(vertical = 4.dp),
    )
}
