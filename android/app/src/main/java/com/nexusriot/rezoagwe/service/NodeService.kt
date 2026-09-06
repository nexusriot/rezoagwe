package com.nexusriot.rezoagwe.service

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.IBinder
import androidx.core.app.NotificationCompat
import com.nexusriot.rezoagwe.MainActivity
import com.nexusriot.rezoagwe.R
import com.nexusriot.rezoagwe.core.BootstrapStatus
import com.nexusriot.rezoagwe.core.NodeStatus
import com.nexusriot.rezoagwe.core.Runtime
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.launchIn
import kotlinx.coroutines.flow.onEach
import kotlinx.coroutines.launch

/**
 * Keeps the node and/or the rendezvous service alive while the UI is away.
 *
 * A gossip protocol that only runs while its screen is open is not participating
 * in a cluster: peers evict it seconds after the phone sleeps.
 */
class NodeService : Service() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private var watcher: Job? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        createChannel()
        startForeground(NOTIFICATION_ID, buildNotification("starting…"))
        watchState()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP_ALL -> {
                // Stopping a role closes sockets and sends a goodbye, which Android
                // will not do on the main thread. The watcher below stops the service
                // once both roles are down, so there is nothing to wait for here.
                scope.launch(Dispatchers.IO) {
                    Runtime.stopNode(this@NodeService)
                    Runtime.stopBootstrap(this@NodeService)
                }
                return START_NOT_STICKY
            }
        }
        // Sticky: if the system reclaims the process, the roles come back.
        return START_STICKY
    }

    override fun onDestroy() {
        watcher?.cancel()
        scope.coroutineContext[Job]?.cancel()
        super.onDestroy()
    }

    private fun watchState() {
        val engine = Runtime.engine(this)
        val bootstrap = Runtime.bootstrap(this)
        watcher = notificationTextFlow(engine.status, bootstrap.status).onEach { text ->
            notificationManager().notify(NOTIFICATION_ID, buildNotification(text))
        }.launchIn(scope)

        // Nothing running means nothing to keep alive.
        scope.launch {
            combine(engine.status, bootstrap.status) { node, boot -> node.running || boot.running }
                .collect { active -> if (!active) stopSelf() }
        }
    }

    private fun buildNotification(text: String): Notification {
        val open = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE,
        )
        val stop = PendingIntent.getService(
            this,
            1,
            Intent(this, NodeService::class.java).setAction(ACTION_STOP_ALL),
            PendingIntent.FLAG_IMMUTABLE,
        )
        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle(getString(R.string.app_name))
            .setContentText(text)
            .setSmallIcon(android.R.drawable.stat_sys_upload)
            .setOngoing(true)
            .setContentIntent(open)
            .addAction(0, getString(R.string.stop), stop)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .build()
    }

    private fun createChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
        val channel = NotificationChannel(
            CHANNEL_ID,
            getString(R.string.channel_name),
            NotificationManager.IMPORTANCE_LOW,
        )
        notificationManager().createNotificationChannel(channel)
    }

    private fun notificationManager() =
        getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager

    companion object {
        private const val CHANNEL_ID = "rezoagwe-node"
        private const val NOTIFICATION_ID = 1
        const val ACTION_STOP_ALL = "com.nexusriot.rezoagwe.STOP_ALL"

        fun ensureRunning(context: Context) {
            val intent = Intent(context, NodeService::class.java)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                context.startForegroundService(intent)
            } else {
                context.startService(intent)
            }
        }
    }
}

/**
 * What the ongoing notification says about the running roles.
 *
 * Deliberately free of anything that ticks — the uptime is on the screen, not
 * here — so that [notificationTextFlow] can drop the repeats.
 */
internal fun notificationText(node: NodeStatus, boot: BootstrapStatus): String = when {
    node.running && boot.running ->
        "node ${node.addr} · ${node.peers} peers · ${node.keys} keys · bootstrap :${boot.port} (${boot.nodes})"
    node.running -> "node ${node.addr} · ${node.peers} peers · ${node.keys} keys"
    boot.running -> "bootstrap :${boot.port} · ${boot.nodes} nodes"
    else -> "idle"
}

/**
 * The notification text, emitted only when it actually changes.
 *
 * The status flows tick on every gossip round whether or not anything moved, and
 * re-posting an identical notification is not free: the shade rebuilds it, every
 * notification listener on the device wakes for it, and on an idle two-node
 * cluster that was happening about twenty times a minute.
 */
internal fun notificationTextFlow(
    node: Flow<NodeStatus>,
    boot: Flow<BootstrapStatus>,
): Flow<String> = combine(node, boot) { n, b -> notificationText(n, b) }.distinctUntilChanged()
