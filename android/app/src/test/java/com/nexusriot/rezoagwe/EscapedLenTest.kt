package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.proto.MAX_FRAME_BYTES
import com.nexusriot.rezoagwe.proto.MAX_REPLICABLE_VALUE_BYTES
import com.nexusriot.rezoagwe.proto.escapedLen
import kotlin.random.Random
import kotlinx.serialization.builtins.serializer
import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Test

/**
 * escapedLen stands in for the encoder when a value is too big to want a
 * second copy of, so it has to agree with it exactly — for every input, or it
 * is not a size check but a guess.
 *
 * This is kotlinx.serialization, not Go's encoder: the escape sets differ, and
 * each implementation measures its own. What they share is the byte ceiling.
 */
class EscapedLenTest {
    private val json = Json

    private fun encodedBytes(s: String): Int =
        json.encodeToString(String.serializer(), s).toByteArray(Charsets.UTF_8).size

    @Test
    fun everyAsciiCharacterCostsWhatTheEncoderCharges() {
        for (c in 0 until 0x80) {
            val s = c.toChar().toString()
            assertEquals("char 0x${c.toString(16)}", encodedBytes(s), escapedLen(s))
        }
    }

    @Test
    fun agreesWithTheEncoderOnTheCasesSomeoneThoughtOf() {
        val cases = listOf(
            "", "plain", "has \"quotes\"", "back\\slash", "new\nline", "tab\there",
            "bell\u0007", "nul\u0000", "<html>&amp;</html>", "héllo wörld 日本語 🎉",
            " ", " ", "🎉", "lone\uD800surrogate", "\uDC00trailing",
        )
        for (s in cases) assertEquals(s, encodedBytes(s), escapedLen(s))
    }

    /** Random UTF-16 code units, lone surrogates and all: the ones nobody thought of. */
    @Test
    fun agreesWithTheEncoderOnArbitraryCodeUnits() {
        val rng = Random(1234)
        repeat(3000) {
            val s = buildString { repeat(rng.nextInt(12)) { append(rng.nextInt(0x11000).toChar()) } }
            assertEquals(s, encodedBytes(s), escapedLen(s))
        }
    }

    /** The same ceiling everywhere, or a value one node accepts is one another refuses. */
    @Test
    fun theCeilingIsTheOneTheOtherImplementationsUse() {
        assertEquals(64 shl 20, MAX_FRAME_BYTES)
        assertEquals((64 shl 20) - (64 shl 10), MAX_REPLICABLE_VALUE_BYTES)
    }
}
