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
 */
val WireJson: Json = Json {
    encodeDefaults = false
    ignoreUnknownKeys = true
    explicitNulls = false
}
