package com.nexusriot.rezoagwe.proto

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json

/**
 * Wire protocol v2, byte-for-byte compatible with the Go implementation.
 *
 * Every packet is `kind(1) | nonce(8) | timestamp(8, big endian) | HMAC-SHA256(32) | JSON body`.
 * The field names below are the Go struct tags: they are the contract, so renaming a
 * property here without a [SerialName] silently stops this app from talking to a cluster.
 */
object Kind {
    const val KV: Byte = 0
    const val CHAT: Byte = 1
    const val STATE_REQUEST: Byte = 2
    const val STATE_RESPONSE: Byte = 3
    const val PEER_GOSSIP: Byte = 4
    const val HELLO: Byte = 5
    const val GOODBYE: Byte = 6
    const val DIGEST: Byte = 7
    const val PULL_REQUEST: Byte = 8
    const val KV_BATCH: Byte = 9
    const val DIRECT_MESSAGE: Byte = 10

    // Allocated by the consistency check (DESIGN §14). This app does not answer
    // one yet — a checker reports it unreachable rather than divergent — but the
    // numbers are protocol, so the table stays complete and a packet that does
    // arrive is labelled rather than counted as "unknown".
    const val FINGERPRINT: Byte = 11
    const val FINGERPRINT_REPLY: Byte = 12

    const val BOOTSTRAP_REGISTER: Byte = 20
    const val BOOTSTRAP_DISCOVER: Byte = 21
    const val BOOTSTRAP_ROSTER: Byte = 22

    fun name(kind: Byte): String = when (kind) {
        KV -> "kv"
        CHAT -> "chat"
        STATE_REQUEST -> "state_request"
        STATE_RESPONSE -> "state_response"
        PEER_GOSSIP -> "peer_gossip"
        HELLO -> "hello"
        GOODBYE -> "goodbye"
        DIGEST -> "digest"
        PULL_REQUEST -> "pull_request"
        KV_BATCH -> "kv_batch"
        DIRECT_MESSAGE -> "direct_message"
        FINGERPRINT -> "fingerprint"
        FINGERPRINT_REPLY -> "fingerprint_reply"
        BOOTSTRAP_REGISTER -> "bootstrap_register"
        BOOTSTRAP_DISCOVER -> "bootstrap_discover"
        BOOTSTRAP_ROSTER -> "bootstrap_roster"
        else -> "unknown"
    }
}

/**
 * Per-key logical clock: a Lamport counter with the writing node's id as a
 * deterministic tiebreak for concurrent writes at the same counter.
 */
@Serializable
data class Version(
    val counter: Long = 0,
    val node: String = "",
) {
    /** True when this version should win over [other]. Equal is *not* newer. */
    fun newerThan(other: Version): Boolean =
        if (counter != other.counter) counter > other.counter else node > other.node

    val isZero: Boolean get() = counter == 0L && node.isEmpty()
}

/**
 * How finely a store is summarised for a consistency check. Protocol: it must
 * match the Go and JavaScript stores, or two converged replicas cannot be
 * compared bucket by bucket.
 */
const val FINGERPRINT_BUCKETS = 16

/**
 * The largest frame a stream will carry, and so the hard ceiling on anything
 * that has to replicate. The same number as transport.MaxFrameSize in Go and
 * MAX_FRAME_SIZE in the desktop client; a test pins them together.
 */
const val MAX_FRAME_BYTES = 64 shl 20

/** Room for the key, the version, the expiry, and the envelope around them. */
private const val FRAME_RESERVE = 64 shl 10

/**
 * The largest a value may be, measured as it will be written on the wire
 * rather than as it sits in memory, and still fit in a frame. Plain text costs
 * its own length, so this is also the plain answer to "how big can a value
 * be"; anything the encoder has to escape costs more.
 *
 * A value past this is not merely large, it is unreplicable: the datagram path
 * refuses it on size, the stream path refuses the frame, and a state sync
 * builds that same frame — so it would be accepted locally, reported as
 * written, and then sit on one replica forever with nothing able to repair it.
 * Refusing the write is the only honest answer, and it is a property of the
 * protocol rather than a tuning knob.
 */
const val MAX_REPLICABLE_VALUE_BYTES = MAX_FRAME_BYTES - FRAME_RESERVE

/**
 * What each character below 0x80 costs inside a JSON string, derived from the
 * encoder rather than from memory. A test re-derives the whole table and fails
 * if kotlinx.serialization ever disagrees.
 */
private val ASCII_ESCAPE_LEN = IntArray(0x80) { c ->
    when {
        c == 0x08 || c == 0x09 || c == 0x0a || c == 0x0c || c == 0x0d || c == 0x22 || c == 0x5c -> 2
        c < 0x20 -> 6 // \u00xx
        else -> 1
    }
}

/**
 * How many bytes [s] occupies once encoded as a JSON string, counted without
 * building it — the point is to measure values far too big to want a second
 * copy of.
 *
 * Counted in UTF-8 bytes, since that is what goes on the wire — which is not
 * the string's length: one character of CJK is three bytes, and a control
 * character is six once escaped.
 */
fun escapedLen(s: String): Int {
    var n = 2 // the surrounding quotes
    var i = 0
    while (i < s.length) {
        val c = s[i]
        when {
            c.code < 0x80 -> n += ASCII_ESCAPE_LEN[c.code]
            c.code < 0x800 -> n += 2
            c.isHighSurrogate() && i + 1 < s.length && s[i + 1].isLowSurrogate() -> {
                n += 4 // a well-formed pair is one four-byte code point
                i++
            }
            // kotlinx.serialization writes an unpaired surrogate through
            // unescaped, and Java's UTF-8 encoder replaces it with '?'. One
            // byte, not the six an escape would cost — measured, because
            // guessing it the other way refuses values that would have fit.
            c.isSurrogate() -> n += 1
            else -> n += 3
        }
        i++
    }
    return n
}

/** Asks a peer to summarise its whole store. */
@Serializable
data class Fingerprint(
    val from: String = "",
    /**
     * When set, also asks for the (key, version) pairs held in those buckets.
     * A checker fills it on a second request, once the digests have said which
     * buckets disagree — which turns "you differ somewhere in this sixteenth
     * of the keyspace" into the names of the keys.
     *
     * Asking for nothing is the old request exactly, and a peer that does not
     * understand the field answers with no entries, so a report against an
     * older node degrades to bucket indices rather than failing.
     */
    val buckets: List<Int> = emptyList(),
)

/**
 * A whole-store summary. Anti-entropy repairs divergence but never reports it,
 * so a cluster can sit split for as long as nobody looks; this is the looking.
 */
@Serializable
data class FingerprintReply(
    val from: String = "",
    val nick: String = "",
    val keys: Int = 0,
    val tombstones: Int = 0,
    val clock: Long = 0,
    val buckets: List<String> = emptyList(),
    /**
     * Every (key, version) the sender holds in the buckets the request named,
     * sorted by key, tombstones included — a key deleted on one replica and
     * live on the other is exactly the kind of divergence worth naming. Empty
     * unless [Fingerprint.buckets] asked.
     */
    val entries: List<KeyVersion> = emptyList(),
)

object KVAction {
    const val SET = "set"
    const val DELETE = "delete"
}

@Serializable
data class KVUpdate(
    val action: String,
    val key: String,
    val value: String = "",
    val version: Version,
    @SerialName("expires_at") val expiresAt: Long = 0,
    @SerialName("deleted_at") val deletedAt: Long = 0,
) {
    val deleted: Boolean get() = action == KVAction.DELETE
}

@Serializable
data class KVBatch(val updates: List<KVUpdate> = emptyList())

@Serializable
data class ChatMessage(
    val sender: String = "",
    val nick: String = "",
    val text: String = "",
    val ts: Long = 0,
    val action: Boolean = false,
    val to: String = "",
)

object ChatKind {
    const val MESSAGE = ""
    const val SYSTEM = "system"
    const val ACTION = "action"
    const val DIRECT = "dm"
}

@Serializable
data class ChatEntry(
    val ts: Long = 0,
    val sender: String = "",
    val nick: String = "",
    val text: String = "",
    val kind: String = ChatKind.MESSAGE,
    val to: String = "",
)

@Serializable
data class StateRequest(val from: String = "")

@Serializable
data class StateResponse(
    val kv: List<KVUpdate> = emptyList(),
    val chat: List<ChatEntry> = emptyList(),
)

@Serializable
data class PeerGossip(
    val from: String = "",
    val nick: String = "",
    val id: String = "",
    val peers: List<String> = emptyList(),
)

@Serializable
data class Hello(
    val from: String = "",
    val nick: String = "",
    val id: String = "",
)

@Serializable
data class Goodbye(
    val from: String = "",
    val nick: String = "",
)

@Serializable
data class KeyVersion(
    val key: String,
    val version: Version,
    val deleted: Boolean = false,
)

/**
 * What the sender holds for a contiguous slice of the sorted keyspace, `(lo, hi)`
 * with both bounds exclusive. The range is what lets the receiver tell "the
 * sender has nothing for this key" from "that key was outside this batch".
 */
@Serializable
data class Digest(
    val from: String = "",
    val lo: String = "",
    val hi: String = "",
    val entries: List<KeyVersion> = emptyList(),
)

@Serializable
data class PullRequest(
    val from: String = "",
    val keys: List<String> = emptyList(),
)

@Serializable
data class BootstrapRegister(
    val from: String = "",
    val nick: String = "",
)

@Serializable
data class BootstrapDiscover(val from: String = "")

@Serializable
data class BootstrapPeer(
    val addr: String = "",
    val nick: String = "",
    @SerialName("last_seen") val lastSeen: Long = 0,
)

@Serializable
data class BootstrapRoster(val peers: List<BootstrapPeer> = emptyList())

/**
 * JSON tuned to match Go's encoding/json:
 * - defaults are not written, mirroring `omitempty`
 * - unknown keys are ignored, so a newer peer can add fields without breaking us
 * - an explicit `null` falls back to the property's default
 *
 * The last one is not a nicety. Go marshals a nil slice as `null` for every field
 * without `omitempty` — `entries` in a digest, `kv` in a state response, `peers`
 * in gossip — so an empty Go node's anti-entropy round arrives as
 * `{"from":"…","entries":null}`. Without coercion that frame authenticates and
 * then fails to parse, and the repair it was carrying is silently dropped.
 */
val WireJson: Json = Json {
    encodeDefaults = false
    ignoreUnknownKeys = true
    explicitNulls = false
    coerceInputValues = true
}
