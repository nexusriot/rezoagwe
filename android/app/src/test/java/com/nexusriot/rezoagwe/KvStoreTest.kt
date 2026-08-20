package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.KvStore
import com.nexusriot.rezoagwe.core.WriteOptions
import com.nexusriot.rezoagwe.proto.Digest
import com.nexusriot.rezoagwe.proto.KVAction
import com.nexusriot.rezoagwe.proto.KVUpdate
import com.nexusriot.rezoagwe.proto.KeyVersion
import com.nexusriot.rezoagwe.proto.Version
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class KvStoreTest {

    @Test
    fun applyIsLastWriteWins() {
        val kv = KvStore("self")
        kv.write("k", "local")

        assertFalse(
            "an older version must not win",
            kv.apply(KVUpdate(KVAction.SET, "k", "stale", Version(0, "peer"))),
        )
        assertEquals("local", kv["k"])

        assertTrue(kv.apply(KVUpdate(KVAction.SET, "k", "fresh", Version(9, "peer"))))
        assertEquals("fresh", kv["k"])
    }

    @Test
    fun tombstoneBlocksStaleResurrect() {
        val kv = KvStore("self")
        kv.apply(KVUpdate(KVAction.SET, "k", "v", Version(2, "a")))
        kv.apply(KVUpdate(KVAction.DELETE, "k", "", Version(5, "a")))
        assertNull(kv["k"])

        kv.apply(KVUpdate(KVAction.SET, "k", "zombie", Version(3, "b")))
        assertNull("a stale set must not resurrect a deleted key", kv["k"])

        kv.apply(KVUpdate(KVAction.SET, "k", "back", Version(6, "b")))
        assertEquals("back", kv["k"])
    }

    @Test
    fun clockAdvancesOnApply() {
        val kv = KvStore("self")
        kv.apply(KVUpdate(KVAction.SET, "k", "v", Version(100, "peer")))
        val update = kv.write("mine", "x")!!
        assertTrue("local write must sort after everything seen", update.version.counter > 100)
    }

    /** The Go and Kotlin stores must resolve a conflict the same way, or replicas diverge. */
    @Test
    fun concurrentWritesConvergeOnNodeIdTiebreak() {
        val a = KvStore("node-a")
        val b = KvStore("node-b")

        val fromA = a.write("k", "from-a")!!
        val fromB = b.write("k", "from-b")!!
        a.apply(fromB)
        b.apply(fromA)

        assertEquals(a["k"], b["k"])
        assertEquals("from-b", a["k"])
    }

    @Test
    fun compareAndSwap() {
        val kv = KvStore("self")
        val first = kv.write("lock", "alice")!!

        assertNull(
            "CAS against the wrong version must fail",
            kv.write("lock", "bob", WriteOptions(expect = Version(99, "x"))),
        )
        assertEquals("alice", kv["lock"])

        assertNotNull(kv.write("lock", "bob", WriteOptions(expect = first.version)))
        assertEquals("bob", kv["lock"])
    }

    @Test
    fun compareAndSwapAgainstAbsence() {
        val kv = KvStore("self")
        assertNotNull("claiming a free key must succeed", kv.write("lock", "alice", WriteOptions(expect = Version())))
        assertNull("claiming a held key must fail", kv.write("lock", "bob", WriteOptions(expect = Version())))
        assertEquals("alice", kv["lock"])
    }

    @Test
    fun ttlHidesExpiredKey() {
        var now = 1000L
        val kv = KvStore("self") { now }
        kv.write("session", "token", WriteOptions(expiresAt = 1010))

        assertEquals("token", kv["session"])
        now = 1011
        assertNull("an expired key must read as absent", kv["session"])
        assertEquals(0, kv.size())
    }

    /**
     * Expiry must not bump the Lamport clock: a sweep that outranked a concurrent
     * legitimate write would silently destroy it.
     */
    @Test
    fun sweepKeepsVersion() {
        var now = 1000L
        val kv = KvStore("self") { now }
        val written = kv.write("session", "token", WriteOptions(expiresAt = 1001))!!

        now = 1002
        assertEquals(1, kv.sweepExpired())

        val update = kv.updatesFor(listOf("session")).single()
        assertEquals(KVAction.DELETE, update.action)
        assertEquals(written.version, update.version)

        kv.write("session", "fresh")
        assertEquals("a later write must still beat the swept tombstone", "fresh", kv["session"])
    }

    @Test
    fun gcReclaimsOldTombstonesOnly() {
        var now = 1000L
        val kv = KvStore("self") { now }
        kv.write("a", "1")
        kv.remove("a")
        kv.write("b", "2")

        assertEquals(0, kv.gcTombstones(3600))
        now = 1000 + 7200
        assertEquals(1, kv.gcTombstones(3600))
        assertEquals(0, kv.tombstones())
        assertEquals("2", kv["b"])
    }

    @Test
    fun digestPaginatesTheKeyspace() {
        val kv = KvStore("self")
        listOf("a", "b", "c", "d").forEach { kv.write(it, it) }

        val first = kv.digest("", 2)
        assertEquals(listOf("a", "b"), first.entries.map { it.key })
        assertTrue("a truncated digest must report an upper bound", first.hi.isNotEmpty())

        val second = kv.digest("b", 2)
        assertEquals(listOf("c", "d"), second.entries.map { it.key })
        assertEquals("the final batch reaches the end of the keyspace", "", second.hi)
    }

    @Test
    fun reconcilePushesAndPulls() {
        val local = KvStore("local")
        local.apply(KVUpdate(KVAction.SET, "shared", "new", Version(5, "x")))
        local.apply(KVUpdate(KVAction.SET, "only-local", "v", Version(1, "x")))
        local.apply(KVUpdate(KVAction.SET, "stale-local", "old", Version(1, "x")))

        val (push, pull) = local.reconcile(
            Digest(
                from = "peer",
                entries = listOf(
                    KeyVersion("shared", Version(2, "x")),
                    KeyVersion("stale-local", Version(9, "x")),
                    KeyVersion("only-peer", Version(1, "x")),
                ),
            ),
            maxPush = 100,
            maxPull = 100,
        )

        val pushed = push.map { it.key }.toSet()
        assertTrue("shared" in pushed)
        assertTrue("only-local" in pushed)
        assertFalse("must not push what the peer holds newer", "stale-local" in pushed)
        assertEquals(setOf("stale-local", "only-peer"), pull.toSet())
    }

    /** A key outside the advertised range was simply not reached yet, and must not be pushed. */
    @Test
    fun reconcileRespectsDigestRange() {
        val local = KvStore("local")
        listOf("a" to "1", "m" to "2", "z" to "3").forEach { (k, v) -> local.write(k, v) }

        val (push, _) = local.reconcile(
            Digest(from = "peer", lo = "", hi = "n", entries = listOf(KeyVersion("a", Version(99, "x")))),
            maxPush = 100,
            maxPull = 100,
        )
        assertEquals(listOf("m"), push.map { it.key })
    }

    @Test
    fun historyRecordsWriters() {
        val kv = KvStore("self")
        kv.write("k", "v1")
        kv.apply(KVUpdate(KVAction.SET, "k", "v2", Version(50, "peer")))

        val history = kv.history("k")
        assertEquals(2, history.size)
        assertTrue(history[0].local)
        assertFalse(history[1].local)
        assertEquals("v2", history[1].value)
    }

    @Test
    fun digestRangeBoundsMatchGo() {
        // Lo is exclusive (it is the sender's cursor), hi is exclusive.
        assertFalse(KvStore.inDigestRange("a", "a", ""))
        assertTrue(KvStore.inDigestRange("b", "a", ""))
        assertTrue(KvStore.inDigestRange("b", "a", "c"))
        assertFalse(KvStore.inDigestRange("c", "a", "c"))
    }
}
