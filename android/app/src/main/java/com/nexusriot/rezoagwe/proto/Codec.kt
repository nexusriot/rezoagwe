package com.nexusriot.rezoagwe.proto

import java.nio.ByteBuffer
import java.security.SecureRandom
import javax.crypto.Mac
import javax.crypto.spec.SecretKeySpec
import kotlinx.serialization.encodeToString

/** Why a frame was rejected. Each maps to a counter, so drops are never silent. */
enum class DecodeError { SHORT_FRAME, BAD_MAC, CLOCK_SKEW, REPLAY, MALFORMED }

class DecodeException(val reason: DecodeError) : Exception(reason.name)

/** A decoded packet: its kind and its still-encoded JSON body. */
data class Frame(val kind: Byte, val body: ByteArray) {
    override fun equals(other: Any?): Boolean =
        other is Frame && kind == other.kind && body.contentEquals(other.body)

    override fun hashCode(): Int = 31 * kind + body.contentHashCode()
}

/**
 * Frames, authenticates and replay-checks packets.
 *
 * There is no unauthenticated path. With no pre-shared key the key is derived from
 * the cluster name alone, which still keeps two clusters on one LAN apart but — the
 * name being public — provides no secrecy.
 */
class Codec(
    psk: String,
    cluster: String,
    private val skewMillis: Long = DEFAULT_SKEW_MILLIS,
    private val now: () -> Long = System::currentTimeMillis,
) {
    private val key: ByteArray = deriveKey(psk, cluster)
    private val random = SecureRandom()
    private val seen = LinkedHashMap<Long, Long>()
    private var lastPrune = 0L

    fun encode(kind: Byte, body: ByteArray): ByteArray {
        val nonce = ByteArray(NONCE_LEN).also(random::nextBytes)
        val ts = now() / 1000
        val frame = ByteArray(HEADER_LEN + body.size)
        frame[0] = kind
        System.arraycopy(nonce, 0, frame, 1, NONCE_LEN)
        ByteBuffer.wrap(frame, 1 + NONCE_LEN, TS_LEN).putLong(ts)
        System.arraycopy(body, 0, frame, HEADER_LEN, body.size)
        val tag = tag(kind, nonce, frame.copyOfRange(1 + NONCE_LEN, 1 + NONCE_LEN + TS_LEN), body)
        System.arraycopy(tag, 0, frame, 1 + NONCE_LEN + TS_LEN, MAC_LEN)
        return frame
    }

    inline fun <reified T> encode(kind: Byte, value: T): ByteArray =
        encode(kind, WireJson.encodeToString(value).toByteArray())

    fun decode(frame: ByteArray): Frame {
        if (frame.size < HEADER_LEN) throw DecodeException(DecodeError.SHORT_FRAME)
        val kind = frame[0]
        val nonce = frame.copyOfRange(1, 1 + NONCE_LEN)
        val tsBytes = frame.copyOfRange(1 + NONCE_LEN, 1 + NONCE_LEN + TS_LEN)
        val mac = frame.copyOfRange(1 + NONCE_LEN + TS_LEN, HEADER_LEN)
        val body = frame.copyOfRange(HEADER_LEN, frame.size)

        if (!constantTimeEquals(mac, tag(kind, nonce, tsBytes, body))) {
            throw DecodeException(DecodeError.BAD_MAC)
        }

        val ts = ByteBuffer.wrap(tsBytes).long * 1000
        val nowMillis = now()
        val delta = nowMillis - ts
        if (delta > skewMillis || delta < -skewMillis) throw DecodeException(DecodeError.CLOCK_SKEW)

        if (!remember(ByteBuffer.wrap(nonce).long, nowMillis)) {
            throw DecodeException(DecodeError.REPLAY)
        }
        return Frame(kind, body)
    }

    private fun tag(kind: Byte, nonce: ByteArray, ts: ByteArray, body: ByteArray): ByteArray {
        val mac = Mac.getInstance(HMAC).apply { init(SecretKeySpec(key, HMAC)) }
        mac.update(kind)
        mac.update(nonce)
        mac.update(ts)
        mac.update(body)
        return mac.doFinal()
    }

    /**
     * Records a nonce and reports whether it was new. Entries older than twice the
     * skew can never be accepted again on timestamp grounds, so they are dropped
     * rather than kept forever.
     */
    private fun remember(nonce: Long, nowMillis: Long): Boolean {
        synchronized(seen) {
            if (nowMillis - lastPrune > skewMillis) {
                val cutoff = nowMillis - 2 * skewMillis
                seen.entries.removeAll { it.value < cutoff }
                lastPrune = nowMillis
            }
            if (seen.containsKey(nonce)) return false
            seen[nonce] = nowMillis
            return true
        }
    }

    companion object {
        const val NONCE_LEN = 8
        const val TS_LEN = 8
        const val MAC_LEN = 32
        const val HEADER_LEN = 1 + NONCE_LEN + TS_LEN + MAC_LEN
        const val DEFAULT_SKEW_MILLIS = 30_000L
        const val DEFAULT_CLUSTER = "rezoagwe"

        private const val HMAC = "HmacSHA256"
        private const val KEY_DOMAIN = "rezoagwe/wire/v2"

        /** Turns (psk, cluster) into the per-cluster framing key. */
        fun deriveKey(psk: String, cluster: String): ByteArray {
            val name = cluster.ifEmpty { DEFAULT_CLUSTER }
            // Go accepts an empty HMAC key; Java's SecretKeySpec rejects one. A
            // single zero byte is a safe substitute rather than a divergence:
            // HMAC zero-pads any key shorter than the block size, so an empty key
            // and one zero byte both expand to the same 64 zero bytes.
            val raw = psk.toByteArray()
            val secret = if (raw.isEmpty()) byteArrayOf(0) else raw
            val mac = Mac.getInstance(HMAC).apply { init(SecretKeySpec(secret, HMAC)) }
            mac.update(KEY_DOMAIN.toByteArray())
            mac.update(0)
            mac.update(name.toByteArray())
            return mac.doFinal()
        }

        /**
         * A short, shareable identity for the framing key.
         *
         * Two devices that cannot talk compare fingerprints to tell "the key
         * differs" from "the network drops packets", without either revealing
         * the key itself.
         */
        fun keyFingerprint(psk: String, cluster: String): String =
            deriveKey(psk, cluster).take(4).joinToString("") { "%02x".format(it) }

        private fun constantTimeEquals(a: ByteArray, b: ByteArray): Boolean {
            if (a.size != b.size) return false
            var diff = 0
            for (i in a.indices) diff = diff or (a[i].toInt() xor b[i].toInt())
            return diff == 0
        }
    }
}
