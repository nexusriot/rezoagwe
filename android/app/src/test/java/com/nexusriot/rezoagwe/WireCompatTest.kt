package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.proto.BootstrapRoster
import com.nexusriot.rezoagwe.proto.Digest
import com.nexusriot.rezoagwe.proto.KVBatch
import com.nexusriot.rezoagwe.proto.KeyVersion
import com.nexusriot.rezoagwe.proto.Version
import com.nexusriot.rezoagwe.proto.PeerGossip
import com.nexusriot.rezoagwe.proto.PullRequest
import com.nexusriot.rezoagwe.proto.StateResponse
import com.nexusriot.rezoagwe.proto.WireJson
import kotlinx.serialization.decodeFromString
import kotlinx.serialization.encodeToString
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * What Go actually puts on the wire.
 *
 * Go marshals a nil slice as `null` for any field without `omitempty`, so an
 * empty digest from a Go node is `{"from":"…","entries":null}`. Every body below
 * was produced by the Go structs in `pkg/proto/wire.go`; refusing one means the
 * repair, roster or gossip it carried is dropped after it authenticated — a
 * silent failure that looks exactly like a network problem.
 */
class WireCompatTest {
    @Test
    fun anEmptyGoDigestDecodes() {
        val body = """{"from":"127.0.0.1:3200","entries":null}"""
        val digest = WireJson.decodeFromString<Digest>(body)
        assertEquals("127.0.0.1:3200", digest.from)
        assertTrue(digest.entries.isEmpty())
        assertTrue(digest.hi.isEmpty())
    }

    @Test
    fun anEmptyGoPullRequestDecodes() {
        val request = WireJson.decodeFromString<PullRequest>("""{"from":"127.0.0.1:3200","keys":null}""")
        assertTrue(request.keys.isEmpty())
    }

    @Test
    fun goGossipFromAPeerlessNodeDecodes() {
        val gossip = WireJson.decodeFromString<PeerGossip>(
            """{"from":"127.0.0.1:3200","nick":"golang","id":"abc","peers":null}""",
        )
        assertEquals("golang", gossip.nick)
        assertTrue(gossip.peers.isEmpty())
    }

    @Test
    fun anEmptyGoStateResponseDecodes() {
        val response = WireJson.decodeFromString<StateResponse>("""{"kv":null,"chat":null}""")
        assertTrue(response.kv.isEmpty())
        assertTrue(response.chat.isEmpty())
    }

    @Test
    fun anEmptyGoBatchAndRosterDecode() {
        assertTrue(WireJson.decodeFromString<KVBatch>("""{"updates":null}""").updates.isEmpty())
        assertTrue(WireJson.decodeFromString<BootstrapRoster>("""{"peers":null}""").peers.isEmpty())
    }

    @Test
    fun weStillOmitEmptyCollectionsRatherThanSendingNull() {
        // Coercion is for reading. Writing `null` back would break a Go peer
        // decoding into a typed slice, so an empty list stays omitted.
        val encoded = WireJson.encodeToString(Digest(from = "127.0.0.1:3137"))
        assertEquals("""{"from":"127.0.0.1:3137"}""", encoded)
    }

    /**
     * The value hash is what lets a consistency check tell apart two replicas
     * that agree on the version but hold different data. A codec that drops it
     * turns that divergence back into apparent agreement — which is what the
     * JavaScript port did until a cluster test caught the key going unnamed
     * while the bucket digests disagreed.
     *
     * It must also stay *off* the anti-entropy digests, which share this shape
     * and never set one: 64 characters per entry there would shrink what fits
     * in a datagram for no gain.
     */
    @Test
    fun `a key version carries its value hash, and omits it when empty`() {
        val hash = "a".repeat(64)
        val encoded = WireJson.encodeToString(
            KeyVersion(key = "k", version = Version(3, "node-a"), valueHash = hash),
        )
        assertTrue("the hash is not on the wire: $encoded", encoded.contains("\"vhash\":\"$hash\""))
        assertFalse("omitempty must drop deleted: $encoded", encoded.contains("deleted"))

        // A digest entry sets no hash, and must not start sending an empty one.
        val digestEntry = WireJson.encodeToString(KeyVersion(key = "k", version = Version(3, "node-a")))
        assertFalse("an empty value hash must be omitted: $digestEntry", digestEntry.contains("vhash"))

        // A peer older than the field sends none, which has to decode as
        // "cannot compare" rather than failing — that is what keeps a rolling
        // upgrade from reporting every peer as divergent.
        val legacy = WireJson.decodeFromString<KeyVersion>(
            """{"key":"k","version":{"counter":3,"node":"node-a"}}""",
        )
        assertEquals("", legacy.valueHash)
    }

    /**
     * The reflexive address, pinned against the bytes Go actually writes.
     *
     * Three implementations serve this roster and all three must agree: a port
     * that omits `seen_at` leaves a joiner unable to learn where the cluster
     * sees it, and one that writes `"seen_at":""` where Go writes nothing is a
     * wire difference even though both decode the same.
     */
    @Test
    fun theRosterCarriesTheObservedAddressExactlyAsGoWritesIt() {
        val go = """{"peers":[{"addr":"10.0.0.1:3137","nick":"alice"}],"seen_at":"192.168.1.9:48812"}"""
        val decoded = WireJson.decodeFromString<BootstrapRoster>(go)
        assertEquals("192.168.1.9:48812", decoded.seenAt)
        assertEquals(1, decoded.peers.size)
        assertEquals(go, WireJson.encodeToString(decoded))
    }

    /**
     * An unobserved address is absent from the frame, not sent as "".
     *
     * Go gives `seen_at` omitempty, so it writes nothing; a port that wrote the
     * empty string would be sending a field Go never sends. The `peers` array
     * is a separate, older and harmless difference — Go has no omitempty there
     * and writes `"peers":[]` where this port omits the default — and both
     * decoders read either form, so only the new field is pinned here.
     */
    @Test
    fun anUnobservedAddressIsOmittedTheWayGoOmitsIt() {
        val encoded = WireJson.encodeToString(BootstrapRoster(peers = emptyList(), seenAt = ""))
        assertFalse("an empty seen_at must not be written", encoded.contains("seen_at"))
        assertEquals("", WireJson.decodeFromString<BootstrapRoster>("""{"peers":[]}""").seenAt)
    }

    /** An older rendezvous sends no field at all; that is not an error. */
    @Test
    fun aRosterWithoutTheFieldStillDecodes() {
        val decoded = WireJson.decodeFromString<BootstrapRoster>("""{"peers":null}""")
        assertEquals("", decoded.seenAt)
        assertTrue(decoded.peers.isEmpty())
    }
}
