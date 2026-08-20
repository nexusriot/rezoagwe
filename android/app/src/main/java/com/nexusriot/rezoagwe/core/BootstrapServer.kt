package com.nexusriot.rezoagwe.core

import com.nexusriot.rezoagwe.net.Packet
import com.nexusriot.rezoagwe.net.UdpTransport
import com.nexusriot.rezoagwe.net.readFrame
import com.nexusriot.rezoagwe.net.writeFrame
import com.nexusriot.rezoagwe.proto.BootstrapDiscover
import com.nexusriot.rezoagwe.proto.BootstrapPeer
import com.nexusriot.rezoagwe.proto.BootstrapRegister
import com.nexusriot.rezoagwe.proto.BootstrapRoster
import com.nexusriot.rezoagwe.proto.Codec
import com.nexusriot.rezoagwe.proto.Kind
import com.nexusriot.rezoagwe.proto.WireJson
import java.io.File
import java.net.Socket
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.decodeFromString

data class BootstrapConfig(
    val port: Int = 9999,
    val nodeTimeoutMs: Long = 30_000,
    val psk: String = "",
    val cluster: String = Codec.DEFAULT_CLUSTER,
)

/** A registered node: where it is, what it calls itself, and when it last checked in. */
@Serializable
data class RosterEntry(
    val addr: String = "",
    val nick: String = "",
    val lastSeen: Long = 0,
)

@Serializable
private data class RosterFile(val nodes: List<RosterEntry> = emptyList())

data class BootstrapStatus(
    val running: Boolean = false,
    val port: Int = 0,
    val nodes: Int = 0,
    val uptimeSec: Long = 0,
)

/**
 * The rendezvous service: it knows the set of node addresses and nothing else —
 * no KV data, no chat. Running it on a phone makes that phone the meeting point
 * for a LAN cluster.
 */
class BootstrapServer(
    var config: BootstrapConfig,
    private val dataFile: File?,
    private val scope: CoroutineScope,
) {
    private val lock = Any()
    private val nodes = LinkedHashMap<String, RosterEntry>()
    private var codec = Codec(config.psk, config.cluster)
    private var transport: UdpTransport? = null
    private var startedAtMs = 0L
    private val jobs = mutableListOf<Job>()

    private val _roster = MutableStateFlow<List<RosterEntry>>(emptyList())
    val roster: StateFlow<List<RosterEntry>> = _roster.asStateFlow()

    private val _status = MutableStateFlow(BootstrapStatus())
    val status: StateFlow<BootstrapStatus> = _status.asStateFlow()

    init {
        load()
        publish()
    }

    val isRunning: Boolean get() = synchronized(lock) { transport != null }

    fun start() {
        synchronized(lock) {
            if (transport != null) return
            codec = Codec(config.psk, config.cluster)
            startedAtMs = System.currentTimeMillis()
            transport = UdpTransport(
                bindPort = config.port,
                onPacket = ::handlePacket,
                onStream = ::serveStream,
            )
        }
        jobs += scope.launch { sweepLoop() }
        publish()
    }

    fun stop() {
        val tr = synchronized(lock) { transport } ?: return
        jobs.forEach { it.cancel() }
        jobs.clear()
        tr.close()
        synchronized(lock) { transport = null }
        publish()
    }

    /** Records (or refreshes) a node, reporting whether this was a new arrival. */
    fun register(addr: String, nick: String): Boolean {
        if (addr.isEmpty()) return false
        val isNew: Boolean
        var changed: Boolean
        synchronized(lock) {
            val previous = nodes[addr]
            isNew = previous == null
            val entry = RosterEntry(
                addr = addr,
                nick = nick.ifEmpty { previous?.nick.orEmpty() },
                lastSeen = System.currentTimeMillis() / 1000,
            )
            changed = isNew || previous?.nick != entry.nick
            nodes[addr] = entry
        }
        if (changed) save()
        publish()
        return isNew
    }

    fun removeStale(): List<RosterEntry> {
        val cutoff = System.currentTimeMillis() / 1000 - config.nodeTimeoutMs / 1000
        val removed = synchronized(lock) {
            val stale = nodes.values.filter { it.lastSeen < cutoff }
            stale.forEach { nodes.remove(it.addr) }
            stale
        }
        if (removed.isNotEmpty()) {
            save()
            publish()
        }
        return removed
    }

    /** The roster as sent on the wire, minus the requester: a node has no use for its own address. */
    fun rosterFor(exclude: String): BootstrapRoster = synchronized(lock) {
        BootstrapRoster(
            peers = nodes.values
                .filter { it.addr != exclude }
                .sortedBy { it.addr }
                .map { BootstrapPeer(addr = it.addr, nick = it.nick, lastSeen = it.lastSeen) },
        )
    }

    private fun handlePacket(packet: Packet) {
        val frame = try {
            codec.decode(packet.data)
        } catch (e: Exception) {
            return // unauthenticated traffic on an open port is background noise
        }
        handle(frame.kind, String(frame.body)) { reply ->
            synchronized(lock) { transport }?.send(replyAddr(packet.from, frame.kind, String(frame.body)), reply)
        }
    }

    private fun serveStream(conn: Socket) {
        val frame = try {
            codec.decode(readFrame(conn.getInputStream()))
        } catch (e: Exception) {
            return
        }
        handle(frame.kind, String(frame.body)) { reply ->
            writeFrame(conn.getOutputStream(), reply)
        }
    }

    private fun handle(kind: Byte, body: String, reply: (ByteArray) -> Unit) {
        try {
            when (kind) {
                Kind.BOOTSTRAP_REGISTER -> {
                    val reg = WireJson.decodeFromString<BootstrapRegister>(body)
                    if (reg.from.isNotEmpty()) register(reg.from, reg.nick)
                }
                Kind.BOOTSTRAP_DISCOVER -> {
                    val req = WireJson.decodeFromString<BootstrapDiscover>(body)
                    // Asking for the roster is itself proof of life, so a joiner
                    // whose REGISTER was lost still ends up known.
                    if (req.from.isNotEmpty()) register(req.from, "")
                    reply(codec.encode(Kind.BOOTSTRAP_ROSTER, rosterFor(req.from)))
                }
            }
        } catch (e: Exception) {
            // Malformed body: drop it.
        }
    }

    /**
     * Where a datagram answer goes. The address a node advertises wins: a node
     * bound to a wildcard address is reachable there, while the source port of its
     * datagram may be ephemeral.
     */
    private fun replyAddr(from: String, kind: Byte, body: String): String {
        if (kind == Kind.BOOTSTRAP_DISCOVER) {
            val advertised = try {
                WireJson.decodeFromString<BootstrapDiscover>(body).from
            } catch (e: Exception) {
                ""
            }
            if (advertised.isNotEmpty()) return advertised
        }
        return from
    }

    private suspend fun sweepLoop() {
        val interval = (config.nodeTimeoutMs / 2).coerceAtLeast(1000)
        while (scope.isActive) {
            delay(interval)
            removeStale()
            publish()
        }
    }

    private fun load() {
        val file = dataFile ?: return
        if (!file.exists()) return
        try {
            // Every restored node's last-seen time is reset: the service was down,
            // so nobody could have checked in, and expiring the whole roster the
            // instant it loads would make persistence pointless.
            val now = System.currentTimeMillis() / 1000
            val saved = WireJson.decodeFromString<RosterFile>(file.readText())
            synchronized(lock) {
                saved.nodes.filter { it.addr.isNotEmpty() }.forEach {
                    nodes[it.addr] = it.copy(lastSeen = now)
                }
            }
        } catch (e: Exception) {
            // A corrupt roster is not worth failing to start over.
        }
    }

    private fun save() {
        val file = dataFile ?: return
        try {
            val snapshot = synchronized(lock) { RosterFile(nodes.values.toList()) }
            file.parentFile?.mkdirs()
            val tmp = File(file.parentFile, file.name + ".tmp")
            tmp.writeText(WireJson.encodeToString(snapshot))
            if (!tmp.renameTo(file)) {
                file.writeText(tmp.readText())
                tmp.delete()
            }
        } catch (e: Exception) {
            // Best effort.
        }
    }

    private fun publish() {
        _roster.value = synchronized(lock) { nodes.values.sortedBy { it.addr } }
        _status.value = BootstrapStatus(
            running = isRunning,
            port = config.port,
            nodes = _roster.value.size,
            uptimeSec = if (startedAtMs == 0L) 0 else (System.currentTimeMillis() - startedAtMs) / 1000,
        )
    }
}
