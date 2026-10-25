package com.nexusriot.rezoagwe.core

import com.nexusriot.rezoagwe.proto.ChatEntry
import com.nexusriot.rezoagwe.proto.WireJson
import java.io.File
import java.io.RandomAccessFile
import java.security.MessageDigest
import kotlinx.serialization.Serializable
import kotlinx.serialization.SerialName
import kotlinx.serialization.encodeToString
import kotlinx.serialization.decodeFromString

@Serializable private data class JournalRecord(val data: String, val sha256: String)
@Serializable private data class JournalDelta(
    @SerialName("node_id") val nodeId: String = "",
    val clock: Long = 0,
    val chat: List<ChatEntry> = emptyList(),
    @SerialName("chat_seq") val chatSeq: Long = 0,
    val set: Map<String,KvEntry> = emptyMap(),
    val remove: List<String> = emptyList(),
)
/** Checksummed deltas; a partial final record is recoverable, corruption is not. */
class Journal(private val file: File, val mode: String = "snapshot") {
    init { require(mode in listOf("snapshot","journal","journal-sync")) }
    private val wal = File(file.path + ".wal")
    private var previous = PersistState()
    private var records = 0
    private fun hash(data: String) = MessageDigest.getInstance("SHA-256").digest(data.toByteArray(Charsets.UTF_8)).joinToString("") { "%02x".format(it.toInt() and 255) }
    fun load(snapshot: PersistState?): PersistState? {
        var state = snapshot ?: PersistState()
        if (!wal.exists()) { previous = state; return snapshot }
        requireNotNull(snapshot) { "Journal checkpoint is missing; recovery blocked" }
        val entries = state.entries.toMutableMap()
        RandomAccessFile(wal,"rw").use { f ->
            val bytes = ByteArray(f.length().also { require(it <= Int.MAX_VALUE) }.toInt())
            f.readFully(bytes)
            var start = 0
            for (i in bytes.indices) if (bytes[i] == 10.toByte()) {
                val record = WireJson.decodeFromString<JournalRecord>(String(bytes,start,i-start,Charsets.UTF_8))
                require(hash(record.data) == record.sha256) { "Journal checksum mismatch at $start" }
                val d = WireJson.decodeFromString<JournalDelta>(record.data)
                entries.putAll(d.set); d.remove.forEach { entries.remove(it) }
                state = PersistState(d.nodeId,d.clock,emptyMap(),d.chat,d.chatSeq)
                start = i+1; records++
            }
            if (start < bytes.size) f.setLength(start.toLong())
        }
        state = state.copy(entries=entries.toMap()); previous=state
        return state
    }
    fun save(state: PersistState, checkpoint: (PersistState)->Unit) {
        if (mode == "snapshot") {
            if(wal.exists()) {
                val writer=Journal(file,"journal"); writer.previous=previous; writer.records=records
                writer.save(state,checkpoint)
            }
            checkpoint(state)
            check(!wal.exists() || wal.delete()) { "Cannot remove journal" }
            previous=state; return
        }
        file.parentFile?.mkdirs()
        if (!file.exists()) checkpoint(PersistState())
        val d=JournalDelta(state.nodeId,state.clock,state.chat,state.chatSeq,
            state.entries.filter { (k,v)->previous.entries[k]!=v }, previous.entries.keys.filter { it !in state.entries })
        val data=WireJson.encodeToString(d)
        val line=(WireJson.encodeToString(JournalRecord(data,hash(data)))+"\n").toByteArray(Charsets.UTF_8)
        RandomAccessFile(wal,"rw").use { f ->
            val before=f.length(); f.seek(before)
            try { f.write(line); if(mode=="journal-sync") f.fd.sync() }
            catch(e:Exception) { f.setLength(before); throw e }
        }
        if(mode=="journal-sync") syncDirectory()
        previous=state; records++
        if(records>=64 || wal.length()>16*1024*1024) {
            checkpoint(state)
            RandomAccessFile(wal,"rw").use { it.setLength(0); if(mode=="journal-sync") it.fd.sync() }
            records=0
        }
    }
    fun syncDirectory() {
        java.nio.channels.FileChannel.open(file.absoluteFile.parentFile!!.toPath(),java.nio.file.StandardOpenOption.READ).use { it.force(true) }
    }
}
