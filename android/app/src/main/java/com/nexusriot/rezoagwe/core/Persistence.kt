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
    /**
     * How many chat broadcasts this node has sent, persisted for the same
     * reason the node id is: a restart that began counting again at 1 would
     * make every peer read the whole conversation as a gap.
     */
    @SerialName("chat_seq") val chatSeq: Long = 0,
)

/**
 * Writes node state to a file.
 *
 * Saves are serialised and gated on a generation number, so a slow write can
 * never overwrite the file with an older snapshot. The write goes through a
 * temporary file and a rename: a reader sees either the old file or the whole
 * new one, never a half-written state.
 */
class Persister(private val file: File, mode: String = "snapshot") {
    private val journal = Journal(file,mode)
    private val lock = Any()
    private var lastGen = 0L
    @Volatile var health = PersistenceHealth(mode=mode)
        private set
    private var blocked = false

    fun save(gen: Long, state: PersistState) {
        synchronized(lock) {
            if (gen != 0L && gen <= lastGen) return
            health = health.copy(pending = true, durable = false)
            if (blocked) return
            try {
                journal.save(state) { checkpoint ->
                    file.parentFile?.mkdirs()
                    val tmp = File(file.parentFile, file.name + ".tmp")
                    java.io.FileOutputStream(tmp).use { out ->
                        out.write(WireJson.encodeToString(checkpoint).toByteArray(Charsets.UTF_8))
                        if (journal.mode == "journal-sync") out.fd.sync()
                    }
                    if (!tmp.renameTo(file)) throw java.io.IOException("atomic state rename failed")
                    if (journal.mode == "journal-sync") journal.syncDirectory()
                }
                lastGen = gen
                health = health.copy(pending = false, durable = journal.mode == "journal-sync", lastSuccessMs = System.currentTimeMillis(), error = "")
            } catch (e: Exception) {
                health = health.copy(error = e.message ?: e.javaClass.simpleName)
            }
        }
    }

    /**
     * Why the last [load] could not read the file, or "" when it had nothing to
     * read or read it fine.
     *
     * A node that cannot read its state file does not fail to start — losing
     * the store is bad and refusing to start is worse — but it starts with an
     * empty replica *and a freshly minted identity*, and then rewrites the
     * unreadable file with that new state. Its earlier writes are outranked
     * until anti-entropy refills it, and every chat line it has said reads as
     * coming from a different author. Swallowing the exception left none of
     * that visible anywhere.
     */
    @Volatile
    var loadError: String = ""
        private set

    fun load(): PersistState? = synchronized(lock) {
        val snapshot = try {
            if (file.exists()) WireJson.decodeFromString<PersistState>(file.readText()) else null
        } catch(e:Exception) {
            loadError = e.message ?: e.javaClass.simpleName
            val backup=File(file.path+".corrupt-"+java.util.UUID.randomUUID())
            if(file.isFile && file.renameTo(backup)) {
                val wal=File(file.path+".wal")
                if(wal.exists() && !wal.renameTo(File(backup.path+".wal"))) blocked=true
                health=health.copy(error=loadError,recoveryFile=backup.absolutePath)
            } else { blocked=true; health=health.copy(error="$loadError; recovery blocked") }
            return@synchronized null
        }
        try { journal.load(snapshot).also { loadError="" } }
        catch(e:Exception) { blocked=true; loadError=e.message ?: "Journal recovery failed"; health=health.copy(error=loadError); null }
    }
}

data class PersistenceHealth(
    val mode: String = "snapshot",
    val durable: Boolean = false,
    val enabled: Boolean = true,
    val pending: Boolean = false,
    val lastSuccessMs: Long = 0,
    val error: String = "",
    val recoveryFile: String = "",
)
