package com.nexusriot.rezoagwe.core

import com.nexusriot.rezoagwe.proto.Kind
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
        )
    }
}

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
)
