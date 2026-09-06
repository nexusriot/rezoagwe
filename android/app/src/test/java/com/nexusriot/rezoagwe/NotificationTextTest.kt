package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.BootstrapStatus
import com.nexusriot.rezoagwe.core.NodeStatus
import com.nexusriot.rezoagwe.service.notificationText
import com.nexusriot.rezoagwe.service.notificationTextFlow
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Test

/**
 * The ongoing notification.
 *
 * The status flows tick on every gossip round whether or not anything moved. The
 * service used to re-post the notification for each tick, so an idle two-node
 * cluster rebuilt it in the shade — and woke every notification listener on the
 * device — about twenty times a minute, with identical text each time.
 */
class NotificationTextTest {
    private fun node(peers: Int = 0, keys: Int = 0, uptime: Long = 0) = NodeStatus(
        addr = "192.168.1.10:3137",
        peers = peers,
        keys = keys,
        uptimeSec = uptime,
        running = true,
    )

    @Test
    fun aStatusThatOnlyTicksDoesNotRepostTheNotification() = runBlocking {
        val nodes = MutableStateFlow(node(peers = 1, keys = 2, uptime = 10))
        val boots = MutableStateFlow(BootstrapStatus())
        val posted = mutableListOf<String>()

        // Unconfined: each emission is delivered before the next assignment, so the
        // list below is what the notification manager would have been handed.
        val collector = CoroutineScope(Dispatchers.Unconfined).launch {
            notificationTextFlow(nodes, boots).collect { posted += it }
        }

        // Only the uptime moves, and the uptime is not on the notification.
        nodes.value = node(peers = 1, keys = 2, uptime = 20)
        nodes.value = node(peers = 1, keys = 2, uptime = 30)
        assertEquals(listOf("node 192.168.1.10:3137 · 1 peers · 2 keys"), posted)

        // Something the text does show, on the other hand, has to get through.
        nodes.value = node(peers = 2, keys = 2, uptime = 40)
        assertEquals(
            listOf(
                "node 192.168.1.10:3137 · 1 peers · 2 keys",
                "node 192.168.1.10:3137 · 2 peers · 2 keys",
            ),
            posted,
        )

        // So does the rendezvous coming up alongside it.
        boots.value = BootstrapStatus(running = true, port = 9999, nodes = 1)
        assertEquals(3, posted.size)
        assertEquals("node 192.168.1.10:3137 · 2 peers · 2 keys · bootstrap :9999 (1)", posted.last())

        collector.cancel()
    }

    @Test
    fun bothRolesRunningAreReportedTogether() {
        val text = notificationText(
            node(peers = 1, keys = 3),
            BootstrapStatus(running = true, port = 9999, nodes = 2),
        )
        assertEquals("node 192.168.1.10:3137 · 1 peers · 3 keys · bootstrap :9999 (2)", text)
    }

    @Test
    fun theRendezvousAloneIsReported() {
        val text = notificationText(
            NodeStatus(running = false),
            BootstrapStatus(running = true, port = 9999, nodes = 4),
        )
        assertEquals("bootstrap :9999 · 4 nodes", text)
    }

    @Test
    fun nothingRunningIsIdle() {
        assertEquals("idle", notificationText(NodeStatus(), BootstrapStatus()))
    }

    /** The uptime must stay out of the text, or nothing above can ever collapse. */
    @Test
    fun theTextIgnoresTheUptime() {
        assertEquals(
            notificationText(node(peers = 1, uptime = 1), BootstrapStatus()),
            notificationText(node(peers = 1, uptime = 9_999), BootstrapStatus()),
        )
    }
}
