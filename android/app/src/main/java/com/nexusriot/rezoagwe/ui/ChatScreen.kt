package com.nexusriot.rezoagwe.ui

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.nexusriot.rezoagwe.core.NodeEngine
import com.nexusriot.rezoagwe.proto.ChatEntry
import com.nexusriot.rezoagwe.proto.ChatKind
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch

private val TIME = SimpleDateFormat("HH:mm:ss", Locale.US)

@Composable
fun ChatScreen(node: NodeEngine) {
    val messages by node.chat.collectAsState()
    val status by node.status.collectAsState()
    var draft by remember { mutableStateOf("") }
    val listState = rememberLazyListState()
    val scope = rememberCoroutineScope()

    LaunchedEffect(messages.size) {
        if (messages.isNotEmpty()) listState.animateScrollToItem(messages.size - 1)
    }

    Column(modifier = Modifier.fillMaxSize()) {
        LazyColumn(
            state = listState,
            modifier = Modifier
                .weight(1f)
                .fillMaxWidth(),
        ) {
            items(messages) { entry -> ChatLine(entry, status.addr) }
        }
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .padding(8.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            OutlinedTextField(
                value = draft,
                onValueChange = { draft = it },
                modifier = Modifier.weight(1f),
                placeholder = { Text("Message, or /help for commands") },
                singleLine = true,
            )
            IconButton(
                enabled = draft.isNotBlank(),
                onClick = {
                    val text = draft
                    draft = ""
                    // Off the main thread: sending dials peers, and an unreachable
                    // one must never stall the UI.
                    scope.launch(Dispatchers.IO) { node.submit(text) }
                },
            ) {
                Icon(Icons.AutoMirrored.Filled.Send, contentDescription = "Send")
            }
        }
    }
}

@Composable
private fun ChatLine(entry: ChatEntry, selfAddr: String) {
    val stamp = TIME.format(Date(entry.ts * 1000))
    val own = entry.sender == selfAddr
    val name = when {
        own -> "you"
        entry.nick.isNotEmpty() -> entry.nick
        entry.sender.isNotEmpty() -> entry.sender
        else -> "?"
    }

    Row(modifier = Modifier.padding(horizontal = 12.dp, vertical = 2.dp)) {
        Text(
            text = stamp,
            style = MaterialTheme.typography.labelSmall,
            fontFamily = FontFamily.Monospace,
            color = MaterialTheme.colorScheme.outline,
            modifier = Modifier.padding(end = 6.dp),
        )
        when (entry.kind) {
            ChatKind.SYSTEM -> Text(
                text = "» ${entry.text}",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.outline,
            )
            ChatKind.ACTION -> Text(
                text = "* $name ${entry.text}",
                style = MaterialTheme.typography.bodyMedium,
                color = colorFor(entry.sender),
            )
            ChatKind.DIRECT -> Column {
                Text(
                    text = if (own) "dm → ${entry.to}" else "dm ← $name",
                    style = MaterialTheme.typography.labelSmall,
                    fontWeight = FontWeight.Bold,
                    color = MaterialTheme.colorScheme.secondary,
                )
                Text(entry.text, style = MaterialTheme.typography.bodyMedium)
            }
            else -> Column {
                Text(
                    text = name,
                    style = MaterialTheme.typography.labelSmall,
                    fontWeight = FontWeight.Bold,
                    color = if (own) MaterialTheme.colorScheme.primary else colorFor(entry.sender),
                )
                Text(entry.text, style = MaterialTheme.typography.bodyMedium)
            }
        }
    }
}
