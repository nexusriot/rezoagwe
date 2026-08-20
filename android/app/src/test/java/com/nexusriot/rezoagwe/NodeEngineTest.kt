package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.BootstrapConfig
import com.nexusriot.rezoagwe.core.BootstrapServer
import com.nexusriot.rezoagwe.core.NodeConfig
import com.nexusriot.rezoagwe.core.NodeEngine
import com.nexusriot.rezoagwe.proto.ChatKind
import com.nexusriot.rezoagwe.proto.Version
import java.net.ServerSocket
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The engine over real loopback sockets. It touches no Android APIs, so the same
 * code the app ships can be exercised here rather than only on a device.
 */
class NodeEngineTest {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val engines = mutableListOf<NodeEngine>()
    private val servers = mutableListOf<BootstrapServer>()

    @After
    fun tearDown() {
        engines.forEach { runCatching { it.stop() } }
        servers.forEach { runCatching { it.stop() } }
        scope.cancel()
    }

    private fun freePort(): Int = ServerSocket(0).use { it.localPort }

    /** Long intervals: the tests drive gossip explicitly, so a round is a step the test took. */
    private fun engine(port: Int, nick: String, seeds: List<String> = emptyList()): NodeEngine {
        val e = NodeEngine(
            NodeConfig(
                advertiseHost = "127.0.0.1",
                port = port,
                nick = nick,
                seeds = seeds,
                gossipIntervalMs = 3_600_000,
                heartbeatIntervalMs = 3_600_000,
                evictThresholdMs = 3_600_000,
                sweepIntervalMs = 3_600_000,
            ),
            dataFile = null,
            scope = scope,
        )
        engines += e
        return e
    }

    private fun waitFor(what: String, timeoutMs: Long = 5_000, condition: () -> Boolean) {
        val deadline = System.currentTimeMillis() + timeoutMs
        while (System.currentTimeMillis() < deadline) {
            if (condition()) return
            Thread.sleep(10)
        }
        throw AssertionError("timed out waiting for $what")
    }

    private fun pair(): Pair<NodeEngine, NodeEngine> {
        val a = engine(freePort(), "alice")
        val b = engine(freePort(), "bob")
        a.start()
        b.start()
        a.addPeer(b.addr)
        b.addPeer(a.addr)
        return a to b
    }

    @Test
    fun writesReachThePeer() {
        val (a, b) = pair()

        a.set("k", "v")
        waitFor("the write to replicate") { b.store["k"] == "v" }

        a.delete("k")
        waitFor("the delete to replicate") { b.store["k"] == null }
    }

    /**
     * The gap anti-entropy exists to close: a write made while the peer was
     * unreachable is never re-sent by the write path.
     */
    @Test
    fun antiEntropyRepairsAMissedWrite() {
        val a = engine(freePort(), "alice")
        val bPort = freePort()
        val b = engine(bPort, "bob")
        a.start()

        // b is not listening yet, so this write is lost to the network.
        a.addPeer("127.0.0.1:$bPort")
        a.set("lost", "value")

        b.start()
        b.addPeer(a.addr)
        assertNull("precondition: the peer must not have the write", b.store["lost"])

        a.antiEntropyRound(b.addr)
        waitFor("anti-entropy to repair the peer") { b.store["lost"] == "value" }
        assertTrue("the peer should have pulled it", b.metrics.aePulled.get() > 0)
    }

    @Test
    fun antiEntropyReconcilesBothDirections() {
        val a = engine(freePort(), "alice")
        val b = engine(freePort(), "bob")
        // Both write while neither is listening, so no update reaches the other.
        a.set("only-a", "1")
        b.set("only-b", "2")

        a.start()
        b.start()
        a.addPeer(b.addr)
        b.addPeer(a.addr)

        a.antiEntropyRound(b.addr)
        waitFor("both sides to converge") {
            a.store["only-b"] == "2" && b.store["only-a"] == "1"
        }
    }

    @Test
    fun chatReachesThePeer() {
        val (a, b) = pair()
        a.sendChat("hello cluster")
        waitFor("chat to arrive") {
            b.chat.value.any { it.text == "hello cluster" && it.kind == ChatKind.MESSAGE }
        }
    }

    /** A direct message goes to one peer, and must not turn up on a third. */
    @Test
    fun directMessagesStayPrivate() {
        val a = engine(freePort(), "alice")
        val b = engine(freePort(), "bob")
        val c = engine(freePort(), "carol")
        listOf(a, b, c).forEach { it.start() }
        a.addPeer(b.addr)
        a.addPeer(c.addr)
        b.addPeer(a.addr)
        c.addPeer(a.addr)

        assertTrue(a.sendDirect(b.addr, "psst"))
        waitFor("the dm to arrive") {
            b.chat.value.any { it.text == "psst" && it.kind == ChatKind.DIRECT }
        }

        // A joiner asking for history must not be handed someone else's dm.
        c.requestStateFrom(a.addr)
        Thread.sleep(300)
        assertFalse(
            "a direct message leaked through chat history sync",
            c.chat.value.any { it.text == "psst" },
        )
    }

    @Test
    fun compareAndSetReplicatesAndRefusesASecondClaim() {
        val (a, b) = pair()

        assertNotNull("claiming a free key must succeed", a.compareAndSet("lock", "alice", 0, Version()))
        waitFor("the claim to replicate") { b.store["lock"] == "alice" }

        assertNull("a held lock must not be claimable", b.compareAndSet("lock", "bob", 0, Version()))
        assertTrue(b.metrics.kvCasFailures.get() > 0)
    }

    @Test
    fun stateSyncCarriesAStoreLargerThanADatagram() {
        val (a, _) = pair()
        val value = "x".repeat(2048)
        repeat(100) { i -> a.set("key-%03d".format(i), value) }

        val joiner = engine(freePort(), "joiner")
        joiner.start()
        joiner.addPeer(a.addr)
        joiner.requestStateFrom(a.addr)

        waitFor("the joiner to receive the whole store") { joiner.store.size() == 100 }
        assertEquals(0, joiner.metrics.streamErrors.get())
    }

    /** Packets from another cluster must be ignored entirely, not merged. */
    @Test
    fun foreignClusterTrafficIsRejected() {
        val a = engine(freePort(), "alice")
        a.start()

        val intruder = NodeEngine(
            NodeConfig(advertiseHost = "127.0.0.1", port = freePort(), cluster = "someone-elses-cluster"),
            dataFile = null,
            scope = scope,
        )
        engines += intruder
        intruder.start()
        intruder.addPeer(a.addr)
        intruder.set("injected", "evil")

        waitFor("the packet to be counted as a failure") { a.metrics.authFailures.get() > 0 }
        assertNull("a foreign cluster wrote into the store", a.store["injected"])
    }

    @Test
    fun bootstrapHandsOutTheRoster() {
        val bootPort = freePort()
        val boot = BootstrapServer(BootstrapConfig(port = bootPort), dataFile = null, scope = scope)
        servers += boot
        boot.start()

        val a = engine(freePort(), "alice", seeds = listOf("127.0.0.1:$bootPort"))
        a.start()
        waitFor("alice to register") { boot.roster.value.any { it.addr == a.addr } }

        val b = engine(freePort(), "bob", seeds = listOf("127.0.0.1:$bootPort"))
        b.start()
        waitFor("bob to learn about alice from bootstrap") { b.peerList.value.any { it.addr == a.addr } }
        assertFalse(
            "the roster must not contain the requester",
            boot.rosterFor(b.addr).peers.any { it.addr == b.addr },
        )
    }

    /** Slash commands are engine-level, so every front end understands the same vocabulary. */
    @Test
    fun submitHandlesCommands() {
        val a = engine(freePort(), "alice")
        a.start()

        a.submit("/set colour blue")
        assertEquals("blue", a.store["colour"])

        a.submit("/setttl token 60 secret")
        assertTrue((a.store.entry("token")?.expiresAt ?: 0) > 0)

        a.submit("/del colour")
        assertNull(a.store["colour"])

        a.submit("/nick zoe")
        assertEquals("zoe", a.status.value.nick)

        a.submit("/bogus")
        assertTrue(a.chat.value.last().text.contains("unknown command"))
    }
}
