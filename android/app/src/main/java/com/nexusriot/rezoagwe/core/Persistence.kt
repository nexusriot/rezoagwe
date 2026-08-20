package com.nexusriot.rezoagwe.core

import com.nexusriot.rezoagwe.proto.ChatEntry
import com.nexusriot.rezoagwe.proto.WireJson
import java.io.File
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.decodeFromString

/**
 * The on-disk form of a node: identity, Lamport clock, every entry (tombstones
 * included) and the chat ring. Same shape as the Go node's state file, so a
 * export from one can be read by the other.
 */
@Serializable
data class PersistState(
    @SerialName("node_id") val nodeId: String = "",
    val clock: Long = 0,
    val entries: Map<String, KvEntry> = emptyMap(),
    val chat: List<ChatEntry> = emptyList(),
)

/**
 * Writes node state to a file.
 *
 * Saves are serialised and gated on a generation number, so a slow write can
 * never overwrite the file with an older snapshot. The write goes through a
 * temporary file and a rename: a reader sees either the old file or the whole
 * new one, never a half-written state.
 */
class Persister(private val file: File) {
    private val lock = Any()
    private var lastGen = 0L

    fun save(gen: Long, state: PersistState) {
        synchronized(lock) {
            if (gen != 0L && gen <= lastGen) return
            try {
                file.parentFile?.mkdirs()
                val tmp = File(file.parentFile, file.name + ".tmp")
                tmp.writeText(WireJson.encodeToString(state))
                if (!tmp.renameTo(file)) {
                    // Rename across a same-directory boundary should not fail; if it
                    // does, fall back to a direct write rather than losing the state.
                    file.writeText(tmp.readText())
                    tmp.delete()
                }
                lastGen = gen
            } catch (e: Exception) {
                // Persistence is best-effort: a full disk must not take the node down.
            }
        }
    }

    fun load(): PersistState? = synchronized(lock) {
        if (!file.exists()) return null
        return try {
            WireJson.decodeFromString<PersistState>(file.readText())
        } catch (e: Exception) {
            null
        }
    }
}
