package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.KvStore
import com.nexusriot.rezoagwe.core.Limits
import com.nexusriot.rezoagwe.core.NodeConfig
import com.nexusriot.rezoagwe.core.NodeEngine
import com.nexusriot.rezoagwe.core.Refusal
import com.nexusriot.rezoagwe.core.WriteOptions
import com.nexusriot.rezoagwe.proto.FINGERPRINT_BUCKETS
import com.nexusriot.rezoagwe.proto.Hello
import com.nexusriot.rezoagwe.proto.MAX_REPLICABLE_VALUE_BYTES
import com.nexusriot.rezoagwe.proto.StoreLimits
import com.nexusriot.rezoagwe.proto.WireJson
import com.nexusriot.rezoagwe.proto.KVAction
import com.nexusriot.rezoagwe.proto.KVUpdate
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
 * The three implementations are one protocol, so a correctness fix in the Go
 * node is a bug still open here until it lands here too. These are the ones
 * that were: an anti-entropy round that repaired half of what it identified, a
 * single keyspace cursor shared across every peer, a store with no bound on
 * what a peer could push into it, and no way to ask whether the replicas
 * actually agree.
 */
class ParityTest {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val engines = mutableListOf<NodeEngine>()

    @After
    fun tearDown() {
        engines.forEach { runCatching { it.stop() } }
        scope.cancel()
    }

    private fun freePort(): Int = ServerSocket(0).use { it.localPort }

    private fun engine(nick: String, limits: Limits = Limits(), heartbeatMs: Long = 3_600_000): NodeEngine {
        val e = NodeEngine(
            NodeConfig(
                advertiseHost = "127.0.0.1",
                port = freePort(),
                nick = nick,
                gossipIntervalMs = 3_600_000,
                heartbeatIntervalMs = heartbeatMs,
                evictThresholdMs = 3_600_000,
                sweepIntervalMs = 3_600_000,
                limits = limits,
            ),
            dataFile = null,
            scope = scope,
        )
        engines += e
        e.start()
        return e
    }

    // The bug: the digest advertised 256 keys while a repair round could carry
    // 128, and the sender's cursor advanced by the whole digest — so the tail of
    // every badly-diverged range waited for a full wrap of the keyspace.
    @Test
    fun `the digest is never wider than one round can repair`() {
        assertEquals(64, NodeConfig(digestBatch = 4096, maxPush = 128, maxPull = 64).effectiveDigestBatch)
        assertEquals(32, NodeConfig(digestBatch = 32, maxPush = 128, maxPull = 128).effectiveDigestBatch)

        // The shipped default has to already satisfy it, or every cluster runs
        // with the bug until someone tunes it.
        val d = NodeConfig()
        assertTrue(d.digestBatch <= minOf(d.maxPush, d.maxPull))
    }

    @Test
    fun `one round repairs everything the digest identified`() {
        val a = engine("alice")
        val b = engine("bob")
        a.addPeer(b.addr)
        b.addPeer(a.addr)

        repeat(300) { i -> b.store.write("k%04d".format(i), "v") }

        a.antiEntropyRound(b.addr)
        val deadline = System.currentTimeMillis() + 5_000
        while (a.store.size() < a.config.effectiveDigestBatch && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
        }
        assertTrue(
            "one round repaired ${a.store.size()} of the ${a.config.effectiveDigestBatch} it advertised for",
            a.store.size() >= a.config.effectiveDigestBatch,
        )
    }

    // A shared cursor divided the keyspace among whichever peers the random
    // target picked, so covering the store against one peer took as many wraps
    // as there were peers.
    @Test
    fun `each peer gets its own keyspace cursor`() {
        val a = engine("alice")
        val b = engine("bob")
        val c = engine("carol")
        a.addPeer(b.addr)
        a.addPeer(c.addr)

        repeat(a.config.effectiveDigestBatch * 3) { i -> a.store.write("k%05d".format(i), "v") }

        a.antiEntropyRound(b.addr)
        a.antiEntropyRound(c.addr)
        val summaryAfterBoth = a.diagnostics().store.digestCursor
        assertTrue("cursors not advanced: $summaryAfterBoth", summaryAfterBoth.contains("2 of 2"))

        a.forgetPeer(b.addr)
        assertTrue(
            "the cursor survived the peer",
            a.diagnostics().store.digestCursor.contains("1 of 1"),
        )
    }

    @Test
    fun `a store limit refuses an oversize write from a peer as well as a local one`() {
        val kv = KvStore("n1")
        kv.setLimits(Limits(maxValueBytes = 8))

        assertNotNull("a value exactly at the limit was refused", kv.write("k", "12345678"))
        assertNull("a value over the limit was accepted", kv.write("k", "123456789"))

        val applied = kv.apply(
            KVUpdate(
                action = KVAction.SET,
                key = "big",
                value = "x".repeat(64),
                version = Version(9, "peer"),
            ),
        )
        assertFalse("an oversize remote update was applied", applied)
        assertNull(kv["big"])
        // The clock still advances: refusing the value is not a reason to let
        // this node's later writes sort before the one it refused.
        assertEquals(9L, kv.fingerprint().clock)
    }

    @Test
    fun `a key limit counts live keys and always allows an update`() {
        val kv = KvStore("n1")
        kv.setLimits(Limits(maxKeys = 2))
        kv.write("a", "1")
        kv.write("b", "1")

        assertNull("a third key was accepted past maxKeys=2", kv.write("c", "1"))
        assertNotNull("an update to a key already held was refused", kv.write("a", "2"))

        kv.remove("a")
        assertNotNull("a deleted key did not free its slot", kv.write("c", "1"))
    }

    @Test
    fun `an unlimited store is the default`() {
        assertNotNull(KvStore("n1").write("k", "x".repeat(100_000)))
    }

    // The vectors the Go and JavaScript stores are held to. A fold that differs
    // between ports reports two converged replicas as divergent, which is the
    // loudest possible false alarm from the one feature whose job is to be
    // believed.
    @Test
    fun `the store fingerprint matches the Go vectors byte for byte`() {
        val kv = KvStore("node-a")
        kv.apply(KVUpdate(action = KVAction.SET, key = "alpha", value = "one", version = Version(3, "node-a")))
        kv.apply(KVUpdate(action = KVAction.SET, key = "beta", value = "two", version = Version(7, "node-b")))
        kv.apply(
            KVUpdate(action = KVAction.DELETE, key = "gamma", version = Version(9, "node-a"), deletedAt = 1_700_000_000),
        )

        val zero = "0".repeat(64)
        val want = mapOf(
            7 to "2b28e2ab4fe49de1ac3292835d3f89a2d896a3349a7cba255bfb32eefce76ffd",
            13 to "80c5334e9dffd54e9791a5080260534d255246b82b94077e9537e2ef01c05fd3",
        )

        val f = kv.fingerprint()
        assertEquals(FINGERPRINT_BUCKETS, f.buckets.size)
        assertEquals(2, f.keys)
        assertEquals(1, f.tombstones)
        assertEquals(9L, f.clock)
        f.buckets.forEachIndexed { i, got -> assertEquals("bucket $i", want[i] ?: zero, got) }
    }

    // A key stays in the bucket its *name* chose, whatever happens to its value.
    // Bucketing on the whole entry relocates it on every write, so one stale
    // value lights up two buckets and neither names a region worth inspecting.
    @Test
    fun `a differing version changes exactly one bucket`() {
        fun seeded(): KvStore = KvStore("n1").apply {
            apply(KVUpdate(action = KVAction.SET, key = "alpha", value = "one", version = Version(3, "n1")))
            apply(KVUpdate(action = KVAction.SET, key = "beta", value = "two", version = Version(7, "n2")))
        }
        val a = seeded()
        val b = seeded().apply {
            apply(KVUpdate(action = KVAction.SET, key = "alpha", value = "one", version = Version(4, "n1")))
        }

        val fa = a.fingerprint().buckets
        val fb = b.fingerprint().buckets
        assertEquals(1, fa.indices.count { fa[it] != fb[it] })
    }

    @Test
    fun `two empty stores fingerprint identically`() {
        assertEquals(KvStore("node-a").fingerprint().buckets, KvStore("node-b").fingerprint().buckets)
    }

    @Test
    fun `a consistency check finds a divergent replica and localises it`() {
        val a = engine("alice")
        val b = engine("bob")
        a.addPeer(b.addr)
        b.addPeer(a.addr)

        repeat(10) { i -> b.store.apply(a.store.write("k$i", "v")!!) }

        var report = a.checkConsistency()
        assertTrue("identical replicas reported divergent: ${report.peers}", report.converged)
        assertEquals(1, report.peers.size)
        assertTrue(report.peers[0].reachable)
        assertEquals(10, report.peers[0].keys)

        a.store.write("lost", "value") // b never sees it
        report = a.checkConsistency()
        assertFalse("a missing key was reported as converged", report.converged)
        assertEquals(1, report.peers[0].differingBuckets.size)
        assertEquals(10, report.peers[0].keys)
    }

    // A peer that does not answer says nothing about whether it agrees.
    @Test
    fun `an unreachable peer is reported separately, not as a disagreement`() {
        val a = engine("alice")
        a.addPeer("127.0.0.1:${freePort()}") // nothing listening

        val report = a.checkConsistency()
        assertEquals(1, report.unreachable)
        assertTrue("an unreachable peer was counted as a disagreement", report.converged)
        assertFalse(report.peers[0].reachable)
        assertTrue(report.peers[0].error.isNotEmpty())
    }

    /**
     * A value no peer could ever receive is refused where it is written, not
     * stored and reported as written.
     *
     * Found by end-to-end testing the Go node: the datagram path refuses it on
     * size, the stream path refuses the frame, and a state sync builds that
     * same frame — so it was accepted locally and sat on one replica forever
     * with nothing able to repair it. Unlike maxValueBytes this is not
     * configurable, because it is not a policy: no setting makes such a value
     * replicate.
     */
    @Test
    fun aValueTooLargeToReplicateIsRefusedWithNoLimitConfigured() {
        val kv = KvStore("n1")
        val atTheLine = "x".repeat(MAX_REPLICABLE_VALUE_BYTES)
        assertNull("a value exactly at the ceiling was refused", kv.admit("fits", atTheLine))
        assertEquals(Refusal.UNSHIPPABLE, kv.admit("over", atTheLine + "x"))
        assertNull("a refused value was written anyway", kv.write("over", atTheLine + "x", WriteOptions()))
    }

    /**
     * The ceiling is measured as the value will be written, not as it sits in
     * memory: one character of CJK is three bytes on the wire, so a string a
     * third of the ceiling is already at it and a raw length check would wave
     * it through to strand itself.
     */
    @Test
    fun theReplicationCeilingCountsEncodedBytesNotCharacters() {
        val kv = KvStore("n1")
        val wide = "\u65e5".repeat(MAX_REPLICABLE_VALUE_BYTES / 2)
        assertTrue("the test value is not under the ceiling by character count",
            wide.length < MAX_REPLICABLE_VALUE_BYTES)
        assertEquals(
            "a string that triples on the wire slipped past the ceiling",
            Refusal.UNSHIPPABLE,
            kv.admit("blob", wide),
        )
    }

    /**
     * Why a write was refused has to be answerable: "null" reads the same for
     * a lost race and a value that will never fit, and the caller owes the
     * user different answers.
     */
    @Test
    fun aRefusedWriteSaysWhichRuleRefusedIt() {
        val kv = KvStore("n1")
        kv.setLimits(Limits(maxValueBytes = 4, maxKeys = 1))
        assertEquals(Refusal.TOO_LARGE, kv.admit("a", "toolong"))
        assertNull(kv.admit("a", "ok"))
        kv.write("a", "ok", WriteOptions())
        assertEquals(Refusal.TOO_MANY_KEYS, kv.admit("b", "ok"))
    }

    /** Every refusal has something to say; a blank one is a dialog with no message. */
    @Test
    fun everyRefusalExplainsItself() {
        for (r in Refusal.entries) {
            assertTrue("$r has no explanation", r.explain().isNotBlank())
        }
    }

    // A peer's advertised limits outlived the peer: removePeer dropped its
    // cursor and its id bindings but not its limits, and the warning list was
    // not filtered to current peers, so a node that left kept its "limits
    // differ" finding for the life of this one.
    @Test
    fun `a departed peer takes its limits warning with it`() {
        // Limits travel on the heartbeat, so a's has to come round.
        val a = engine("a", Limits(maxKeys = 5), heartbeatMs = 50)
        val b = engine("b")
        a.addPeer(b.addr)
        b.addPeer(a.addr)
        val deadline = System.currentTimeMillis() + 5_000
        while (b.diagnostics().replicationWarnings.isEmpty() && System.currentTimeMillis() < deadline) Thread.sleep(10)
        assertEquals(1, b.diagnostics().replicationWarnings.size)
        b.forgetPeer(a.addr)
        assertEquals(emptyList<String>(), b.diagnostics().replicationWarnings)
    }

    // The encoder omits defaults, so "no limits" goes out as `"limits":{}`.
    // Every reader must take the missing fields as zero; the desktop once read
    // them as absent and reported every phone as a limits mismatch.
    @Test
    fun `no limits are advertised as an empty object`() {
        val hello = Hello(from = "a", limits = StoreLimits())
        assertTrue(WireJson.encodeToString(Hello.serializer(), hello).contains("\"limits\":{}"))
    }

    // Found on an IPv6-only Wi-Fi: the app advertised "2a00:…:add9:3137",
    // which no Go peer can parse, so it saw the cluster and the cluster never
    // saw it. Both its own address and every packet source are bracketed now,
    // and the unbracketed form is refused at the door as Go refuses it.
    @Test
    fun `an IPv6 host is bracketed wherever an address is built`() {
        assertEquals("[2a00:f3c::1]:3137", NodeEngine.joinHostPort("2a00:f3c::1", 3137))
        assertEquals("[2a00:f3c::1]:3137", NodeEngine.joinHostPort("[2a00:f3c::1]", 3137))
        assertEquals("[fe80::1%wlan0]:3137", NodeEngine.joinHostPort("fe80::1%wlan0", 3137))
        assertEquals("10.0.0.2:3137", NodeEngine.joinHostPort("10.0.0.2", 3137))
        assertFalse(NodeEngine.validPeerAddr("2a00:f3c::1:3137"))
        assertTrue(NodeEngine.validPeerAddr("[2a00:f3c::1]:3137"))
        assertTrue(NodeEngine.validPeerAddr(":3137"))
        val e = NodeEngine(NodeConfig(advertiseHost = "2a00:f3c::1", port = 3137), dataFile = null, scope = scope)
        assertEquals("[2a00:f3c::1]:3137", e.addr)
        assertNotNull(com.nexusriot.rezoagwe.net.UdpTransport.parseAddr(e.addr))
    }

    @Test
    fun `a peer advertising an IPv6 address is adopted under it`() {
        val a = engine("a")
        val b = NodeEngine(
            NodeConfig(advertiseHost = "::1", port = freePort(), nick = "b",
                gossipIntervalMs = 3_600_000, heartbeatIntervalMs = 3_600_000,
                evictThresholdMs = 3_600_000, sweepIntervalMs = 3_600_000),
            dataFile = null, scope = scope,
        ).also { engines += it; it.start() }
        b.addPeer(a.addr)
        b.gossipTo(a.addr)
        val deadline = System.currentTimeMillis() + 5_000
        while (a.peerList.value.none { it.addr == b.addr } && System.currentTimeMillis() < deadline) Thread.sleep(10)
        assertEquals(listOf(b.addr), a.peerList.value.map { it.addr })
    }

    // The Go fix, mirrored: a hearsay peer is not passed on, so a dead address
    // dies out instead of circulating (found on the tablet after an IPv6 to
    // IPv4 move: its old address was on every node's graph minutes later).
    @Test
    fun `a dead address is not kept alive by gossip`() {
        val nodes = (1..3).map { i ->
            NodeEngine(
                NodeConfig(advertiseHost = "127.0.0.1", port = freePort(), nick = "n$i",
                    gossipIntervalMs = 20, heartbeatIntervalMs = 20,
                    evictThresholdMs = 200, sweepIntervalMs = 3_600_000),
                dataFile = null, scope = scope,
            ).also { engines += it; it.start(); it.awaitJoined() }
        }
        for (n in nodes) for (m in nodes) n.addPeer(m.addr)
        val dead = "127.0.0.1:" + freePort()
        nodes[0].addPeer(dead)
        Thread.sleep(1_500)
        for (n in nodes) {
            val held = n.peerList.value.map { it.addr }
            assertFalse("${n.addr} still holds $dead: $held", dead in held)
            assertEquals(2, held.size)
        }
    }
}
