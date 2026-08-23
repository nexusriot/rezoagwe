package com.nexusriot.rezoagwe.core

import com.nexusriot.rezoagwe.proto.Kind
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicLong

/**
 * Counters for what a replicating node does. Anti-entropy is invisible when it
 * works, so these are how you tell it is running at all.
 */
class Metrics {
    val packetsSent = AtomicLong()
    val packetsReceived = AtomicLong()
    val bytesSent = AtomicLong()
    val bytesReceived = AtomicLong()
    val sendErrors = AtomicLong()

    val authFailures = AtomicLong()
    val replayDrops = AtomicLong()
    val skewDrops = AtomicLong()
    val malformedDrops = AtomicLong()

    val kvApplied = AtomicLong()
    val kvRejectedStale = AtomicLong()
    val kvLocalWrites = AtomicLong()
    val kvCasFailures = AtomicLong()
    val kvExpired = AtomicLong()
    val kvGced = AtomicLong()

    val aeRounds = AtomicLong()
    val aePushed = AtomicLong()
    val aePulled = AtomicLong()

    val stateSyncOut = AtomicLong()
    val stateSyncIn = AtomicLong()
    val streamErrors = AtomicLong()

    private val sentByKind = HashMap<Byte, AtomicLong>()
    private val recvByKind = HashMap<Byte, AtomicLong>()

    // Traffic per address rather than per cluster: "the node sends and receives"
    // hides the case that matters, which is one peer that only ever receives.
    private val perAddr = ConcurrentHashMap<String, AddrCounters>()

    private val rateLock = Any()
    private var lastSampleMs = 0L
    private var lastSent = 0L
    private var lastReceived = 0L
    private var lastBytesSent = 0L
    private var lastBytesReceived = 0L
    @Volatile
    private var rates = Rates()

    fun sent(kind: Byte, bytes: Int) {
        packetsSent.incrementAndGet()
        bytesSent.addAndGet(bytes.toLong())
        counter(sentByKind, kind).incrementAndGet()
    }

    fun received(kind: Byte, bytes: Int) {
        packetsReceived.incrementAndGet()
        bytesReceived.addAndGet(bytes.toLong())
        counter(recvByKind, kind).incrementAndGet()
    }

    fun sentTo(addr: String, bytes: Int) {
        val c = counters(addr)
        c.packetsOut.incrementAndGet()
        c.bytesOut.addAndGet(bytes.toLong())
    }

    fun receivedFrom(addr: String, bytes: Int) {
        val c = counters(addr)
        c.packetsIn.incrementAndGet()
        c.bytesIn.addAndGet(bytes.toLong())
    }

    fun sendFailedTo(addr: String) {
        counters(addr).sendErrors.incrementAndGet()
    }

    /** A frame from [addr] that did not authenticate, parse, or pass the replay guard. */
    fun rejectedFrom(addr: String) {
        counters(addr).rejected.incrementAndGet()
    }

    fun forAddr(addr: String): AddrTraffic = counters(addr).snapshot(addr)

    fun addrs(): List<AddrTraffic> =
        perAddr.entries.map { (addr, c) -> c.snapshot(addr) }.sortedBy { it.addr }

    private fun counters(addr: String): AddrCounters = perAddr.getOrPut(addr) { AddrCounters() }

    /**
     * Folds the counters since the previous call into per-second rates.
     *
     * Rates are computed here rather than in the UI so they keep updating while
     * the screen is closed — which is exactly when a node is expected to keep
     * gossiping.
     */
    fun sample(nowMs: Long) {
        synchronized(rateLock) {
            val sent = packetsSent.get()
            val received = packetsReceived.get()
            val outBytes = bytesSent.get()
            val inBytes = bytesReceived.get()
            val elapsed = nowMs - lastSampleMs
            if (lastSampleMs > 0 && elapsed > 0) {
                val perSec = 1000.0 / elapsed
                rates = Rates(
                    windowMs = elapsed,
                    packetsSent = (sent - lastSent) * perSec,
                    packetsReceived = (received - lastReceived) * perSec,
                    bytesSent = (outBytes - lastBytesSent) * perSec,
                    bytesReceived = (inBytes - lastBytesReceived) * perSec,
                )
            }
            lastSampleMs = nowMs
            lastSent = sent
            lastReceived = received
            lastBytesSent = outBytes
            lastBytesReceived = inBytes
        }
    }

    private fun counter(map: HashMap<Byte, AtomicLong>, kind: Byte): AtomicLong =
        synchronized(map) { map.getOrPut(kind) { AtomicLong() } }

    fun snapshot(): MetricsSnapshot {
        val kinds = sortedMapOf<String, KindCount>()
        synchronized(sentByKind) {
            sentByKind.forEach { (k, v) ->
                kinds[Kind.name(k)] = KindCount(Kind.name(k), v.get(), 0)
            }
        }
        synchronized(recvByKind) {
            recvByKind.forEach { (k, v) ->
                val name = Kind.name(k)
                kinds[name] = (kinds[name] ?: KindCount(name, 0, 0)).copy(received = v.get())
            }
        }
        return MetricsSnapshot(
            packetsSent = packetsSent.get(),
            packetsReceived = packetsReceived.get(),
            bytesSent = bytesSent.get(),
            bytesReceived = bytesReceived.get(),
            sendErrors = sendErrors.get(),
            authFailures = authFailures.get(),
            replayDrops = replayDrops.get(),
            skewDrops = skewDrops.get(),
            malformedDrops = malformedDrops.get(),
            kvApplied = kvApplied.get(),
            kvRejectedStale = kvRejectedStale.get(),
            kvLocalWrites = kvLocalWrites.get(),
            kvCasFailures = kvCasFailures.get(),
            kvExpired = kvExpired.get(),
            kvGced = kvGced.get(),
            aeRounds = aeRounds.get(),
            aePushed = aePushed.get(),
            aePulled = aePulled.get(),
            stateSyncOut = stateSyncOut.get(),
            stateSyncIn = stateSyncIn.get(),
            streamErrors = streamErrors.get(),
            kinds = kinds.values.toList(),
            rates = rates,
        )
    }
}

/** Per-address counters. One instance per address the node has ever talked to. */
class AddrCounters {
    val packetsOut = AtomicLong()
    val packetsIn = AtomicLong()
    val bytesOut = AtomicLong()
    val bytesIn = AtomicLong()
    val sendErrors = AtomicLong()
    val rejected = AtomicLong()

    fun snapshot(addr: String) = AddrTraffic(
        addr = addr,
        packetsOut = packetsOut.get(),
        packetsIn = packetsIn.get(),
        bytesOut = bytesOut.get(),
        bytesIn = bytesIn.get(),
        sendErrors = sendErrors.get(),
        rejected = rejected.get(),
    )
}

data class AddrTraffic(
    val addr: String,
    val packetsOut: Long = 0,
    val packetsIn: Long = 0,
    val bytesOut: Long = 0,
    val bytesIn: Long = 0,
    val sendErrors: Long = 0,
    val rejected: Long = 0,
)

/** Throughput over the last sampling window. */
data class Rates(
    val windowMs: Long = 0,
    val packetsSent: Double = 0.0,
    val packetsReceived: Double = 0.0,
    val bytesSent: Double = 0.0,
    val bytesReceived: Double = 0.0,
)

data class KindCount(val kind: String, val sent: Long, val received: Long)

data class MetricsSnapshot(
    val packetsSent: Long = 0,
    val packetsReceived: Long = 0,
    val bytesSent: Long = 0,
    val bytesReceived: Long = 0,
    val sendErrors: Long = 0,
    val authFailures: Long = 0,
    val replayDrops: Long = 0,
    val skewDrops: Long = 0,
    val malformedDrops: Long = 0,
    val kvApplied: Long = 0,
    val kvRejectedStale: Long = 0,
    val kvLocalWrites: Long = 0,
    val kvCasFailures: Long = 0,
    val kvExpired: Long = 0,
    val kvGced: Long = 0,
    val aeRounds: Long = 0,
    val aePushed: Long = 0,
    val aePulled: Long = 0,
    val stateSyncOut: Long = 0,
    val stateSyncIn: Long = 0,
    val streamErrors: Long = 0,
    val kinds: List<KindCount> = emptyList(),
    val rates: Rates = Rates(),
)
