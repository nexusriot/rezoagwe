package com.nexusriot.rezoagwe.core

import com.nexusriot.rezoagwe.proto.Digest
import com.nexusriot.rezoagwe.proto.KVAction
import com.nexusriot.rezoagwe.proto.KVUpdate
import com.nexusriot.rezoagwe.proto.KeyVersion
import com.nexusriot.rezoagwe.proto.Version
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

/** How many versions of a key are remembered. History is a debugging aid: it is neither persisted nor synced. */
private const val HISTORY_PER_KEY = 20

/** A versioned value. Deletes are kept as tombstones so a stale set cannot resurrect a key. */
@Serializable
data class KvEntry(
    val value: String = "",
    val version: Version = Version(),
    val deleted: Boolean = false,
    @SerialName("expires_at") val expiresAt: Long = 0,
    @SerialName("deleted_at") val deletedAt: Long = 0,
) {
    fun expired(nowSec: Long): Boolean = expiresAt != 0L && nowSec >= expiresAt
    fun visible(nowSec: Long): Boolean = !deleted && !expired(nowSec)
}

/** The exported view of a live key. */
data class Entry(
    val key: String,
    val value: String,
    val version: Version,
    val expiresAt: Long,
)

/** One observed version of a key. [local] separates a write made here from one merged in from a peer. */
data class HistoryEntry(
    val version: Version,
    val value: String,
    val deleted: Boolean,
    val local: Boolean,
    val at: Long,
)

/**
 * A write option set. The zero value is an unconditional write with no expiry.
 *
 * [expect] turns the write into a compare-and-swap: it only lands if the current
 * version matches. A zero Version requires the key to be absent, which is what
 * makes "claim this lock" work.
 */
data class WriteOptions(
    val expiresAt: Long = 0,
    val expect: Version? = null,
)

/** What reconciliation decided after comparing a peer's digest with local state. */
data class Reconciliation(
    val push: List<KVUpdate>,
    val pull: List<String>,
)

/**
 * Version-aware KV store with last-write-wins merge — the Kotlin twin of the Go
 * `model.KVStore`. Both sides have to agree on every rule here, or two replicas of
 * the same cluster silently disagree about what a key holds.
 */
class KvStore(
    private val nodeId: String,
    private val nowSec: () -> Long = { System.currentTimeMillis() / 1000 },
) {
    private val lock = Any()
    private var clock: Long = 0
    private val store = HashMap<String, KvEntry>()
    private val history = HashMap<String, MutableList<HistoryEntry>>()
    private var onChange: (() -> Unit)? = null

    fun setOnChange(f: () -> Unit) = synchronized(lock) { onChange = f }

    fun loadState(savedClock: Long, entries: Map<String, KvEntry>) = synchronized(lock) {
        clock = savedClock
        store.clear()
        store.putAll(entries)
    }

    fun snapshotState(): Pair<Long, Map<String, KvEntry>> = synchronized(lock) {
        clock to HashMap(store)
    }

    private fun record(key: String, entry: KvEntry, local: Boolean) {
        val list = history.getOrPut(key) { mutableListOf() }
        list.add(HistoryEntry(entry.version, entry.value, entry.deleted, local, nowSec()))
        while (list.size > HISTORY_PER_KEY) list.removeAt(0)
    }

    /** Records a local set. Null means a compare-and-swap precondition failed. */
    fun write(key: String, value: String, opt: WriteOptions = WriteOptions()): KVUpdate? =
        mutate(key, value, remove = false, opt = opt)

    /** Records a local tombstone. Null means a compare-and-swap precondition failed. */
    fun remove(key: String, opt: WriteOptions = WriteOptions()): KVUpdate? =
        mutate(key, "", remove = true, opt = opt)

    private fun mutate(key: String, value: String, remove: Boolean, opt: WriteOptions): KVUpdate? {
        val update: KVUpdate
        synchronized(lock) {
            val now = nowSec()
            val expect = opt.expect
            if (expect != null) {
                val current = store[key]
                if (current == null || !current.visible(now)) {
                    // Missing, tombstoned and expired all read as absent.
                    if (!expect.isZero) return null
                } else if (current.version != expect) {
                    return null
                }
            }
            clock++
            val version = Version(clock, nodeId)
            val entry = if (remove) {
                KvEntry(value = "", version = version, deleted = true, deletedAt = now)
            } else {
                KvEntry(value = value, version = version, expiresAt = opt.expiresAt)
            }
            store[key] = entry
            record(key, entry, local = true)
            update = KVUpdate(
                action = if (remove) KVAction.DELETE else KVAction.SET,
                key = key,
                value = entry.value,
                version = version,
                expiresAt = entry.expiresAt,
                deletedAt = entry.deletedAt,
            )
        }
        onChange?.invoke()
        return update
    }

    /**
     * Merges a remote update under last-write-wins, reporting whether local state
     * changed. The Lamport clock advances past any counter seen, so this node's
     * later writes sort after it.
     */
    fun apply(u: KVUpdate): Boolean {
        synchronized(lock) {
            if (u.version.counter > clock) clock = u.version.counter
            val current = store[u.key]
            if (current != null && !u.version.newerThan(current.version)) return false
            val entry = KvEntry(
                value = u.value,
                version = u.version,
                deleted = u.deleted,
                expiresAt = u.expiresAt,
                deletedAt = if (u.deleted && u.deletedAt == 0L) nowSec() else u.deletedAt,
            )
            store[u.key] = entry
            record(u.key, entry, local = false)
        }
        onChange?.invoke()
        return true
    }

    operator fun get(key: String): String? = entry(key)?.value

    fun entry(key: String): Entry? = synchronized(lock) {
        val e = store[key] ?: return null
        if (!e.visible(nowSec())) return null
        Entry(key, e.value, e.version, e.expiresAt)
    }

    fun entries(): List<Entry> = synchronized(lock) {
        val now = nowSec()
        store.filter { it.value.visible(now) }
            .map { (k, e) -> Entry(k, e.value, e.version, e.expiresAt) }
            .sortedBy { it.key }
    }

    fun size(): Int = synchronized(lock) { store.count { it.value.visible(nowSec()) } }

    fun tombstones(): Int = synchronized(lock) { store.count { it.value.deleted } }

    /** Every entry, tombstones included — the snapshot a joining peer merges. */
    fun updates(): List<KVUpdate> = synchronized(lock) {
        store.map { (k, e) -> entryUpdate(k, e) }.sortedBy { it.key }
    }

    fun updatesFor(keys: List<String>): List<KVUpdate> = synchronized(lock) {
        keys.mapNotNull { k -> store[k]?.let { entryUpdate(k, it) } }
    }

    fun history(key: String): List<HistoryEntry> = synchronized(lock) {
        history[key]?.toList() ?: emptyList()
    }

    /**
     * Advertises up to [limit] keys after [after], reporting the range `(lo, hi)`
     * it covers with both bounds exclusive. An empty `hi` means the digest reached
     * the end of the keyspace and the caller should restart its cursor.
     */
    fun digest(after: String, limit: Int): Digest = synchronized(lock) {
        val keys = store.keys.filter { after.isEmpty() || it > after }.sorted()
        val batch = if (keys.size > limit) keys.take(limit) else keys
        // hi is exclusive and must be the last key's exact successor, so the next
        // batch starts where this one stopped. The Go side appends the same NUL.
        val hi = if (keys.size > limit) batch.last() + "\u0000" else ""
        Digest(
            lo = after,
            hi = hi,
            entries = batch.map { k ->
                val e = store.getValue(k)
                KeyVersion(k, e.version, e.deleted)
            },
        )
    }

    /**
     * Compares a peer's digest against local state: what to push back (this node is
     * newer, or the peer is missing it entirely) and what to pull (the peer is
     * newer, or this node has never seen it).
     */
    fun reconcile(d: Digest, maxPush: Int, maxPull: Int): Reconciliation = synchronized(lock) {
        val push = mutableListOf<KVUpdate>()
        val pull = mutableListOf<String>()
        val advertised = HashSet<String>(d.entries.size)

        for (adv in d.entries) {
            advertised.add(adv.key)
            val local = store[adv.key]
            when {
                local == null -> pull.add(adv.key)
                local.version.newerThan(adv.version) -> push.add(entryUpdate(adv.key, local))
                adv.version.newerThan(local.version) -> pull.add(adv.key)
            }
        }
        for ((k, e) in store) {
            if (k in advertised) continue
            if (inDigestRange(k, d.lo, d.hi)) push.add(entryUpdate(k, e))
        }
        Reconciliation(
            push = push.sortedBy { it.key }.take(maxPush),
            pull = pull.sorted().take(maxPull),
        )
    }

    /**
     * Turns entries whose TTL has passed into tombstones, keeping each entry's
     * existing version.
     *
     * Keeping the version is what makes expiry safe: bumping the clock here would
     * let a sweep outrank a concurrent legitimate write, and every replica sweeps
     * independently. Expiry being deterministic, replicas agree without exchanging
     * a message.
     */
    fun sweepExpired(): Int {
        var swept = 0
        synchronized(lock) {
            val now = nowSec()
            for ((k, e) in store.entries.toList()) {
                if (e.deleted || !e.expired(now)) continue
                store[k] = e.copy(value = "", deleted = true, deletedAt = now)
                swept++
            }
        }
        if (swept > 0) onChange?.invoke()
        return swept
    }

    /**
     * Drops tombstones older than [olderThanSec].
     *
     * This is the one operation that can resurrect a key: a peer that never saw the
     * delete and still holds the value will push it back once the tombstone is
     * gone. The age has to exceed the longest partition expected to heal.
     */
    fun gcTombstones(olderThanSec: Long): Int {
        if (olderThanSec <= 0) return 0
        var removed = 0
        synchronized(lock) {
            val cutoff = nowSec() - olderThanSec
            for ((k, e) in store.entries.toList()) {
                if (e.deleted && e.deletedAt != 0L && e.deletedAt < cutoff) {
                    store.remove(k)
                    history.remove(k)
                    removed++
                }
            }
        }
        if (removed > 0) onChange?.invoke()
        return removed
    }

    private fun entryUpdate(key: String, e: KvEntry) = KVUpdate(
        action = if (e.deleted) KVAction.DELETE else KVAction.SET,
        key = key,
        value = e.value,
        version = e.version,
        expiresAt = e.expiresAt,
        deletedAt = e.deletedAt,
    )

    companion object {
        /** Lo is exclusive: it is the sender's cursor, the last key it already advertised. */
        fun inDigestRange(key: String, lo: String, hi: String): Boolean {
            if (lo.isNotEmpty() && key <= lo) return false
            return hi.isEmpty() || key < hi
        }
    }
}
