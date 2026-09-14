package com.nexusriot.rezoagwe.core

import com.nexusriot.rezoagwe.net.Packet
import com.nexusriot.rezoagwe.net.UdpTransport
import com.nexusriot.rezoagwe.net.readFrame
import com.nexusriot.rezoagwe.net.writeFrame
import com.nexusriot.rezoagwe.proto.BootstrapDiscover
import com.nexusriot.rezoagwe.proto.BootstrapRegister
import com.nexusriot.rezoagwe.proto.BootstrapRoster
import com.nexusriot.rezoagwe.proto.ChatEntry
import com.nexusriot.rezoagwe.proto.ChatKind
import com.nexusriot.rezoagwe.proto.ChatMessage
import com.nexusriot.rezoagwe.proto.Codec
import com.nexusriot.rezoagwe.proto.DecodeError
import com.nexusriot.rezoagwe.proto.DecodeException
import com.nexusriot.rezoagwe.proto.Digest
import com.nexusriot.rezoagwe.proto.Fingerprint
import com.nexusriot.rezoagwe.proto.FingerprintReply
import com.nexusriot.rezoagwe.proto.Goodbye
import com.nexusriot.rezoagwe.proto.Hello
import com.nexusriot.rezoagwe.proto.KVAction
import com.nexusriot.rezoagwe.proto.KVBatch
import com.nexusriot.rezoagwe.proto.KVUpdate
import com.nexusriot.rezoagwe.proto.Kind
import com.nexusriot.rezoagwe.proto.PeerGossip
import com.nexusriot.rezoagwe.proto.PullRequest
import com.nexusriot.rezoagwe.proto.StateRequest
import com.nexusriot.rezoagwe.proto.StateResponse
import com.nexusriot.rezoagwe.proto.Version
import com.nexusriot.rezoagwe.proto.WireJson
import java.io.File
import java.net.Inet4Address
import java.net.NetworkInterface
import java.net.Socket
import java.util.UUID
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

private const val CHAT_RING = 500
private const val ACTIVITY_RING = 300
private const val STATE_SYNC_CHAT_LINES = 200

/** Entries in a state response that has to fit one datagram. The stream path has no such limit. */
private const val DATAGRAM_SYNC_LIMIT = 200

/** Soft byte budget and entry cap for one anti-entropy batch. */
private const val BATCH_BYTES = 48 * 1024
private const val BATCH_ENTRIES = 64

data class NodeConfig(
    val advertiseHost: String = "",
    val port: Int = 3137,
    val nick: String = "anon",
    val seeds: List<String> = emptyList(),
    val psk: String = "",
    val cluster: String = Codec.DEFAULT_CLUSTER,
    val gossipIntervalMs: Long = 10_000,
    val heartbeatIntervalMs: Long = 5_000,
    val evictThresholdMs: Long = 15_000,
    val sweepIntervalMs: Long = 5_000,
    val tombstoneTtlSec: Long = 0,
    /**
     * How many keys one anti-entropy digest advertises, and how much repair
     * traffic a single round may trigger.
     *
     * digestBatch must not exceed either budget: the sender's cursor advances by
     * the whole digest, so a range that identified more repairs than one round
     * can carry has its tail stranded until the cursor wraps the entire keyspace.
     * [effectiveDigestBatch] clamps it rather than letting that happen quietly.
     */
    val digestBatch: Int = 128,
    val maxPush: Int = 128,
    val maxPull: Int = 128,
    val limits: Limits = Limits(),
) {
    /** The digest width the engine actually uses: never wider than one round can repair. */
    val effectiveDigestBatch: Int get() = minOf(digestBatch, maxPush, maxPull)
}

data class Peer(
    val addr: String,
    val nick: String = "",
    val lastSeenMs: Long = 0,
    val firstSeenMs: Long = 0,
)

data class NodeStatus(
    val addr: String = "",
    val nick: String = "",
    val nodeId: String = "",
    val cluster: String = "",
    val peers: Int = 0,
    val keys: Int = 0,
    val tombstones: Int = 0,
    val uptimeSec: Long = 0,
    val running: Boolean = false,
) {
    val connected: Boolean get() = peers > 0
}

@Serializable
data class ExportFile(
    val node: String = "",
    val cluster: String = "",
    val exported_at: Long = 0,
    val entries: List<KVUpdate> = emptyList(),
)

/**
 * A rezoagwe peer: membership, replication, chat and anti-entropy.
 *
 * This is a port of the Go `node.Node`, and the two have to stay in step — every
 * rule here (last-write-wins merge, digest ranges, deterministic expiry) is part
 * of the protocol, not an implementation detail.
 */
class NodeEngine(
    config: NodeConfig,
    private val dataFile: File?,
    private val scope: CoroutineScope,
) {
    var config: NodeConfig = config
        private set

    private val persister = dataFile?.let { Persister(it) }
    val nodeId: String
    val store: KvStore
    val metrics = Metrics()

    private val codec = Codec(config.psk, config.cluster)
    private var transport: UdpTransport? = null
    private val jobs = mutableListOf<Job>()
    private val lock = Any()

    private val peers = LinkedHashMap<String, Peer>()
    private val idToAddr = HashMap<String, String>()
    // What each peer last told us its own peer list was. This is the only source
    // of links between two nodes that are not us, and so the only way the app can
    // draw the cluster rather than just our corner of it.
    private val gossipViews = LinkedHashMap<String, GossipView>()
    private val chatLog = mutableListOf<ChatEntry>()
    private val activityLog = mutableListOf<String>()
    private var persistGen = 0L

    // One cursor per peer. A shared cursor divided the keyspace among whichever
    // peers the random target happened to pick, so covering the whole store
    // against any single peer took as many wraps as there were peers.
    private val aeCursors = HashMap<String, String>()
    private var startedAtMs = 0L
    private var nick: String = config.nick

    private val _entries = MutableStateFlow<List<Entry>>(emptyList())
    val entries: StateFlow<List<Entry>> = _entries.asStateFlow()

    private val _peerList = MutableStateFlow<List<Peer>>(emptyList())
    val peerList: StateFlow<List<Peer>> = _peerList.asStateFlow()

    private val _chat = MutableStateFlow<List<ChatEntry>>(emptyList())
    val chat: StateFlow<List<ChatEntry>> = _chat.asStateFlow()

    private val _activity = MutableStateFlow<List<String>>(emptyList())
    val activity: StateFlow<List<String>> = _activity.asStateFlow()

    private val _status = MutableStateFlow(NodeStatus())
    val status: StateFlow<NodeStatus> = _status.asStateFlow()

    private val _metricsFlow = MutableStateFlow(MetricsSnapshot())
    val metricsFlow: StateFlow<MetricsSnapshot> = _metricsFlow.asStateFlow()

    private val _topology = MutableStateFlow(Topology())
    val topology: StateFlow<Topology> = _topology.asStateFlow()

    val addr: String get() = "${config.advertiseHost.ifEmpty { localIpv4() }}:${config.port}"

    init {
        val saved = persister?.load()
        // Identity is persisted, not derived from the address: a phone changes
        // networks constantly, and its writes have to keep sorting consistently.
        nodeId = saved?.nodeId?.takeIf { it.isNotEmpty() } ?: UUID.randomUUID().toString()
        store = KvStore(nodeId)
        store.setLimits(config.limits)
        if (saved != null) {
            store.loadState(saved.clock, saved.entries)
            chatLog.addAll(saved.chat)
        }
        store.setOnChange(::persist)
        if (saved == null || saved.nodeId != nodeId) persist()
        publishAll()
    }

    fun start() {
        synchronized(lock) {
            if (transport != null) return
            startedAtMs = System.currentTimeMillis()
            transport = UdpTransport(
                bindPort = config.port,
                onPacket = ::handlePacket,
                onStream = ::serveStream,
                onError = { what, e -> logActivity("$what: ${e.message}") },
            )
        }
        jobs += scope.launch { gossipLoop() }
        jobs += scope.launch { heartbeatLoop() }
        jobs += scope.launch { evictLoop() }
        jobs += scope.launch { sweepLoop() }
        scope.launch { join() }
        publishStatus()
    }

    fun stop() {
        val tr = synchronized(lock) { transport } ?: return
        // Announce first: peers drop us at once instead of waiting out the
        // eviction timeout.
        broadcast(Kind.GOODBYE, Goodbye(from = addr, nick = nick))
        jobs.forEach { it.cancel() }
        jobs.clear()
        tr.close()
        // Membership is what the socket learned, so it dies with the socket. Keeping
        // it left the peer list and the graph claiming a live cluster, frozen at
        // "seen 0s ago", for a node that had stopped listening.
        synchronized(lock) {
            transport = null
            peers.clear()
            idToAddr.clear()
            gossipViews.clear()
        }
        publishPeers()
        publishStatus()
    }

    val isRunning: Boolean get() = synchronized(lock) { transport != null }

    // ---- outbound -------------------------------------------------------

    private inline fun <reified T> send(addr: String, kind: Byte, value: T) {
        val tr = synchronized(lock) { transport } ?: return
        try {
            val pkt = codec.encode(kind, value)
            tr.send(addr, pkt)
            metrics.sent(kind, pkt.size)
            metrics.sentTo(normalizeAddr(addr), pkt.size)
        } catch (e: Exception) {
            // The reason matters: "3 send errors" reads as a flaky network, but the
            // cause can equally be this process (a datagram sent from the UI thread
            // throws NetworkOnMainThreadException). Record the class and message so
            // the diagnostics can tell those apart.
            metrics.sendFailed(normalizeAddr(addr), e)
        }
    }

    private inline fun <reified T> broadcast(kind: Byte, value: T) {
        for (peer in peerAddrs()) send(peer, kind, value)
    }

    private fun peerAddrs(): List<String> = synchronized(lock) { peers.keys.toList() }

    private fun helloMessage() = Hello(from = addr, nick = nick, id = nodeId)

    private fun helloAllPeers() = broadcast(Kind.HELLO, helloMessage())

    // ---- membership -----------------------------------------------------

    /** Adds a peer, returning true when it was newly learned. Malformed addresses are refused at the door. */
    fun addPeer(peerAddr: String): Boolean {
        if (peerAddr.isEmpty() || isSelf(peerAddr) || !validPeerAddr(peerAddr)) return false
        val added: Boolean
        val now = System.currentTimeMillis()
        synchronized(lock) {
            val existing = peers[peerAddr]
            added = existing == null
            peers[peerAddr] = (existing ?: Peer(peerAddr, firstSeenMs = now)).copy(lastSeenMs = now)
        }
        if (added) publishPeers()
        return added
    }

    private fun touchPeer(peerAddr: String) {
        if (peerAddr.isEmpty() || isSelf(peerAddr)) return
        synchronized(lock) {
            peers[peerAddr]?.let { peers[peerAddr] = it.copy(lastSeenMs = System.currentTimeMillis()) }
        }
    }

    private fun setNick(peerAddr: String, peerNick: String) {
        if (peerAddr.isEmpty() || peerNick.isEmpty()) return
        synchronized(lock) {
            peers[peerAddr]?.let { peers[peerAddr] = it.copy(nick = peerNick) }
        }
        publishPeers()
    }

    private fun removePeer(peerAddr: String) {
        synchronized(lock) {
            peers.remove(peerAddr)
            idToAddr.entries.removeAll { it.value == peerAddr }
            // A departed peer's cursor goes with it, or a long-running node
            // accumulates one per address it has ever spoken to, and a peer that
            // returns resumes a walk from before it left.
            aeCursors.remove(peerAddr)
        }
        publishPeers()
    }

    /**
     * Drops a peer now instead of waiting out the eviction window.
     *
     * Gossip will usually teach it back, which is the point: it is how a link can
     * be cut on purpose to see whether the cluster heals.
     */
    fun forgetPeer(peerAddr: String) {
        val known = synchronized(lock) { peers.containsKey(peerAddr) }
        if (!known) return
        val peerNick = nickOf(peerAddr)
        removePeer(peerAddr)
        announceLeave(peerAddr, peerNick)
    }

    fun nickOf(peerAddr: String): String = synchronized(lock) { peers[peerAddr]?.nick.orEmpty() }

    private fun isSelf(candidate: String): Boolean =
        candidate == addr || normalizeAddr(candidate) == normalizeAddr(addr)

    /** Renders a version's origin as something readable. */
    fun writerName(id: String): String {
        if (id.isEmpty()) return "?"
        if (id == nodeId) return "you"
        val peerAddr = synchronized(lock) { idToAddr[id] } ?: return id.take(8)
        return nickOf(peerAddr).ifEmpty { peerAddr }
    }

    // ---- local operations ----------------------------------------------

    fun set(key: String, value: String, ttlSeconds: Long = 0): KVUpdate? =
        write(key, value, ttlSeconds, expect = null)

    /**
     * Writes only if the key still holds [expect]; a zero version requires the key
     * to be absent. This is the primitive a lock or a leader election is built on.
     */
    fun compareAndSet(key: String, value: String, ttlSeconds: Long, expect: Version): KVUpdate? =
        write(key, value, ttlSeconds, expect)

    private fun write(key: String, value: String, ttlSeconds: Long, expect: Version?): KVUpdate? {
        val expiresAt = if (ttlSeconds > 0) System.currentTimeMillis() / 1000 + ttlSeconds else 0
        val update = store.write(key, value, WriteOptions(expiresAt, expect))
        if (update == null) {
            metrics.kvCasFailures.incrementAndGet()
            return null
        }
        metrics.kvLocalWrites.incrementAndGet()
        broadcast(Kind.KV, update)
        publishEntries()
        return update
    }

    fun delete(key: String): KVUpdate? = remove(key, null)

    fun compareAndDelete(key: String, expect: Version): KVUpdate? = remove(key, expect)

    private fun remove(key: String, expect: Version?): KVUpdate? {
        val update = store.remove(key, WriteOptions(expect = expect))
        if (update == null) {
            metrics.kvCasFailures.incrementAndGet()
            return null
        }
        metrics.kvLocalWrites.incrementAndGet()
        broadcast(Kind.KV, update)
        publishEntries()
        return update
    }

    fun export(): String = WireJson.encodeToString(
        ExportFile(
            node = addr,
            cluster = config.cluster,
            exported_at = System.currentTimeMillis() / 1000,
            entries = store.updates(),
        )
    )

    /**
     * Loads an export. Preserving the exported versions merges under the usual
     * last-write-wins rule — a restore; re-stamping every entry as a fresh local
     * write forces it to win — a seed.
     */
    fun import(data: String, asLocalWrites: Boolean): Int {
        val file = WireJson.decodeFromString<ExportFile>(data)
        var applied = 0
        for (u in file.entries) {
            if (asLocalWrites) {
                if (u.deleted) {
                    delete(u.key)
                } else {
                    val ttl = if (u.expiresAt > 0) u.expiresAt - System.currentTimeMillis() / 1000 else 0
                    if (u.expiresAt > 0 && ttl <= 0) continue
                    set(u.key, u.value, ttl)
                }
                applied++
            } else if (store.apply(u)) {
                applied++
                broadcast(Kind.KV, u)
            }
        }
        if (applied > 0) {
            logActivity("imported $applied entries")
            publishEntries()
        }
        return applied
    }

    // ---- chat -----------------------------------------------------------

    fun sendChat(text: String) = sendChatMessage(text, action = false)

    fun sendAction(text: String) = sendChatMessage(text, action = true)

    private fun sendChatMessage(text: String, action: Boolean) {
        val ts = System.currentTimeMillis() / 1000
        appendChat(
            ChatEntry(
                ts = ts,
                sender = addr,
                nick = nick,
                text = text,
                kind = if (action) ChatKind.ACTION else ChatKind.MESSAGE,
            )
        )
        broadcast(Kind.CHAT, ChatMessage(sender = addr, nick = nick, text = text, ts = ts, action = action))
    }

    fun sendDirect(target: String, text: String): Boolean {
        val peerAddr = resolvePeer(target) ?: return false
        val ts = System.currentTimeMillis() / 1000
        send(peerAddr, Kind.DIRECT_MESSAGE, ChatMessage(sender = addr, nick = nick, text = text, ts = ts, to = peerAddr))
        appendChat(ChatEntry(ts = ts, sender = addr, nick = nick, text = text, kind = ChatKind.DIRECT, to = peerAddr))
        return true
    }

    private fun resolvePeer(target: String): String? = synchronized(lock) {
        peers[target]?.addr ?: peers.values.firstOrNull { it.nick.equals(target, ignoreCase = true) }?.addr
    }

    fun renameSelf(newNick: String) {
        val previous = nick
        nick = newNick
        config = config.copy(nick = newNick)
        system("$previous is now known as $newNick")
        helloAllPeers()
        publishStatus()
        publishTopology()
    }

    /** Handles one line of input, slash commands included — the same vocabulary as the terminal client. */
    fun submit(input: String) {
        val text = input.trim()
        if (text.isEmpty()) return
        if (!text.startsWith("/")) {
            sendChat(text)
            return
        }
        val (cmd, rest) = split2(text.substring(1))
        when (cmd.lowercase()) {
            "help", "?" -> system(
                "commands: /nick <name>  /me <text>  /msg <peer> <text>  /peers  " +
                    "/keys  /get <key>  /set <key> <value>  /setttl <key> <seconds> <value>  /del <key>"
            )
            "nick" -> if (rest.isEmpty()) system("usage: /nick <name>") else renameSelf(rest)
            "me" -> if (rest.isEmpty()) system("usage: /me <text>") else sendAction(rest)
            "msg", "dm" -> {
                val (target, body) = split2(rest)
                when {
                    target.isEmpty() || body.isEmpty() -> system("usage: /msg <peer> <text>")
                    !sendDirect(target, body) -> system("no such peer: $target")
                }
            }
            "peers" -> {
                val known = peerAddrs().sorted()
                if (known.isEmpty()) system("no peers known")
                else system("peers: " + known.joinToString(", ") { nickOf(it).ifEmpty { it } })
            }
            "keys" -> {
                val keys = store.entries().map { it.key }
                if (keys.isEmpty()) system("store is empty") else system("keys: " + keys.joinToString(", "))
            }
            "get" -> {
                if (rest.isEmpty()) system("usage: /get <key>")
                else store[rest]?.let { system("$rest = $it") } ?: system("$rest is not set")
            }
            "set" -> {
                val (key, value) = split2(rest)
                if (key.isEmpty()) system("usage: /set <key> <value>") else {
                    set(key, value)
                    system("set $key")
                }
            }
            "setttl" -> {
                val (key, remainder) = split2(rest)
                val (secs, value) = split2(remainder)
                val ttl = secs.toLongOrNull() ?: 0
                if (key.isEmpty() || ttl <= 0) system("usage: /setttl <key> <seconds> <value>") else {
                    set(key, value, ttl)
                    system("set $key (expires in ${ttl}s)")
                }
            }
            "del", "delete" -> {
                if (rest.isEmpty()) system("usage: /del <key>") else {
                    delete(rest)
                    system("deleted $rest")
                }
            }
            else -> system("unknown command: /$cmd (try /help)")
        }
    }

    fun system(text: String) =
        appendChat(ChatEntry(ts = System.currentTimeMillis() / 1000, text = text, kind = ChatKind.SYSTEM))

    private fun appendChat(entry: ChatEntry) {
        synchronized(lock) {
            chatLog.add(entry)
            while (chatLog.size > CHAT_RING) chatLog.removeAt(0)
        }
        persist()
        publishChat()
    }

    private fun logActivity(text: String) {
        val stamp = java.text.SimpleDateFormat("HH:mm:ss", java.util.Locale.US)
            .format(java.util.Date())
        synchronized(lock) {
            activityLog.add("$stamp $text")
            while (activityLog.size > ACTIVITY_RING) activityLog.removeAt(0)
        }
        _activity.value = synchronized(lock) { activityLog.toList() }
    }

    private fun announceJoin(peerAddr: String, peerNick: String) =
        system(if (peerNick.isEmpty()) "$peerAddr joined" else "$peerNick ($peerAddr) joined")

    private fun announceLeave(peerAddr: String, peerNick: String) =
        system(if (peerNick.isEmpty()) "$peerAddr left" else "$peerNick ($peerAddr) left")

    // ---- inbound --------------------------------------------------------

    private fun handlePacket(packet: Packet) {
        val source = normalizeAddr(packet.from)
        val frame = try {
            codec.decode(packet.data)
        } catch (e: DecodeException) {
            when (e.reason) {
                DecodeError.BAD_MAC -> metrics.authFailures
                DecodeError.REPLAY -> metrics.replayDrops
                DecodeError.CLOCK_SKEW -> metrics.skewDrops
                else -> metrics.malformedDrops
            }.incrementAndGet()
            // Attributed to the socket source: a rejected frame is exactly the case
            // where the sender's claim about who it is cannot be trusted.
            metrics.rejectedFrom(source)
            return
        }
        metrics.received(frame.kind, packet.data.size)
        metrics.receivedFrom(source, packet.data.size)
        dispatch(frame.kind, String(frame.body))
    }

    private fun dispatch(kind: Byte, body: String) {
        try {
            when (kind) {
                Kind.KV -> handleKv(WireJson.decodeFromString<KVUpdate>(body))
                Kind.KV_BATCH -> {
                    val batch = WireJson.decodeFromString<KVBatch>(body)
                    var changed = false
                    for (u in batch.updates) if (applyRemote(u)) changed = true
                    if (changed) publishEntries()
                }
                Kind.CHAT -> handleChat(WireJson.decodeFromString<ChatMessage>(body))
                Kind.DIRECT_MESSAGE -> handleDirect(WireJson.decodeFromString<ChatMessage>(body))
                Kind.STATE_REQUEST -> handleStateRequest(WireJson.decodeFromString<StateRequest>(body))
                Kind.STATE_RESPONSE -> mergeState(WireJson.decodeFromString<StateResponse>(body))
                Kind.PEER_GOSSIP -> handleGossip(WireJson.decodeFromString<PeerGossip>(body))
                Kind.HELLO -> handleHello(WireJson.decodeFromString<Hello>(body))
                Kind.GOODBYE -> handleGoodbye(WireJson.decodeFromString<Goodbye>(body))
                Kind.DIGEST -> handleDigest(WireJson.decodeFromString<Digest>(body))
                Kind.PULL_REQUEST -> handlePull(WireJson.decodeFromString<PullRequest>(body))
                Kind.FINGERPRINT -> handleFingerprint(WireJson.decodeFromString<Fingerprint>(body))
                // The checker reads its answers off the stream it asked over, so a
                // datagram reply needs accepting but not correlating.
                Kind.FINGERPRINT_REPLY ->
                    touchPeer(WireJson.decodeFromString<FingerprintReply>(body).from)
                Kind.BOOTSTRAP_ROSTER -> applyRoster(WireJson.decodeFromString<BootstrapRoster>(body))
                else -> metrics.malformedDrops.incrementAndGet()
            }
        } catch (e: Exception) {
            metrics.malformedDrops.incrementAndGet()
        }
    }

    /**
     * Merges one remote update, counting and narrating the outcome. A rejected
     * update is not an error — it is a conflict last-write-wins resolved — but it
     * is invisible without the activity feed, which is when replication bugs hide.
     */
    private fun applyRemote(u: KVUpdate): Boolean {
        if (store.apply(u)) {
            metrics.kvApplied.incrementAndGet()
            val who = writerName(u.version.node)
            if (u.deleted) logActivity("$who deleted ${u.key} (v${u.version.counter})")
            else logActivity("$who set ${u.key} = ${preview(u.value)} (v${u.version.counter})")
            return true
        }
        metrics.kvRejectedStale.incrementAndGet()
        logActivity("ignored stale ${u.action} for ${u.key} (v${u.version.counter} from ${writerName(u.version.node)})")
        return false
    }

    private fun handleKv(u: KVUpdate) {
        if (applyRemote(u)) publishEntries()
    }

    private fun handleChat(m: ChatMessage) {
        touchPeer(m.sender)
        if (m.nick.isNotEmpty()) setNick(m.sender, m.nick)
        appendChat(
            ChatEntry(
                ts = m.ts,
                sender = m.sender,
                nick = m.nick,
                text = m.text,
                kind = if (m.action) ChatKind.ACTION else ChatKind.MESSAGE,
            )
        )
        if (addPeer(m.sender)) {
            announceJoin(m.sender, m.nick)
            send(m.sender, Kind.HELLO, helloMessage())
        }
    }

    private fun handleDirect(m: ChatMessage) {
        touchPeer(m.sender)
        if (m.nick.isNotEmpty()) setNick(m.sender, m.nick)
        appendChat(
            ChatEntry(ts = m.ts, sender = m.sender, nick = m.nick, text = m.text, kind = ChatKind.DIRECT, to = addr)
        )
    }

    private fun handleStateRequest(req: StateRequest) {
        touchPeer(req.from)
        metrics.stateSyncOut.incrementAndGet()
        send(req.from, Kind.STATE_RESPONSE, stateResponse(DATAGRAM_SYNC_LIMIT))
    }

    private fun mergeState(resp: StateResponse) {
        metrics.stateSyncIn.incrementAndGet()
        var changed = false
        for (u in resp.kv) {
            if (store.apply(u)) {
                metrics.kvApplied.incrementAndGet()
                changed = true
            }
        }
        if (resp.chat.isNotEmpty()) {
            // A snapshot carries history this node may already hold. Appending it
            // wholesale duplicated every line on the second sync, and a joiner that
            // syncs from two peers saw the same conversation two or three times.
            val grew = synchronized(lock) {
                val before = chatLog.size
                val merged = LinkedHashSet(chatLog)
                merged.addAll(resp.chat)
                if (merged.size == before) return@synchronized false
                val ordered = merged.sortedBy { it.ts }
                chatLog.clear()
                chatLog.addAll(ordered.takeLast(CHAT_RING))
                true
            }
            if (grew) {
                persist()
                publishChat()
            }
        }
        if (changed) {
            logActivity("state sync merged ${resp.kv.size} entries")
            publishEntries()
        }
    }

    private fun handleGossip(g: PeerGossip) {
        touchPeer(g.from)
        if (g.nick.isNotEmpty()) setNick(g.from, g.nick)
        if (g.id.isNotEmpty()) synchronized(lock) { idToAddr[g.id] = g.from }
        recordView(g.from, g.peers)
        if (addPeer(g.from)) {
            announceJoin(g.from, g.nick)
            send(g.from, Kind.HELLO, helloMessage())
        }
        for (p in g.peers) {
            if (addPeer(p)) {
                announceJoin(p, nickOf(p))
                send(p, Kind.HELLO, helloMessage())
            }
        }
    }

    private fun handleHello(h: Hello) {
        touchPeer(h.from)
        if (h.nick.isNotEmpty()) setNick(h.from, h.nick)
        if (h.id.isNotEmpty()) synchronized(lock) { idToAddr[h.id] = h.from }
        if (addPeer(h.from)) announceJoin(h.from, h.nick)
    }

    /** Keeps the sender's advertised peer list, which is what makes a cluster-wide graph possible. */
    private fun recordView(from: String, advertised: List<String>) {
        if (from.isEmpty()) return
        val owner = normalizeAddr(from)
        if (owner == normalizeAddr(addr)) return
        synchronized(lock) {
            gossipViews[owner] = GossipView(
                peers = advertised.map { normalizeAddr(it) }.filter { it.isNotEmpty() }.distinct(),
                atMs = System.currentTimeMillis(),
            )
        }
        publishTopology()
    }

    private fun handleGoodbye(g: Goodbye) {
        val known = synchronized(lock) { peers.containsKey(g.from) }
        if (!known) return
        val peerNick = g.nick.ifEmpty { nickOf(g.from) }
        removePeer(g.from)
        announceLeave(g.from, peerNick)
    }

    // ---- anti-entropy ---------------------------------------------------

    /**
     * Advertises a slice of the local keyspace to one peer.
     *
     * This closes the last correctness gap in the replication model: a dropped KV
     * datagram is never re-sent by the write path, so two stores stay divergent
     * until the next overlapping write. The digest carries versions, not values,
     * so a round is cheap.
     */
    fun antiEntropyRound(target: String) {
        if (target.isEmpty()) return
        val d = synchronized(lock) {
            val digest = store.digest(aeCursors[target] ?: "", config.effectiveDigestBatch)
            when {
                digest.hi.isEmpty() -> aeCursors.remove(target) // covered the tail; start over
                digest.entries.isNotEmpty() -> aeCursors[target] = digest.entries.last().key
            }
            digest
        }
        metrics.aeRounds.incrementAndGet()
        send(target, Kind.DIGEST, d.copy(from = addr))
    }

    private fun handleDigest(d: Digest) {
        touchPeer(d.from)
        val (push, pull) = store.reconcile(d, config.maxPush, config.maxPull)
        if (push.isNotEmpty()) {
            metrics.aePushed.addAndGet(push.size.toLong())
            logActivity("anti-entropy: pushing ${push.size} entries to ${nickOf(d.from).ifEmpty { d.from }}")
            sendUpdates(d.from, push)
        }
        if (pull.isNotEmpty()) {
            metrics.aePulled.addAndGet(pull.size.toLong())
            logActivity("anti-entropy: pulling ${pull.size} entries from ${nickOf(d.from).ifEmpty { d.from }}")
            send(d.from, Kind.PULL_REQUEST, PullRequest(from = addr, keys = pull))
        }
    }

    private fun handlePull(req: PullRequest) {
        touchPeer(req.from)
        val updates = store.updatesFor(req.keys)
        if (updates.isNotEmpty()) sendUpdates(req.from, updates)
    }

    /** Ships updates as batches, split on a byte budget so a repair of large values still fits a datagram. */
    private fun sendUpdates(target: String, updates: List<KVUpdate>) {
        var batch = mutableListOf<KVUpdate>()
        var size = 0
        fun flush() {
            if (batch.isEmpty()) return
            send(target, Kind.KV_BATCH, KVBatch(batch))
            batch = mutableListOf()
            size = 0
        }
        for (u in updates) {
            val entrySize = u.key.length + u.value.length + 160
            if (batch.isNotEmpty() && (size + entrySize > BATCH_BYTES || batch.size >= BATCH_ENTRIES)) flush()
            batch.add(u)
            size += entrySize
        }
        flush()
    }

    // ---- state sync & bootstrap ----------------------------------------

    private fun stateResponse(limit: Int): StateResponse {
        val updates = store.updates().let { if (limit > 0 && it.size > limit) it.take(limit) else it }
        // Direct messages are stripped: handing a joining node someone else's
        // private conversation because it asked for history would be a quiet leak.
        val history = synchronized(lock) {
            chatLog.filter { it.kind != ChatKind.DIRECT }.takeLast(STATE_SYNC_CHAT_LINES)
        }
        return StateResponse(kv = updates, chat = history)
    }

    private fun serveStream(conn: Socket) {
        val frame = try {
            codec.decode(readFrame(conn.getInputStream()))
        } catch (e: Exception) {
            metrics.streamErrors.incrementAndGet()
            return
        }
        metrics.received(frame.kind, frame.body.size)
        if (frame.kind == Kind.STATE_REQUEST) {
            val req = WireJson.decodeFromString<StateRequest>(String(frame.body))
            touchPeer(req.from)
            metrics.stateSyncOut.incrementAndGet()
            // A stream has no datagram ceiling: send the whole store.
            val pkt = codec.encode(Kind.STATE_RESPONSE, stateResponse(0))
            writeFrame(conn.getOutputStream(), pkt)
            metrics.sent(Kind.STATE_RESPONSE, pkt.size)
        } else if (frame.kind == Kind.FINGERPRINT) {
            val req = WireJson.decodeFromString<Fingerprint>(String(frame.body))
            touchPeer(req.from)
            val pkt = codec.encode(Kind.FINGERPRINT_REPLY, fingerprintReply())
            writeFrame(conn.getOutputStream(), pkt)
            metrics.sent(Kind.FINGERPRINT_REPLY, pkt.size)
        } else {
            dispatch(frame.kind, String(frame.body))
        }
    }

    /**
     * One peer's answer to "what do you hold?".
     *
     * [reachable] is false when the peer never answered; a silent peer says
     * nothing about whether it agrees, so it must not be counted as if it did.
     */
    data class PeerConsistency(
        val addr: String,
        val nick: String = "",
        val reachable: Boolean = false,
        val error: String = "",
        val keys: Int = 0,
        val tombstones: Int = 0,
        val clock: Long = 0,
        val agrees: Boolean = false,
        val differingBuckets: List<Int> = emptyList(),
    )

    /**
     * The answer to the question anti-entropy never asks out loud: are the
     * replicas actually the same right now?
     */
    data class ConsistencyReport(
        val addr: String,
        val keys: Int,
        val tombstones: Int,
        val clock: Long,
        val checkedAtMs: Long,
        val peers: List<PeerConsistency>,
        val converged: Boolean,
        val unreachable: Int,
    )

    /**
     * Asks every peer to summarise its store and compares the answers with this
     * node's.
     *
     * Over streams, not datagrams: a dropped answer would read as a peer that
     * disagrees, which is exactly the wrong conclusion to draw from packet loss.
     * Blocking, and it dials every peer — callers must be off the main thread.
     */
    fun checkConsistency(): ConsistencyReport {
        val local = store.fingerprint()
        val results = peerAddrs().map { peerAddr -> fingerprintPeer(peerAddr, local) }.sortedBy { it.addr }
        return ConsistencyReport(
            addr = addr,
            keys = local.keys,
            tombstones = local.tombstones,
            clock = local.clock,
            checkedAtMs = System.currentTimeMillis(),
            peers = results,
            converged = results.none { it.reachable && !it.agrees },
            unreachable = results.count { !it.reachable },
        )
    }

    private fun fingerprintPeer(peerAddr: String, local: FingerprintReply): PeerConsistency {
        val reply = requestFingerprint(peerAddr)
            ?: return PeerConsistency(addr = peerAddr, nick = nickOf(peerAddr), error = "no answer")
        if (reply.buckets.size != local.buckets.size) {
            // A peer summarising at a different granularity cannot be compared
            // bucket by bucket; say so rather than reporting a false divergence.
            return PeerConsistency(
                addr = peerAddr,
                nick = nickOf(peerAddr),
                error = "peer reported ${reply.buckets.size} buckets, this node uses ${local.buckets.size}",
            )
        }
        val differing = local.buckets.indices.filter { local.buckets[it] != reply.buckets[it] }
        return PeerConsistency(
            addr = peerAddr,
            nick = reply.nick.ifEmpty { nickOf(peerAddr) },
            reachable = true,
            keys = reply.keys,
            tombstones = reply.tombstones,
            clock = reply.clock,
            agrees = differing.isEmpty(),
            differingBuckets = differing,
        )
    }

    /**
     * One framed round trip that hands the answer back, rather than dispatching
     * it like [exchangeStream] does. A consistency check needs the reply, not a
     * side effect.
     */
    private fun requestFingerprint(peer: String): FingerprintReply? {
        val tr = synchronized(lock) { transport } ?: return null
        val conn = tr.dial(peer) ?: return null
        return try {
            val pkt = codec.encode(Kind.FINGERPRINT, Fingerprint(from = addr))
            writeFrame(conn.getOutputStream(), pkt)
            metrics.sent(Kind.FINGERPRINT, pkt.size)
            val frame = codec.decode(readFrame(conn.getInputStream()))
            metrics.received(frame.kind, frame.body.size)
            if (frame.kind != Kind.FINGERPRINT_REPLY) null
            else WireJson.decodeFromString<FingerprintReply>(String(frame.body))
        } catch (e: Exception) {
            metrics.streamErrors.incrementAndGet()
            null
        } finally {
            try {
                conn.close()
            } catch (_: Exception) {
            }
        }
    }

    /**
     * How far the anti-entropy sweep has walked, as one line.
     *
     * There is a cursor per peer now, so a single value would name whichever one
     * happened to be read. What matters to a reader is how many peers are
     * part-way through a sweep and where the furthest has reached.
     */
    private fun sweepSummary(): String = synchronized(lock) {
        val known = peers.size
        if (aeCursors.isEmpty()) return "all $known peer(s) at the start of the keyspace"
        val furthest = aeCursors.values.maxOrNull() ?: ""
        "${aeCursors.size} of $known peer(s) mid-sweep, furthest past \"$furthest\""
    }

    /** This node's store summary, labelled so a report can name who produced it. */
    fun fingerprintReply(): FingerprintReply =
        store.fingerprint().copy(from = addr, nick = nick)

    /** Answers a summary request that arrived as a datagram. */
    private fun handleFingerprint(req: Fingerprint) {
        if (req.from.isEmpty()) return
        touchPeer(req.from)
        send(req.from, Kind.FINGERPRINT_REPLY, fingerprintReply())
    }

    /** One framed request/response round trip. Returns false when the stream path is unavailable. */
    private inline fun <reified T> exchangeStream(peer: String, kind: Byte, value: T): Boolean {
        val tr = synchronized(lock) { transport } ?: return false
        val conn = tr.dial(peer) ?: return false
        return try {
            val pkt = codec.encode(kind, value)
            writeFrame(conn.getOutputStream(), pkt)
            metrics.sent(kind, pkt.size)
            val frame = codec.decode(readFrame(conn.getInputStream()))
            metrics.received(frame.kind, frame.body.size)
            dispatch(frame.kind, String(frame.body))
            true
        } catch (e: Exception) {
            metrics.streamErrors.incrementAndGet()
            false
        } finally {
            try {
                conn.close()
            } catch (_: Exception) {
            }
        }
    }

    fun requestStateFrom(peer: String) {
        val req = StateRequest(from = addr)
        if (!exchangeStream(peer, Kind.STATE_REQUEST, req)) send(peer, Kind.STATE_REQUEST, req)
    }

    private fun join() {
        registerWithBootstrap()
        discoverFromBootstrap()
        helloAllPeers()
        peerAddrs().randomOrNull()?.let { requestStateFrom(it) }
    }

    private fun registerWithBootstrap() {
        val reg = BootstrapRegister(from = addr, nick = nick)
        for (seed in config.seeds) if (seed.isNotEmpty()) send(seed, Kind.BOOTSTRAP_REGISTER, reg)
    }

    /**
     * Asks the seeds for the roster: the stream path first (an unbounded roster),
     * then datagrams, whose reply arrives asynchronously — so an unreachable
     * rendezvous service can never stall startup.
     */
    private fun discoverFromBootstrap() {
        val req = BootstrapDiscover(from = addr)
        for (seed in config.seeds) {
            if (seed.isEmpty()) continue
            if (exchangeStream(seed, Kind.BOOTSTRAP_DISCOVER, req)) return
        }
        for (seed in config.seeds) if (seed.isNotEmpty()) send(seed, Kind.BOOTSTRAP_DISCOVER, req)
    }

    private fun applyRoster(roster: BootstrapRoster) {
        var learned = false
        for (p in roster.peers) {
            if (addPeer(p.addr)) learned = true
            if (p.nick.isNotEmpty()) setNick(p.addr, p.nick)
        }
        if (!learned) return
        helloAllPeers()
        peerAddrs().randomOrNull()?.let { requestStateFrom(it) }
    }

    // ---- loops ----------------------------------------------------------

    private suspend fun gossipLoop() {
        while (scope.isActive) {
            delay(config.gossipIntervalMs)
            val known = peerAddrs()
            if (known.isEmpty()) continue
            gossipTo(known.random())
        }
    }

    /**
     * One gossip round to a single peer: our membership view, then a digest.
     *
     * Reconciliation is piggybacked on the same tick — gossip already picked a
     * random peer, and the digest is what makes convergence a mechanism rather
     * than a hope.
     */
    fun gossipTo(target: String) {
        if (target.isEmpty()) return
        send(target, Kind.PEER_GOSSIP, PeerGossip(from = addr, nick = nick, id = nodeId, peers = peerAddrs()))
        antiEntropyRound(target)
    }

    private suspend fun heartbeatLoop() {
        while (scope.isActive) {
            delay(config.heartbeatIntervalMs)
            registerWithBootstrap()
            if (peerAddrs().isEmpty()) discoverFromBootstrap()
            helloAllPeers()
            publishStatus()
        }
    }

    private suspend fun evictLoop() {
        while (scope.isActive) {
            delay(config.heartbeatIntervalMs)
            val now = System.currentTimeMillis()
            val stale = synchronized(lock) {
                peers.values.filter { now - it.lastSeenMs > config.evictThresholdMs }
            }
            for (peer in stale) {
                removePeer(peer.addr)
                announceLeave(peer.addr, peer.nick)
            }
            pruneViews(now)
        }
    }

    private fun pruneViews(nowMs: Long) {
        val ttl = maxOf(60_000L, config.gossipIntervalMs * 6)
        val dropped = synchronized(lock) {
            val old = gossipViews.filterValues { nowMs - it.atMs > ttl }.keys
            old.forEach { gossipViews.remove(it) }
            old.isNotEmpty()
        }
        if (dropped) publishTopology()
    }

    private suspend fun sweepLoop() {
        while (scope.isActive) {
            delay(config.sweepIntervalMs)
            val expired = store.sweepExpired()
            if (expired > 0) {
                metrics.kvExpired.addAndGet(expired.toLong())
                logActivity("$expired key(s) expired")
                publishEntries()
            }
            if (config.tombstoneTtlSec > 0) {
                val gced = store.gcTombstones(config.tombstoneTtlSec)
                if (gced > 0) {
                    metrics.kvGced.addAndGet(gced.toLong())
                    logActivity("$gced tombstone(s) reclaimed")
                }
            }
            metrics.sample(System.currentTimeMillis())
            publishMetrics()
        }
    }

    // ---- persistence & publishing --------------------------------------

    private fun persist() {
        val p = persister ?: return
        val gen: Long
        val state: PersistState
        synchronized(lock) {
            persistGen++
            gen = persistGen
            val (clock, entries) = store.snapshotState()
            state = PersistState(nodeId = nodeId, clock = clock, entries = entries, chat = chatLog.toList())
        }
        p.save(gen, state)
    }

    private fun publishAll() {
        publishEntries()
        publishPeers()
        publishChat()
        publishStatus()
        publishMetrics()
    }

    private fun publishEntries() {
        _entries.value = store.entries()
        publishStatus()
    }

    private fun publishPeers() {
        _peerList.value = synchronized(lock) { peers.values.sortedBy { it.addr } }
        publishStatus()
        publishTopology()
    }

    private fun publishTopology() {
        val (self, view) = synchronized(lock) { peers.values.toList() to HashMap(gossipViews) }
        _topology.value = TopologyBuilder.build(
            selfAddr = addr,
            selfNick = nick,
            peers = self,
            views = view,
            nowMs = System.currentTimeMillis(),
        )
    }

    private fun publishChat() {
        _chat.value = synchronized(lock) { chatLog.toList() }
    }

    private fun publishMetrics() {
        _metricsFlow.value = metrics.snapshot()
    }

    private fun publishStatus() {
        _status.value = NodeStatus(
            addr = addr,
            nick = nick,
            nodeId = nodeId,
            cluster = config.cluster,
            peers = synchronized(lock) { peers.size },
            keys = store.size(),
            tombstones = store.tombstones(),
            uptimeSec = if (startedAtMs == 0L) 0 else (System.currentTimeMillis() - startedAtMs) / 1000,
            running = isRunning,
        )
    }

    fun history(key: String): List<HistoryEntry> = store.history(key)

    /**
     * Everything this node knows about itself, in one snapshot.
     *
     * Collected here rather than read field by field from the UI so a report is
     * internally consistent — peers, counters and topology all describe the same
     * instant.
     */
    fun diagnostics(): NodeDiagnostics {
        val tr = synchronized(lock) { transport }
        val snapshot = metrics.snapshot()
        val traffic = metrics.addrs().associateBy { it.addr }
        val selfAddr = normalizeAddr(addr)
        val peerRows: List<PeerDiagnostics>
        val known: Set<String>
        synchronized(lock) {
            peerRows = peers.values.sortedBy { it.addr }.map { peer ->
                val key = normalizeAddr(peer.addr)
                val t = traffic[key]
                val view = gossipViews[key]
                PeerDiagnostics(
                    addr = peer.addr,
                    nick = peer.nick,
                    firstSeenMs = peer.firstSeenMs,
                    lastSeenMs = peer.lastSeenMs,
                    packetsOut = t?.packetsOut ?: 0,
                    packetsIn = t?.packetsIn ?: 0,
                    bytesOut = t?.bytesOut ?: 0,
                    bytesIn = t?.bytesIn ?: 0,
                    rejected = t?.rejected ?: 0,
                    sendErrors = t?.sendErrors ?: 0,
                    advertisedPeers = view?.peers?.size ?: -1,
                    lastGossipMs = view?.atMs ?: 0,
                )
            }
            known = peers.keys.map { normalizeAddr(it) }.toSet()
        }
        // An address that sends us packets while being no peer of ours is worth
        // showing: it is what a NAT, a stale peer or a wrong advertise host looks
        // like from this side.
        val strangers = traffic.values
            .filter { it.addr != selfAddr && it.addr !in known && (it.packetsIn > 0 || it.rejected > 0) }
            .map {
                PeerDiagnostics(
                    addr = it.addr,
                    known = false,
                    packetsIn = it.packetsIn,
                    packetsOut = it.packetsOut,
                    bytesIn = it.bytesIn,
                    bytesOut = it.bytesOut,
                    rejected = it.rejected,
                    sendErrors = it.sendErrors,
                )
            }
            .sortedBy { it.addr }

        return NodeDiagnostics(
            addr = addr,
            nick = nick,
            nodeId = nodeId,
            cluster = config.cluster,
            keyFingerprint = Codec.keyFingerprint(config.psk, config.cluster),
            pskSet = config.psk.isNotEmpty(),
            configuredPort = config.port,
            boundPort = tr?.port ?: 0,
            streamListener = tr?.streamsAvailable ?: false,
            advertiseHost = config.advertiseHost,
            localIpv4 = localIpv4(),
            running = tr != null,
            startedAtMs = startedAtMs,
            uptimeSec = if (startedAtMs == 0L) 0 else (System.currentTimeMillis() - startedAtMs) / 1000,
            gossipIntervalMs = config.gossipIntervalMs,
            heartbeatIntervalMs = config.heartbeatIntervalMs,
            evictThresholdMs = config.evictThresholdMs,
            sweepIntervalMs = config.sweepIntervalMs,
            tombstoneTtlSec = config.tombstoneTtlSec,
            seeds = config.seeds,
            peers = peerRows,
            strangers = strangers,
            store = store.stats().copy(digestCursor = sweepSummary()),
            metrics = snapshot,
            topology = _topology.value,
            chatLines = synchronized(lock) { chatLog.size },
            activityLines = synchronized(lock) { activityLog.size },
        )
    }

    companion object {
        fun validPeerAddr(addr: String): Boolean {
            val i = addr.lastIndexOf(':')
            if (i < 0) return false
            val host = addr.substring(0, i)
            val port = addr.substring(i + 1).toIntOrNull() ?: return false
            if (port !in 1..65535) return false
            // An empty host is allowed: it is what a node bound to the wildcard
            // address advertises, and it resolves to the loopback interface.
            return host.none { it.isWhitespace() }
        }

        /** Canonicalises an address so a node recognises the aliases of its own. */
        fun normalizeAddr(addr: String): String {
            val i = addr.lastIndexOf(':')
            if (i < 0) return addr
            val host = addr.substring(0, i)
            val port = addr.substring(i + 1)
            val canonical = when (host) {
                "", "0.0.0.0", "localhost", "::", "[::]" -> "127.0.0.1"
                else -> host
            }
            return "$canonical:$port"
        }

        /** The first non-loopback IPv4 address, which is what peers on the LAN can reach. */
        fun localIpv4(): String {
            return try {
                NetworkInterface.getNetworkInterfaces().toList()
                    .filter { it.isUp && !it.isLoopback }
                    .flatMap { it.inetAddresses.toList() }
                    .filterIsInstance<Inet4Address>()
                    .firstOrNull()
                    ?.hostAddress ?: "127.0.0.1"
            } catch (e: Exception) {
                "127.0.0.1"
            }
        }

        fun split2(s: String): Pair<String, String> {
            val trimmed = s.trim()
            val i = trimmed.indexOfFirst { it == ' ' || it == '\t' }
            return if (i < 0) trimmed to "" else trimmed.substring(0, i) to trimmed.substring(i + 1).trim()
        }

        fun preview(value: String): String = if (value.length > 40) value.take(37) + "..." else value
    }
}
