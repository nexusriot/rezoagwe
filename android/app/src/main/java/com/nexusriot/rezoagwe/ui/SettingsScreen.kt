package com.nexusriot.rezoagwe.ui

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
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
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.VisualTransformation
import androidx.compose.ui.unit.dp
import com.nexusriot.rezoagwe.core.NodeEngine
import com.nexusriot.rezoagwe.core.Runtime
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch

@Composable
fun SettingsScreen() {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val saved by Runtime.settings.collectAsState()

    // Saveable, not merely remembered: a rotation threw away every unsaved edit
    // and reseeded the form from storage, which on a tablet is one wrist
    // movement away at any moment.
    var durability by rememberSaveable(saved) { mutableStateOf(saved.durability) }
    var nick by rememberSaveable(saved) { mutableStateOf(saved.nick) }
    var port by rememberSaveable(saved) { mutableStateOf(saved.port.toString()) }
    var advertiseHost by rememberSaveable(saved) { mutableStateOf(saved.advertiseHost) }
    var seeds by rememberSaveable(saved) { mutableStateOf(saved.seeds) }
    var psk by rememberSaveable(saved) { mutableStateOf(saved.psk) }
    var cluster by rememberSaveable(saved) { mutableStateOf(saved.cluster) }
    var bootstrapPort by rememberSaveable(saved) { mutableStateOf(saved.bootstrapPort.toString()) }
    var tombstoneTtl by rememberSaveable(saved) { mutableStateOf(saved.tombstoneTtlSec.toString()) }
    // What the last Apply would not keep. Without this a refused value stays in
    // the field with nothing in effect behind it.
    var refusals by rememberSaveable { mutableStateOf(listOf<String>()) }
    var pendingSwitch by rememberSaveable(stateSaver = androidx.compose.runtime.saveable.listSaver<com.nexusriot.rezoagwe.core.Settings?, Any>(
        save = { s -> if (s == null) emptyList() else listOf(s.nick, s.port, s.advertiseHost, s.seeds, s.psk, s.cluster, s.bootstrapPort, s.tombstoneTtlSec, s.durability) },
        restore = { v -> if (v.isEmpty()) null else com.nexusriot.rezoagwe.core.Settings(v[0] as String, v[1] as Int, v[2] as String, v[3] as String, v[4] as String, v[5] as String, v[6] as Int, v[7] as Long, v[8] as String) },
    )) { mutableStateOf<com.nexusriot.rezoagwe.core.Settings?>(null) }
    pendingSwitch?.let { target ->
        androidx.compose.material3.AlertDialog(
            onDismissRequest = { pendingSwitch = null },
            title = { Text("Switch cluster profile?") },
            text = { Text("Switch from ${saved.cluster} to ${target.cluster}. Current values, chat and identity stay in their profile. The destination loads its own data or starts empty with a new identity. Nothing is copied. Use Export then Import to migrate data explicitly.") },
            confirmButton = { Button(onClick = {
                pendingSwitch = null
                scope.launch(Dispatchers.IO) { Runtime.applySettings(context, target) }
            }) { Text("Switch") } },
            dismissButton = { Button(onClick = { pendingSwitch = null }) { Text("Cancel") } },
        )
    }

    val export = androidx.activity.compose.rememberLauncherForActivityResult(
        androidx.activity.result.contract.ActivityResultContracts.CreateDocument("application/json")
    ) { uri -> if (uri != null) scope.launch(Dispatchers.IO) {
        try { context.contentResolver.openOutputStream(uri)?.use { it.write(Runtime.engine(context).export().toByteArray()) } }
        catch (e: Exception) { refusals = listOf("Export failed: ${e.message}") }
    } }
    val importLauncher = androidx.activity.compose.rememberLauncherForActivityResult(
        androidx.activity.result.contract.ActivityResultContracts.OpenDocument()
    ) { uri -> if (uri != null) scope.launch(Dispatchers.IO) {
        try { context.contentResolver.openInputStream(uri)?.bufferedReader()?.use { Runtime.engine(context).import(it.readText(), false) } }
        catch (e: Exception) { refusals = listOf("Import failed: ${e.message}") }
    } }
    Column(
        modifier = Modifier
            .fillMaxSize()
            .verticalScroll(rememberScrollState())
            .padding(16.dp)
            .widthIn(max = 560.dp),
    ) {
        Field(nick, { nick = it }, "Nickname")
        Field(durability, { durability = it }, "Durability: snapshot, journal, journal-sync")
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
                "provides no secrecy. Nodes only talk to peers with the same key and cluster. " +
                "Spaces around the key and the cluster name are trimmed, since a keyboard that " +
                "adds one would otherwise cut this node off with nothing on screen to show it.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.padding(vertical = 12.dp),
        )

        Button(
            modifier = Modifier.fillMaxWidth(),
            onClick = {
                val typed = saved.copy(
                    durability = durability,
                    nick = nick,
                    port = port.toIntOrNull() ?: saved.port,
                    advertiseHost = advertiseHost,
                    seeds = seeds,
                    psk = psk,
                    cluster = cluster,
                    bootstrapPort = bootstrapPort.toIntOrNull() ?: saved.bootstrapPort,
                    tombstoneTtlSec = tombstoneTtl.toLongOrNull() ?: 0,
                )
                val updated = typed.sanitized(saved)
                refusals = typed.refusals(saved)
                // The form must never show a value that is not in effect, and the
                // state flow does not re-seed it when the only edit was refused:
                // the sanitized settings are identical to the stored ones, so
                // nothing recomposes. Put the kept values back by hand.
                nick = updated.nick
                port = updated.port.toString()
                advertiseHost = updated.advertiseHost
                seeds = updated.seeds
                psk = updated.psk
                cluster = updated.cluster
                bootstrapPort = updated.bootstrapPort.toString()
                tombstoneTtl = updated.tombstoneTtlSec.toString()
                // Applying restarts a role, which closes and rebinds sockets: not
                // work for the frame the button was pressed on.
                if (updated.cluster != saved.cluster) pendingSwitch = updated
                else scope.launch(Dispatchers.IO) { Runtime.applySettings(context, updated) }
            },
        ) { Text("Apply") }

        Text("Data migration is explicit. Export before switching, then import into the destination profile (merge by version).")
        Button(onClick = { export.launch("rezoagwe-export.json") }) { Text("Export profile data") }
        Button(onClick = { importLauncher.launch(arrayOf("application/json")) }) { Text("Import into this profile") }
        if (refusals.isNotEmpty()) {
            Text(
                text = refusals.joinToString("\n"),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.error,
                modifier = Modifier.padding(top = 8.dp),
            )
        }

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
        visualTransformation = if (secret) PasswordVisualTransformation() else VisualTransformation.None,
        modifier = Modifier
            .fillMaxWidth()
            .padding(vertical = 4.dp),
    )
}
