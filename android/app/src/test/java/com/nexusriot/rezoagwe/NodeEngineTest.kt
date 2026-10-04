package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.BootstrapConfig
import com.nexusriot.rezoagwe.core.BootstrapServer
import com.nexusriot.rezoagwe.core.NodeConfig
import com.nexusriot.rezoagwe.core.NodeEngine
import com.nexusriot.rezoagwe.core.NodeRole
import com.nexusriot.rezoagwe.core.Severity
import com.nexusriot.rezoagwe.core.healthChecks
import com.nexusriot.rezoagwe.net.MAX_DATAGRAM_PAYLOAD
import com.nexusriot.rezoagwe.proto.ChatKind
import com.nexusriot.rezoagwe.proto.Version
import java.io.IOException
import java.net.DatagramSocket
import java.net.InetSocketAddress
import java.net.ServerSocket
import java.net.SocketException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The engine over real loopback sockets. It touches no Android APIs, so the same
 * code the app ships can be exercised here rather than only on a device.
 */
/**
 * A stand-in for the platform's exception of the same name: the real one lives in
 * android.jar, which on the unit-test classpath is a stub that throws when
 * constructed. The classification under test reads the class name, and this
 * class has the name that matters.
 */
private class NetworkOnMainThreadException : RuntimeException()

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

    /**
     * A port free for both sockets the engine binds. Checking only TCP left the
     * suite able to pick a number whose UDP half was taken, which failed as a
     * BindException in an unrelated test.
     */
    private fun freePort(): Int {
        repeat(20) {
            val candidate = ServerSocket(0).use { it.localPort }
            val usable = try {
                DatagramSocket(null).use { udp ->
                    udp.reuseAddress = true
                    udp.bind(InetSocketAddress(candidate))
                    true
                }
            } catch (e: IOException) {
                false
            }
            if (usable) return candidate
        }
        throw AssertionError("no free port for both TCP and UDP")
    }

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

    /**
     * Runs a consistency check until its single peer reports named
     * differences.
     *
     * Naming the keys costs a second round trip over a real socket, and the
     * check degrades to bucket indices when that fails rather than erroring —
     * deliberately, so a peer too old to ask still produces a report. Retrying
     * is what a person does, and it keeps the assertion about the diff rather
     * than about whether one TCP exchange happened to land.
     */
    private fun namedDifferencesOf(node: NodeEngine): NodeEngine.PeerConsistency {
        var found: NodeEngine.PeerConsistency? = null
        waitFor("the check to name the diverging keys") {
            found = node.checkConsistency().peers.singleOrNull()
                ?.takeIf { !it.agrees && it.differences.isNotEmpty() }
            found != null
        }
        return found!!
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

    /**
     * Membership belongs to the socket. It used to outlive it, so the peer list
     * and the graph kept drawing a live cluster — frozen at "seen 0s ago",
     * because with nothing gossiping there was nothing left to refresh them —
     * for a node that had stopped listening.
     */
    /**
     * A send that fails has to say what failed. The counter alone cannot tell an
     * unroutable peer apart from this process refusing to send, and those two
     * have nothing in common but the number.
     */
    @Test
    fun aFailedSendRecordsItsCause() {
        val a = engine(freePort(), "alice")
        a.metrics.sendFailed("10.0.0.2:3137", NetworkOnMainThreadException())

        val failure = a.metrics.snapshot().lastSendError
        assertNotNull("the failure should have been recorded", failure)
        assertEquals("10.0.0.2:3137", failure!!.addr)
        assertEquals("NetworkOnMainThreadException", failure.cause)
        assertTrue("it should be recognised as our bug, not the network's", failure.onMainThread)
        assertEquals(1, a.metrics.sendErrors.get())
    }

    @Test
    fun anOrdinaryFailedSendIsNotBlamedOnTheUiThread() {
        val a = engine(freePort(), "alice")
        a.metrics.sendFailed("10.0.0.2:3137", SocketException("Network is unreachable"))

        val failure = a.metrics.snapshot().lastSendError!!
        assertFalse(failure.onMainThread)
        assertTrue(failure.toString().contains("Network is unreachable"))
    }

    @Test
    fun stoppingClearsMembership() {
        val (a, b) = pair()
        waitFor("the peers to see each other") { a.peerList.value.isNotEmpty() }
        assertTrue("precondition: the graph should have both nodes", a.topology.value.nodes.size > 1)

        a.stop()

        assertTrue("the peer list should empty when the node stops", a.peerList.value.isEmpty())
        assertEquals("the status should stop claiming peers", 0, a.status.value.peers)
        assertTrue(
            "the graph should collapse to this node alone",
            a.topology.value.nodes.size <= 1,
        )
        assertTrue("the peer is unaffected", b.isRunning)
    }

    /**
     * A state snapshot carries chat history the receiver may already hold. It was
     * appended wholesale, so a second sync — which is routine, one per peer a
     * joiner syncs from — showed the whole conversation twice.
     */
    @Test
    fun repeatedStateSyncDoesNotDuplicateChatHistory() {
        val (a, b) = pair()
        a.sendChat("only once")
        waitFor("the message to reach the peer") { b.chat.value.any { it.text == "only once" } }

        // The size is measured after the first sync, not before it. The first
        // one legitimately brings lines this node has not seen — a's own
        // "joined" observations — and whether gossip has already delivered
        // them is a race. Idempotency is a claim about the syncs after that,
        // which is what this test is for.
        b.requestStateFrom(a.addr)
        waitFor("the first snapshot to be served") { a.metrics.stateSyncOut.get() >= 1 }
        Thread.sleep(200)
        val settled = b.chat.value.size

        repeat(3) { b.requestStateFrom(a.addr) }
        waitFor("the snapshots to be served") { a.metrics.stateSyncOut.get() >= 4 }
        Thread.sleep(200)

        assertEquals(
            "the same line must not be merged twice",
            1,
            b.chat.value.count { it.text == "only once" },
        )
        assertEquals("repeated snapshots added lines", settled, b.chat.value.size)
    }

    /** Chat merged out of a snapshot has to end up in time order, not remote-then-local. */
    @Test
    fun mergedChatHistoryStaysInTimeOrder() {
        val (a, b) = pair()
        a.sendChat("first")
        waitFor("the first message to replicate") { b.chat.value.any { it.text == "first" } }
        b.sendChat("second")
        waitFor("the second message to replicate") { a.chat.value.any { it.text == "second" } }

        b.requestStateFrom(a.addr)
        Thread.sleep(300)

        val texts = b.chat.value.map { it.text }
        assertTrue("both messages should be present", texts.containsAll(listOf("first", "second")))
        assertTrue(
            "the earlier message should still come first, got $texts",
            texts.indexOf("first") < texts.indexOf("second"),
        )
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

    /**
     * The graph a phone can draw of a cluster it is only part of.
     *
     * Alice hears bob's peer list and so learns that bob talks to carol — a link
     * neither endpoint is alice, and the only reason a partition is visible at all.
     */
    @Test
    fun gossipTeachesUsLinksBetweenOtherNodes() {
        val a = engine(freePort(), "alice")
        val b = engine(freePort(), "bob")
        val carol = "127.0.0.1:${freePort()}"
        a.start()
        b.start()
        a.addPeer(b.addr)
        b.addPeer(a.addr)
        b.addPeer(carol)

        b.gossipTo(a.addr)
        waitFor("alice to learn the link between bob and carol") {
            a.topology.value.links.any { setOf(it.a, it.b) == setOf(b.addr, carol) }
        }

        val topology = a.topology.value
        assertEquals(NodeRole.SELF, topology.node(a.addr)!!.role)
        assertEquals(NodeRole.DIRECT, topology.node(b.addr)!!.role)
        assertEquals(2, topology.node(b.addr)!!.advertised)
        // One component: carol is unreachable for us but hangs off bob, who is not.
        assertEquals(1, topology.components().size)
    }

    @Test
    fun aPeerListedByNobodyWeTalkToStaysIndirect() {
        val a = engine(freePort(), "alice")
        val b = engine(freePort(), "bob")
        val ghost = "127.0.0.1:${freePort()}"
        a.start()
        b.start()
        a.addPeer(b.addr)
        b.addPeer(a.addr)
        b.addPeer(ghost)
        b.gossipTo(a.addr)

        // Alice adopts every gossiped address, so the ghost becomes a peer of hers
        // too; dropping it again is what leaves a node known but not reachable.
        waitFor("alice to adopt the gossiped address") { a.topology.value.node(ghost) != null }
        a.forgetPeer(ghost)
        waitFor("the ghost to remain in the graph without being a peer") {
            a.topology.value.node(ghost)?.role == NodeRole.INDIRECT
        }
    }

    @Test
    fun trafficIsAttributedToTheAddressItWentTo() {
        val (a, b) = pair()
        a.set("k", "v")
        waitFor("the write to replicate") { b.store["k"] == "v" }

        // Nothing acknowledges a KV update, so inbound traffic needs the peer to
        // say something of its own.
        b.gossipTo(a.addr)
        waitFor("inbound traffic to be attributed") {
            a.diagnostics().peers.singleOrNull()?.packetsIn?.let { it > 0 } == true
        }

        val diagnostics = a.diagnostics()
        val peer = diagnostics.peers.single()
        assertEquals(b.addr, peer.addr)
        assertTrue("packets should be counted per peer", peer.packetsOut > 0)
        assertTrue("bytes should be counted per peer", peer.bytesOut > 0)
        assertTrue("bytes in should be counted per peer", peer.bytesIn > 0)
        assertTrue(diagnostics.strangers.isEmpty())
        assertEquals(1, diagnostics.store.keys)
        assertTrue(diagnostics.running)
        assertTrue(diagnostics.streamListener)
        assertEquals(a.nodeId, diagnostics.nodeId)
    }

    /** A frame framed with another key is counted, and blamed on where it came from. */
    @Test
    fun packetsFromAForeignClusterAreRejectedAndAttributed() {
        val port = freePort()
        val victim = engine(port, "alice")
        victim.start()
        val stranger = NodeEngine(
            NodeConfig(advertiseHost = "127.0.0.1", port = freePort(), nick = "intruder", psk = "wrong-key"),
            dataFile = null,
            scope = scope,
        )
        engines += stranger
        stranger.start()
        stranger.addPeer("127.0.0.1:$port")
        stranger.set("hello", "there")

        waitFor("the frame to be refused") { victim.metrics.authFailures.get() > 0 }
        val diagnostics = victim.diagnostics()
        assertTrue("nothing may be applied from a foreign cluster", diagnostics.store.keys == 0)
        assertTrue(
            "the sender should be listed as an unknown source",
            diagnostics.strangers.any { it.rejected > 0 },
        )
        val checks = healthChecks(diagnostics, System.currentTimeMillis())
        assertTrue(checks.any { it.severity == Severity.ERROR && it.title.contains("authentication") })
    }

    /**
     * A value too big for one datagram still has to reach the peer.
     *
     * Found on a live LAN cluster of a Go node, the desktop client and this
     * app: a 200 KB write was accepted, reported success, and then never
     * replicated. Every gossip tick retried the same oversized datagram, the
     * kernel refused it with EMSGSIZE, and the node counted a send error — so
     * the one mechanism meant to repair divergence was the mechanism
     * generating it, and the cluster stayed split for as long as the key
     * existed with nothing but a counter to say so.
     *
     * Over real loopback sockets, so the ceiling under test is the kernel's.
     */
    @Test
    fun anOversizedValueReplicatesOverAStream() {
        val (a, b) = pair()
        val big = "x".repeat(MAX_DATAGRAM_PAYLOAD + 1)

        a.set("big", big)

        waitFor("the oversized value to reach the peer") { b.store["big"] == big }
        assertEquals("the stream fallback still counted a send error", 0, a.metrics.sendErrors.get())
    }

    /**
     * The same value must converge through anti-entropy, not only through the
     * write path: a peer that missed the write is repaired by the digest
     * round, which ships entries the same way.
     */
    @Test
    fun antiEntropyRepairsAnOversizedValue() {
        val a = engine(freePort(), "alice")
        a.start()
        val big = "y".repeat(MAX_DATAGRAM_PAYLOAD + 1)
        a.set("big", big)

        // b only learns of a afterwards, so it never saw the write go out.
        val b = engine(freePort(), "bob")
        b.start()
        a.addPeer(b.addr)
        b.addPeer(a.addr)

        a.antiEntropyRound(b.addr)
        waitFor("anti-entropy to repair the oversized value") { b.store["big"] == big }
    }

    /**
     * With nothing listening there is nowhere to put it, and the node must say
     * so rather than report the write as sent.
     */
    @Test
    fun anOversizedValueWithNoReachablePeerIsASendError() {
        val a = engine(freePort(), "alice")
        a.start()
        a.addPeer("127.0.0.1:${freePort()}") // nothing bound: both paths fail

        a.set("big", "z".repeat(MAX_DATAGRAM_PAYLOAD + 1))
        waitFor("the failed send to be counted") { a.metrics.sendErrors.get() > 0 }
    }


    /**
     * "They differ somewhere in bucket 7" is where a report used to stop,
     * which on a store of a few hundred keys is a sixteenth of the keyspace to
     * read by hand. The check knows which key it is; it just never said.
     *
     * Over real loopback sockets, so this exercises the second round trip the
     * naming needs, not a second copy of the diffing rules.
     */
    @Test
    fun aConsistencyReportNamesTheDivergingKey() {
        val a = engine(freePort(), "alice")
        a.start()
        for (k in listOf("alpha", "beta", "gamma", "delta")) a.set(k, "shared")

        // b is built from a's updates directly, then given one of its own, so
        // the two stores differ on exactly one name without needing a
        // partition between two real sockets.
        val b = engine(freePort(), "bob")
        b.start()
        a.store.updates().forEach { b.store.apply(it) }
        b.set("beta", "only-on-b")

        a.addPeer(b.addr)
        b.addPeer(a.addr)

        val peer = namedDifferencesOf(a)
        assertEquals("named ${peer.differences}", 1, peer.differences.size)
        assertEquals("beta", peer.differences[0].key)
        assertTrue(
            "both sides hold the key, so both versions should be named",
            peer.differences[0].local.isNotEmpty() && peer.differences[0].remote.isNotEmpty(),
        )
        assertNotEquals(peer.differences[0].local, peer.differences[0].remote)
    }

    /**
     * A key one replica has never seen is the divergence that matters most — a
     * write that never arrived — and it cannot be found by walking the local
     * store, which is the direction a naive diff takes.
     */
    @Test
    fun aReportNamesAKeyThisNodeHasNeverSeen() {
        val a = engine(freePort(), "alice")
        val b = engine(freePort(), "bob")
        a.start()
        b.start()
        b.set("orphan", "never-arrived")
        a.addPeer(b.addr)
        b.addPeer(a.addr)

        val found = namedDifferencesOf(a).differences.find { it.key == "orphan" }
        assertNotNull("a key only the peer holds was not named", found)
        assertEquals("this node never saw the key, so its version must be empty", "", found!!.local)
        assertTrue("the peer's version should be named", found.remote.isNotEmpty())
    }

    /**
     * A key deleted on one side and live on the other is a divergence the
     * version alone does not explain, so the report has to say which side
     * holds a tombstone.
     */
    @Test
    fun aReportSaysWhichSideHoldsATombstone() {
        val a = engine(freePort(), "alice")
        val b = engine(freePort(), "bob")
        a.start()
        b.start()
        a.set("doomed", "v")
        a.store.updates().forEach { b.store.apply(it) }
        b.delete("doomed")

        a.addPeer(b.addr)
        b.addPeer(a.addr)

        val d = namedDifferencesOf(a).differences.single()
        assertEquals("doomed", d.key)
        assertFalse("this node holds the value, not a tombstone", d.localDeleted)
        assertTrue("the peer holds a tombstone and the report does not say so", d.remoteDeleted)
    }

    /** Agreement still reports nothing to look at. */
    @Test
    fun aConvergedReportNamesNothing() {
        val a = engine(freePort(), "alice")
        val b = engine(freePort(), "bob")
        a.start()
        b.start()
        a.set("k", "v")
        a.store.updates().forEach { b.store.apply(it) }
        a.addPeer(b.addr)
        b.addPeer(a.addr)

        val report = a.checkConsistency()
        assertTrue("a converged cluster reported as divergent", report.converged)
        assertTrue("an agreeing peer was given differences", report.peers.single().differences.isEmpty())
    }


    /**
     * Saying the same thing twice is not a duplicate to be cleaned up.
     *
     * The merge deduplicated on the entry itself, and nothing in a chat entry
     * distinguishes two identical lines — same text, same sender, same second.
     * So "ok" said twice collapsed to one on the next state sync, which is
     * routine: one per peer a joiner syncs from. The line vanished from the
     * screen and from the persisted log, and nothing said why.
     *
     * It surfaced as an intermittent failure in
     * [repeatedStateSyncDoesNotDuplicateChatHistory], where two identical join
     * lines landed in the same second.
     */
    @Test
    fun aLineGenuinelySaidTwiceSurvivesAStateSync() {
        val (a, b) = pair()
        b.sendChat("ok")
        b.sendChat("ok") // same text, same sender, same second
        waitFor("both lines to be logged") { b.chat.value.count { it.text == "ok" } == 2 }

        b.requestStateFrom(a.addr)
        waitFor("the snapshot to be served") { a.metrics.stateSyncOut.get() >= 1 }
        Thread.sleep(200)

        assertEquals(
            "a state sync deleted one of two lines that were genuinely said",
            2,
            b.chat.value.count { it.text == "ok" },
        )
    }

    /**
     * And a snapshot may legitimately carry more copies than this node holds.
     *
     * Pulled over the stream rather than waited for over the broadcast: two
     * datagrams both arriving is not something a test can rely on, and this is
     * about the merge, not the transport.
     */
    @Test
    fun aJoinerReceivesEveryCopyOfALineSaidTwice() {
        val a = engine(freePort(), "alice")
        a.start()
        a.sendChat("ok")
        a.sendChat("ok")
        waitFor("the writer to hold both") { a.chat.value.count { it.text == "ok" } == 2 }

        val joiner = engine(freePort(), "carol")
        joiner.start()
        joiner.addPeer(a.addr)
        joiner.requestStateFrom(a.addr)
        waitFor("the joiner to receive both copies") {
            joiner.chat.value.count { it.text == "ok" } == 2
        }

        // Syncing again must not grow it further.
        joiner.requestStateFrom(a.addr)
        Thread.sleep(200)
        assertEquals(
            "a repeated snapshot duplicated the conversation",
            2,
            joiner.chat.value.count { it.text == "ok" },
        )
    }

}
