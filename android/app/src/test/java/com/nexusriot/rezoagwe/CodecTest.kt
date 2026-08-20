package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.proto.Codec
import com.nexusriot.rezoagwe.proto.DecodeError
import com.nexusriot.rezoagwe.proto.DecodeException
import com.nexusriot.rezoagwe.proto.Hello
import com.nexusriot.rezoagwe.proto.KVAction
import com.nexusriot.rezoagwe.proto.KVUpdate
import com.nexusriot.rezoagwe.proto.Kind
import com.nexusriot.rezoagwe.proto.WireJson
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import kotlinx.serialization.encodeToString
import kotlinx.serialization.decodeFromString

class CodecTest {

    @Test
    fun roundTrip() {
        val codec = Codec("secret", "prod")
        val frame = codec.encode(Kind.HELLO, Hello(from = ":3137", nick = "phone"))

        val decoded = Codec("secret", "prod").decode(frame)
        assertEquals(Kind.HELLO, decoded.kind)
        val hello = WireJson.decodeFromString<Hello>(String(decoded.body))
        assertEquals("phone", hello.nick)
    }

    @Test
    fun wrongKeyIsRejected() {
        val frame = Codec("secret", "prod").encode(Kind.HELLO, Hello(from = ":1"))
        assertRejects(DecodeError.BAD_MAC) { Codec("other", "prod").decode(frame) }
    }

    @Test
    fun clustersAreSeparated() {
        val frame = Codec("", "alpha").encode(Kind.HELLO, Hello(from = ":1"))
        assertRejects(DecodeError.BAD_MAC) { Codec("", "beta").decode(frame) }
        Codec("", "alpha").decode(frame) // same cluster still works
    }

    @Test
    fun tamperingIsDetected() {
        val frame = Codec("secret", "prod").encode(Kind.HELLO, Hello(from = ":1"))
        frame[frame.size - 1] = (frame[frame.size - 1].toInt() xor 0xff).toByte()
        assertRejects(DecodeError.BAD_MAC) { Codec("secret", "prod").decode(frame) }
    }

    @Test
    fun replayIsRejected() {
        val sender = Codec("secret", "prod")
        val receiver = Codec("secret", "prod")
        val frame = sender.encode(Kind.HELLO, Hello(from = ":1"))

        receiver.decode(frame)
        assertRejects(DecodeError.REPLAY) { receiver.decode(frame) }
    }

    @Test
    fun staleTimestampIsRejected() {
        val past = System.currentTimeMillis() - 10 * 60 * 1000
        val sender = Codec("secret", "prod", now = { past })
        val frame = sender.encode(Kind.HELLO, Hello(from = ":1"))
        assertRejects(DecodeError.CLOCK_SKEW) { Codec("secret", "prod").decode(frame) }
    }

    @Test
    fun shortFrameIsRejected() {
        assertRejects(DecodeError.SHORT_FRAME) { Codec("", "").decode(byteArrayOf(1, 2, 3)) }
    }

    /**
     * Key derivation has to match the Go implementation exactly, or this app
     * silently cannot talk to any cluster. These vectors come from the Go
     * `proto.DeriveKey`.
     */
    @Test
    fun keyDerivationMatchesGo() {
        assertEquals(
            "557630ced9a5d93a06aa171910b5453186a09d1db4cc1f22c16ca0aa6b4b7eec",
            Codec.deriveKey("", "rezoagwe").toHex(),
        )
        assertEquals(
            "02e49ce58c95bd03572f543a1a2b99a9bd3d9a5e65e2b9196ea4865898efb43c",
            Codec.deriveKey("s3cret", "prod").toHex(),
        )
    }

    /**
     * A real frame produced by the Go node, decoded here: it proves the framing,
     * the MAC input order and the JSON field names all agree across the two
     * implementations. The clock is pinned to the frame's own timestamp so the
     * skew check does not turn this into a test with a shelf life.
     */
    @Test
    fun decodesFrameProducedByGo() {
        val frame = GO_FRAME.fromHex()
        val codec = Codec("s3cret", "prod", now = { GO_FRAME_TS * 1000 })

        val decoded = codec.decode(frame)
        assertEquals(Kind.KV, decoded.kind)

        val update = WireJson.decodeFromString<KVUpdate>(String(decoded.body))
        assertEquals(KVAction.SET, update.action)
        assertEquals("colour", update.key)
        assertEquals("blue", update.value)
        assertEquals(7L, update.version.counter)
        assertEquals("go-node", update.version.node)
    }

    /** The reverse direction: what this app encodes has to be what Go expects to read. */
    @Test
    fun encodesFrameGoCanRead() {
        val update = KVUpdate(
            action = KVAction.SET,
            key = "colour",
            value = "blue",
            version = com.nexusriot.rezoagwe.proto.Version(7, "go-node"),
        )
        val body = WireJson.encodeToString(update)
        assertEquals(
            """{"action":"set","key":"colour","value":"blue","version":{"counter":7,"node":"go-node"}}""",
            body,
        )

        val frame = Codec("s3cret", "prod").encode(Kind.KV, update)
        assertEquals(Kind.KV, frame[0])
        assertEquals(Codec.HEADER_LEN + body.length, frame.size)
    }

    /** Fields Go omits with `omitempty` must not be written, or the wire drifts apart. */
    @Test
    fun defaultsAreOmittedLikeGo() {
        val json = WireJson.encodeToString(Hello(from = ":3137"))
        assertEquals("""{"from":":3137"}""", json)
        assertTrue("nick must be omitted when empty", !json.contains("nick"))
    }

    private fun assertRejects(expected: DecodeError, block: () -> Unit) {
        try {
            block()
            fail("expected $expected")
        } catch (e: DecodeException) {
            assertEquals(expected, e.reason)
        }
    }

    private fun ByteArray.toHex(): String = joinToString("") { "%02x".format(it) }

    private fun String.fromHex(): ByteArray =
        chunked(2).map { it.toInt(16).toByte() }.toByteArray()

    companion object {
        private const val GO_FRAME =
            "00f22451949b5ebb9e000000006a7efed20c99f6e1b637e6b1604e08b2168d0cea" +
                "c41edf41f1e81f52429255b1cf25c7777b22616374696f6e223a22736574222c22" +
                "6b6579223a22636f6c6f7572222c2276616c7565223a22626c7565222c22766572" +
                "73696f6e223a7b22636f756e746572223a372c226e6f6465223a22676f2d6e6f64" +
                "65227d7d"
        private const val GO_FRAME_TS = 1786707666L
    }
}
