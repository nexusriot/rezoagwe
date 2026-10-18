package com.nexusriot.rezoagwe.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.History
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Checkbox
import androidx.compose.material3.FloatingActionButton
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import com.nexusriot.rezoagwe.core.Entry
import com.nexusriot.rezoagwe.core.NodeEngine
import com.nexusriot.rezoagwe.proto.Version
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

@Composable
fun KeysScreen(node: NodeEngine) {
    val entries by node.entries.collectAsState()
    // Which dialog is open survives a configuration change, because losing it
    // takes the half-typed key and value with it. A rotation is the common one
    // and a tablet is the device that gets rotated; a process death while the
    // dialog is open is the same loss, more rarely.
    //
    // The row being edited is held by key rather than by Entry: an Entry is not
    // saveable, and the key is the identity anyway. The version the dialog
    // opened against is kept beside it because that, not the current version, is
    // what the optional guard has to compare — re-reading the entry on restore
    // would quietly let through a peer write made while the screen was turning.
    var editingKey by rememberSaveable { mutableStateOf<String?>(null) }
    var editingVersionCounter by rememberSaveable { mutableStateOf(0L) }
    var editingVersionNode by rememberSaveable { mutableStateOf("") }
    var creating by rememberSaveable { mutableStateOf(false) }
    var historyOf by rememberSaveable { mutableStateOf<String?>(null) }
    var confirmDelete by rememberSaveable { mutableStateOf<String?>(null) }
    // A refused guarded write used to leave only a line in the chat log, which is
    // not the screen the user is looking at: the edit simply vanished.
    var notice by rememberSaveable { mutableStateOf<String?>(null) }
    val scope = rememberCoroutineScope()

    Box(modifier = Modifier.fillMaxSize()) {
        if (entries.isEmpty()) {
            Text(
                text = "No keys yet.",
                modifier = Modifier.align(Alignment.Center),
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        LazyColumn(
            modifier = Modifier.fillMaxSize(),
            // Room for the button that would otherwise sit on the last row.
            contentPadding = PaddingValues(bottom = 88.dp),
        ) {
            notice?.let { text ->
                item {
                    Row(
                        modifier = Modifier
                            .fillMaxWidth()
                            .background(MaterialTheme.colorScheme.errorContainer)
                            .clickable { notice = null }
                            .padding(horizontal = 12.dp, vertical = 10.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        Text(
                            text = text,
                            modifier = Modifier.weight(1f),
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onErrorContainer,
                        )
                        Text(
                            text = "Dismiss",
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.onErrorContainer,
                        )
                    }
                }
            }
            items(entries, key = { it.key }) { entry ->
                KeyRow(
                    entry = entry,
                    onEdit = {
                        editingKey = entry.key
                        editingVersionCounter = entry.version.counter
                        editingVersionNode = entry.version.node
                    },
                    onHistory = { historyOf = entry.key },
                    onDelete = { confirmDelete = entry.key },
                )
                HorizontalDivider()
            }
        }
        FloatingActionButton(
            onClick = { creating = true },
            modifier = Modifier
                .align(Alignment.BottomEnd)
                .padding(16.dp),
        ) {
            Icon(Icons.Filled.Add, contentDescription = "New key")
        }
    }

    if (creating) {
        KeyDialog(
            title = "New key",
            initialKey = "",
            initialValue = "",
            initialTtl = 0,
            keyEditable = true,
            guardHint = "Guard (only write if the key does not exist)",
            onDismiss = { creating = false },
            onSave = { key, value, ttl, guard ->
                creating = false
                notice = null
                // The write broadcasts to every peer, so it runs off the UI thread:
                // Android refuses a datagram sent from there, and the update reached
                // the cluster only on the next anti-entropy round.
                scope.launch {
                    // A store refusal is not a lost guard and must not be
                    // reported as one: "it already exists" is the wrong thing
                    // to tell someone whose value was simply too big, and for
                    // a while it was not reported at all.
                    val rejected = withContext(Dispatchers.IO) { node.admit(key, value) }
                    if (rejected != null) {
                        val why = "$key was not written: ${rejected.explain()}"
                        node.system(why)
                        notice = why
                        return@launch
                    }
                    val refused = withContext(Dispatchers.IO) {
                        if (guard) {
                            node.compareAndSet(key, value, ttl, Version()) == null
                        } else {
                            node.set(key, value, ttl)
                            false
                        }
                    }
                    if (refused) {
                        val why = "guarded write to $key refused: it already exists"
                        node.system(why)
                        notice = why
                    }
                }
            },
        )
    }

    // The entry may have been deleted by a peer while the dialog was open, and
    // after a restore it may be gone entirely; the dialog closes rather than
    // editing something that is no longer there.
    editingKey?.let { openKey ->
        val entry = entries.firstOrNull { it.key == openKey }
        if (entry == null) {
            editingKey = null
            return@let
        }
        val openedAt = Version(editingVersionCounter, editingVersionNode)
        KeyDialog(
            title = "Edit ${entry.key}",
            initialKey = entry.key,
            initialValue = entry.value,
            initialTtl = if (entry.expiresAt > 0) {
                (entry.expiresAt - System.currentTimeMillis() / 1000).coerceAtLeast(0)
            } else {
                0
            },
            keyEditable = false,
            guardHint = "Guard (refuse if a peer changed it meanwhile)",
            onDismiss = { editingKey = null },
            onSave = { key, value, ttl, guard ->
                editingKey = null
                notice = null
                scope.launch {
                    val rejected = withContext(Dispatchers.IO) { node.admit(key, value) }
                    if (rejected != null) {
                        val why = "$key was not written: ${rejected.explain()}"
                        node.system(why)
                        notice = why
                        return@launch
                    }
                    val refused = withContext(Dispatchers.IO) {
                        if (guard) {
                            // The version on screen when the dialog opened is the guard:
                            // if a peer wrote the key in the meantime, the save is refused
                            // rather than silently overwriting the newer value.
                            node.compareAndSet(key, value, ttl, openedAt) == null
                        } else {
                            node.set(key, value, ttl)
                            false
                        }
                    }
                    if (refused) {
                        val why = "guarded write to $key refused: it changed since the form opened"
                        node.system(why)
                        notice = why
                    }
                }
            },
        )
    }

    historyOf?.let { key ->
        HistoryDialog(node, key) { historyOf = null }
    }

    confirmDelete?.let { key ->
        AlertDialog(
            onDismissRequest = { confirmDelete = null },
            title = { Text("Delete $key?") },
            text = { Text("The delete replicates as a versioned tombstone, so it cannot be undone by a stale write.") },
            confirmButton = {
                TextButton(onClick = {
                    scope.launch(Dispatchers.IO) { node.delete(key) }
                    confirmDelete = null
                }) { Text("Delete") }
            },
            dismissButton = {
                TextButton(onClick = { confirmDelete = null }) { Text("Cancel") }
            },
        )
    }
}

@Composable
private fun KeyRow(entry: Entry, onEdit: () -> Unit, onHistory: () -> Unit, onDelete: () -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .clickable(onClick = onEdit)
            .padding(horizontal = 12.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(modifier = Modifier.weight(1f)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(displayKey(entry.key), fontWeight = FontWeight.Bold)
                if (entry.expiresAt > 0) {
                    val remaining = (entry.expiresAt - System.currentTimeMillis() / 1000).coerceAtLeast(0)
                    Text(
                        text = "  ⏳ ${formatUptime(remaining)}",
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.secondary,
                    )
                }
            }
            Text(
                text = displayValue(entry.value),
                style = MaterialTheme.typography.bodySmall,
                fontFamily = FontFamily.Monospace,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Text(
                text = "v${entry.version.counter}",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.outline,
            )
        }
        IconButton(onClick = onHistory) {
            Icon(Icons.Filled.History, contentDescription = "History")
        }
        IconButton(onClick = onDelete) {
            Icon(Icons.Filled.Delete, contentDescription = "Delete")
        }
    }
}

@Composable
private fun KeyDialog(
    title: String,
    initialKey: String,
    initialValue: String,
    initialTtl: Long,
    keyEditable: Boolean,
    guardHint: String,
    onDismiss: () -> Unit,
    onSave: (key: String, value: String, ttlSeconds: Long, guard: Boolean) -> Unit,
) {
    // Saveable, not merely remembered: everything in this dialog is typed by
    // hand, and a rotation threw all of it away along with the dialog itself.
    // The seeds are keyed on the initial key so that opening a different row
    // re-seeds the form rather than showing the previous row's text.
    var key by rememberSaveable(initialKey) { mutableStateOf(initialKey) }
    var value by rememberSaveable(initialKey) { mutableStateOf(initialValue) }
    var ttl by rememberSaveable(initialKey) {
        mutableStateOf(if (initialTtl > 0) initialTtl.toString() else "")
    }
    var guard by rememberSaveable(initialKey) { mutableStateOf(false) }

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = {
            Column(modifier = Modifier.widthIn(max = 520.dp)) {
                OutlinedTextField(
                    value = key,
                    onValueChange = { key = it },
                    label = { Text("Key") },
                    enabled = keyEditable,
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
                OutlinedTextField(
                    value = value,
                    onValueChange = { value = it },
                    label = { Text("Value") },
                    modifier = Modifier.fillMaxWidth(),
                )
                OutlinedTextField(
                    value = ttl,
                    onValueChange = { ttl = it.filter(Char::isDigit) },
                    label = { Text("TTL in seconds (blank = never expires)") },
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Checkbox(checked = guard, onCheckedChange = { guard = it })
                    Text(guardHint, style = MaterialTheme.typography.bodySmall)
                }
            }
        },
        confirmButton = {
            TextButton(
                enabled = key.isNotBlank(),
                onClick = { onSave(key.trim(), value, ttl.toLongOrNull() ?: 0, guard) },
            ) { Text("Save") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun HistoryDialog(node: NodeEngine, key: String, onDismiss: () -> Unit) {
    val history = remember(key) { node.history(key) }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("History of $key") },
        text = {
            if (history.isEmpty()) {
                Text("No recorded versions.")
            } else {
                LazyColumn {
                    // Newest first: the last thing that happened is what you want.
                    items(history.reversed()) { h ->
                        Column(modifier = Modifier.padding(vertical = 4.dp)) {
                            Text(
                                text = "v${h.version.counter} ${if (h.deleted) "delete" else "set"} " +
                                    "by ${node.writerName(h.version.node)} ${if (h.local) "(local)" else "(remote)"}",
                                style = MaterialTheme.typography.labelMedium,
                                fontWeight = FontWeight.Bold,
                            )
                            if (!h.deleted) {
                                Text(
                                    text = h.value.take(120),
                                    style = MaterialTheme.typography.bodySmall,
                                    fontFamily = FontFamily.Monospace,
                                )
                            }
                        }
                    }
                }
            }
        },
        confirmButton = { TextButton(onClick = onDismiss) { Text("Close") } },
    )
}
