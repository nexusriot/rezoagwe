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
    val durability: String = "snapshot",
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
     *
     * A port outside 1..65535 is refused the same way, in favour of the value
     * the user already had. The field accepts digits, so "99999" is reachable
     * by one keystroke, and neither end of the range fails usefully: binding
     * 99999 throws and leaves the node stopped, while 0 binds an *ephemeral*
     * port and the node then advertises ":0" — an address no peer can dial. It
     * still gossips outward, so its own screen reports CONNECTED with a full
     * peer list while every other node has dropped it, and the ":0" it
     * registers is handed to each new joiner by bootstrap. Refusing the value
     * is the only answer that cannot be misread.
     */
    fun sanitized(fallback: Settings = Settings()): Settings = copy(
        durability = durability.takeIf { it in listOf("snapshot","journal","journal-sync") } ?: fallback.durability,
        nick = nick.trim().ifBlank { fallback.nick },
        port = port.takeIf { it in VALID_PORTS } ?: fallback.port,
        advertiseHost = advertiseHost.trim(),
        seeds = seeds.trim(),
        psk = psk.trim(),
        cluster = cluster.trim().ifBlank { fallback.cluster },
        bootstrapPort = bootstrapPort.takeIf { it in VALID_PORTS } ?: fallback.bootstrapPort,
    )

    /**
     * What [sanitized] would not keep, phrased for the person who typed it.
     *
     * Refusing a value is only half an answer. The settings form is seeded from
     * storage, so a refusal that changes nothing else leaves the refused text
     * sitting in the field with the node still on its old port: the user is
     * looking at a number that is not in effect and nothing says so. Worse, the
     * form only re-seeds when the stored settings actually change, so editing
     * *just* the bad field is exactly the case with no feedback at all.
     *
     * Trimming is deliberately not reported. It is documented on the screen, it
     * is what the user wanted, and naming it on every paste would bury the two
     * messages that matter.
     */
    fun refusals(fallback: Settings = Settings()): List<String> {
        val out = mutableListOf<String>()
        if (durability !in listOf("snapshot","journal","journal-sync")) out += "Durability must be snapshot, journal, or journal-sync."
        if (port !in VALID_PORTS) {
            out += "Node port $port is not a usable port; keeping ${fallback.port}."
        }
        if (bootstrapPort !in VALID_PORTS) {
            out += "Bootstrap port $bootstrapPort is not a usable port; keeping ${fallback.bootstrapPort}."
        }
        if (nick.isBlank()) out += "Nickname cannot be blank; keeping ${fallback.nick}."
        if (cluster.isBlank()) out += "Cluster name cannot be blank; keeping ${fallback.cluster}."
        return out
    }

    fun toNodeConfig() = NodeConfig(
        advertiseHost = advertiseHost,
        port = port,
        nick = nick,
        seeds = seedList(),
        psk = psk,
        cluster = cluster,
        tombstoneTtlSec = tombstoneTtlSec,
        durability = durability,
    )

    fun toBootstrapConfig() = BootstrapConfig(
        port = bootstrapPort,
        psk = psk,
        cluster = cluster,
    )

    companion object {
        /** What a socket can actually bind. Port 0 is excluded: see [sanitized]. */
        val VALID_PORTS = 1..65535
    }
}

private const val PREFS = "rezoagwe"

/**
 * Which roles the user last asked for, kept apart from [Settings] because it is
 * intent rather than configuration — nothing on the settings screen edits it.
 *
 * START_STICKY brings the service back after the system reclaims the process,
 * but the process comes back empty: a freshly built engine is stopped, the
 * watcher sees nothing running and stops the service again, and the node the
 * user started never returns. Observed on a tablet — the activity closed, the
 * service restarted, and twelve seconds later Android logged
 * "Stop FGS timeout" and froze the process, with the node gone from the
 * cluster and nothing on screen to say so. Remembering the intent is what
 * makes the sticky restart mean anything.
 */
private const val KEY_NODE_WANTED = "nodeWanted"
private const val KEY_BOOTSTRAP_WANTED = "bootstrapWanted"

object SettingsStore {
    /**
     * Preferences written by an older build — or by a build that did not yet
     * range-check the port — are repaired on the way in rather than trusted,
     * so a value that can never bind cannot outlive the version that stored it.
     */
    fun load(context: Context): Settings {
        val p = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val defaults = Settings()
        return Settings(
            durability = p.getString("durability", defaults.durability)!!,
            nick = p.getString("nick", defaults.nick)!!,
            port = p.getInt("port", defaults.port),
            advertiseHost = p.getString("advertiseHost", defaults.advertiseHost)!!,
            seeds = p.getString("seeds", defaults.seeds)!!,
            psk = p.getString("psk", defaults.psk)!!,
            cluster = p.getString("cluster", defaults.cluster)!!,
            bootstrapPort = p.getInt("bootstrapPort", defaults.bootstrapPort),
            tombstoneTtlSec = p.getLong("tombstoneTtlSec", defaults.tombstoneTtlSec),
        ).sanitized()
    }

    fun save(context: Context, s: Settings) {
        context.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit()
            .putString("durability", s.durability)
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

    @Synchronized fun init(context: Context) {
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
    @Synchronized fun applySettings(context: Context, updated: Settings) {
        init(context)
        val previous = _settings.value
        SettingsStore.save(context, updated)
        _settings.value = updated

        val engine = engineRef!!
        val nodeChanged = previous.durability != updated.durability || previous.port != updated.port ||
            previous.psk != updated.psk ||
            previous.cluster != updated.cluster ||
            previous.advertiseHost != updated.advertiseHost ||
            previous.seeds != updated.seeds ||
            previous.tombstoneTtlSec != updated.tombstoneTtlSec
        if (nodeChanged) {
            val wasRunning = engine.isRunning
            engine.stop()
            val health = engine.persistenceHealth()
            if (health.pending && health.error.isNotEmpty()) {
                SettingsStore.save(context, previous)
                _settings.value = previous
                _lastError.value = "Profile not switched: ${health.error}"
                return
            }
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
            if (previous.cluster != updated.cluster) {
                bootstrapRef = BootstrapServer(updated.toBootstrapConfig(), rosterFile(context), scope)
            } else bootstrap.config = updated.toBootstrapConfig()
            if (wasRunning) bootstrapRef!!.start()
        }
    }

    fun startNode(context: Context) = guard {
        wantRole(context, KEY_NODE_WANTED, true)
        engine(context).start()
    }

    fun stopNode(context: Context) = guard {
        wantRole(context, KEY_NODE_WANTED, false)
        engine(context).stop()
    }

    fun startBootstrap(context: Context) = guard {
        wantRole(context, KEY_BOOTSTRAP_WANTED, true)
        bootstrap(context).start()
    }

    fun stopBootstrap(context: Context) = guard {
        wantRole(context, KEY_BOOTSTRAP_WANTED, false)
        bootstrap(context).stop()
    }

    /** Whether the user has asked for a role that is not currently up. */
    fun anyRoleWanted(context: Context): Boolean {
        val p = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        return p.getBoolean(KEY_NODE_WANTED, false) || p.getBoolean(KEY_BOOTSTRAP_WANTED, false)
    }

    /**
     * Brings back the roles the user had running, after the system reclaimed
     * the process and START_STICKY rebuilt the service around an empty one.
     *
     * Binds sockets, so callers must be off the main thread.
     */
    fun restoreWantedRoles(context: Context) {
        init(context)
        val p = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        if (p.getBoolean(KEY_NODE_WANTED, false) && !engine(context).isRunning) startNode(context)
        if (p.getBoolean(KEY_BOOTSTRAP_WANTED, false) && !bootstrap(context).isRunning) startBootstrap(context)
    }

    private fun wantRole(context: Context, key: String, wanted: Boolean) {
        context.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().putBoolean(key, wanted).apply()
    }

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

    private fun stateFile(context: Context) = File(Profiles.initialize(context.filesDir, _settings.value.cluster), "node.json")

    private fun rosterFile(context: Context) = File(Profiles.initialize(context.filesDir, _settings.value.cluster), "bootstrap.json")
}
