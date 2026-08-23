package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.proto.BootstrapRoster
import com.nexusriot.rezoagwe.proto.Digest
import com.nexusriot.rezoagwe.proto.KVBatch
import com.nexusriot.rezoagwe.proto.PeerGossip
import com.nexusriot.rezoagwe.proto.PullRequest
import com.nexusriot.rezoagwe.proto.StateResponse
import com.nexusriot.rezoagwe.proto.WireJson
import kotlinx.serialization.decodeFromString
import kotlinx.serialization.encodeToString
import org.junit.Assert.assertEquals
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
}
