package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.KvStore
import com.nexusriot.rezoagwe.core.Limits
import com.nexusriot.rezoagwe.core.NodeConfig
import com.nexusriot.rezoagwe.core.NodeEngine
import com.nexusriot.rezoagwe.proto.FINGERPRINT_BUCKETS
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

    private fun engine(nick: String, limits: Limits = Limits()): NodeEngine {
        val e = NodeEngine(
            NodeConfig(
                advertiseHost = "127.0.0.1",
                port = freePort(),
                nick = nick,
                gossipIntervalMs = 3_600_000,
                heartbeatIntervalMs = 3_600_000,
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
            7 to "a2026aaa5ddb78cb47d1db8e5973b787420dafb7fc1ad30cca5944a5495f9898",
            13 to "5afa98b8cab4abc9294acac825fad1b3f02d6125ec2f5b0def0456acb56e3abc",
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
}
