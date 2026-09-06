package com.nexusriot.rezoagwe.core

import android.content.Context
import com.nexusriot.rezoagwe.proto.Codec
import java.io.File
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/** Everything the user can configure, persisted in shared preferences. */
data class Settings(
    val nick: String = "phone",
    val port: Int = 3137,
    val advertiseHost: String = "",
    val seeds: String = "",
    val psk: String = "",
    val cluster: String = Codec.DEFAULT_CLUSTER,
    val bootstrapPort: Int = 9999,
    val tombstoneTtlSec: Long = 0,
) {
    fun seedList(): List<String> =
        seeds.split(',').map { it.trim() }.filter { it.isNotEmpty() }

    /**
     * The values as they should be stored: surrounding whitespace off every text
     * field, and blanks replaced from [fallback].
     *
     * The cluster name and the key feed the framing key, so a stray space is not
     * cosmetic — it changes the key and the node stops talking to every peer,
     * with nothing on screen to show why. A soft keyboard supplies exactly that:
     * it appends a space after a word it thinks it has completed, so typing
     * "e2e" into the cluster field stores "e2e ". Trimming here rather than at
     * each call site is the point — the seeds and the advertised host were
     * already trimmed, and the two fields that mattered most were not.
     */
    fun sanitized(fallback: Settings = Settings()): Settings = copy(
        nick = nick.trim().ifBlank { fallback.nick },
        advertiseHost = advertiseHost.trim(),
        seeds = seeds.trim(),
        psk = psk.trim(),
        cluster = cluster.trim().ifBlank { fallback.cluster },
    )

    fun toNodeConfig() = NodeConfig(
        advertiseHost = advertiseHost,
        port = port,
        nick = nick,
        seeds = seedList(),
        psk = psk,
        cluster = cluster,
        tombstoneTtlSec = tombstoneTtlSec,
    )

    fun toBootstrapConfig() = BootstrapConfig(
        port = bootstrapPort,
        psk = psk,
        cluster = cluster,
    )
}

private const val PREFS = "rezoagwe"

object SettingsStore {
    fun load(context: Context): Settings {
        val p = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val defaults = Settings()
        return Settings(
            nick = p.getString("nick", defaults.nick)!!,
            port = p.getInt("port", defaults.port),
            advertiseHost = p.getString("advertiseHost", defaults.advertiseHost)!!,
            seeds = p.getString("seeds", defaults.seeds)!!,
            psk = p.getString("psk", defaults.psk)!!,
            cluster = p.getString("cluster", defaults.cluster)!!,
            bootstrapPort = p.getInt("bootstrapPort", defaults.bootstrapPort),
            tombstoneTtlSec = p.getLong("tombstoneTtlSec", defaults.tombstoneTtlSec),
        )
    }

    fun save(context: Context, s: Settings) {
        context.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit()
            .putString("nick", s.nick)
            .putInt("port", s.port)
            .putString("advertiseHost", s.advertiseHost)
            .putString("seeds", s.seeds)
            .putString("psk", s.psk)
            .putString("cluster", s.cluster)
            .putInt("bootstrapPort", s.bootstrapPort)
            .putLong("tombstoneTtlSec", s.tombstoneTtlSec)
            .apply()
    }
}

/**
 * Owns the node and the rendezvous service for the whole process.
 *
 * Both roles are singletons rather than per-screen objects: they hold sockets and
 * survive the UI, which is the entire point of running them behind a foreground
 * service.
 */
object Runtime {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    // The engine is a flow, not a field: changing the port or the key rebuilds it,
    // and the UI has to follow the new instance rather than keep rendering a
    // stopped one.
    private val _engine = MutableStateFlow<NodeEngine?>(null)
    val engineFlow: StateFlow<NodeEngine?> = _engine.asStateFlow()

    private val _bootstrap = MutableStateFlow<BootstrapServer?>(null)
    val bootstrapFlow: StateFlow<BootstrapServer?> = _bootstrap.asStateFlow()

    private var engineRef: NodeEngine?
        get() = _engine.value
        set(value) {
            _engine.value = value
        }

    private var bootstrapRef: BootstrapServer?
        get() = _bootstrap.value
        set(value) {
            _bootstrap.value = value
        }

    private val _settings = MutableStateFlow(Settings())
    val settings: StateFlow<Settings> = _settings.asStateFlow()

    private val _lastError = MutableStateFlow<String?>(null)
    val lastError: StateFlow<String?> = _lastError.asStateFlow()

    fun init(context: Context) {
        if (engineRef != null) return
        _settings.value = SettingsStore.load(context)
        engineRef = NodeEngine(_settings.value.toNodeConfig(), stateFile(context), scope)
        bootstrapRef = BootstrapServer(_settings.value.toBootstrapConfig(), rosterFile(context), scope)
    }

    fun engine(context: Context): NodeEngine {
        init(context)
        return engineRef!!
    }

    fun bootstrap(context: Context): BootstrapServer {
        init(context)
        return bootstrapRef!!
    }

    /**
     * Applies new settings, rebuilding whichever role they affect.
     *
     * The port, cluster and key are baked into a running socket and codec, so a
     * change means a restart — done here rather than silently ignored, which is
     * the failure mode that makes settings screens untrustworthy.
     */
    fun applySettings(context: Context, updated: Settings) {
        init(context)
        val previous = _settings.value
        SettingsStore.save(context, updated)
        _settings.value = updated

        val engine = engineRef!!
        val nodeChanged = previous.port != updated.port ||
            previous.psk != updated.psk ||
            previous.cluster != updated.cluster ||
            previous.advertiseHost != updated.advertiseHost ||
            previous.seeds != updated.seeds ||
            previous.tombstoneTtlSec != updated.tombstoneTtlSec
        if (nodeChanged) {
            val wasRunning = engine.isRunning
            engine.stop()
            engineRef = NodeEngine(updated.toNodeConfig(), stateFile(context), scope)
            if (wasRunning) startNode(context)
        } else if (previous.nick != updated.nick) {
            engine.renameSelf(updated.nick)
        }

        val bootstrap = bootstrapRef!!
        if (previous.bootstrapPort != updated.bootstrapPort ||
            previous.psk != updated.psk ||
            previous.cluster != updated.cluster
        ) {
            val wasRunning = bootstrap.isRunning
            bootstrap.stop()
            bootstrap.config = updated.toBootstrapConfig()
            if (wasRunning) bootstrap.start()
        }
    }

    fun startNode(context: Context) = guard { engine(context).start() }

    fun stopNode(context: Context) = guard { engine(context).stop() }

    fun startBootstrap(context: Context) = guard { bootstrap(context).start() }

    fun stopBootstrap(context: Context) = guard { bootstrap(context).stop() }

    fun clearError() {
        _lastError.value = null
    }

    /** Binding a port is the one operation a user can plausibly get wrong; surface it instead of crashing. */
    private inline fun guard(block: () -> Unit) {
        try {
            block()
        } catch (e: Exception) {
            _lastError.value = e.message ?: e.toString()
        }
    }

    private fun stateFile(context: Context) = File(context.filesDir, "node.json")

    private fun rosterFile(context: Context) = File(context.filesDir, "bootstrap.json")
}
