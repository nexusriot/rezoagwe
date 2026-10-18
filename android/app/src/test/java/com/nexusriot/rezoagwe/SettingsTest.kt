package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.Settings
import com.nexusriot.rezoagwe.proto.Codec
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * What the settings screen stores.
 *
 * The cluster name and the pre-shared key are not free text: both feed the
 * framing key, so a character nobody can see changes it and the node silently
 * stops talking to every peer. A soft keyboard supplies exactly that character
 * — it appends a space after a word it believes it has completed — which is how
 * a cluster typed as "e2e" reached shared preferences as "e2e " on a real
 * device, invisible in the field, in the diagnostics and in the key fingerprint
 * next to it.
 */
class SettingsTest {
    @Test
    fun trailingSpaceFromTheKeyboardIsNotPartOfTheCluster() {
        val typed = Settings(cluster = "e2e ", psk = "demo ")
        val stored = typed.sanitized()

        assertEquals("e2e", stored.cluster)
        assertEquals("demo", stored.psk)
    }

    @Test
    fun everyTextFieldIsTrimmed() {
        val typed = Settings(
            nick = "  tablet ",
            advertiseHost = " 192.168.1.10 ",
            seeds = "  192.168.1.10:9999, 192.168.1.11:9999 ",
            psk = " s3cret ",
            cluster = " home ",
        )
        val stored = typed.sanitized()

        assertEquals("tablet", stored.nick)
        assertEquals("192.168.1.10", stored.advertiseHost)
        assertEquals("192.168.1.10:9999, 192.168.1.11:9999", stored.seeds)
        assertEquals("s3cret", stored.psk)
        assertEquals("home", stored.cluster)
    }

    /** Trimming must not turn a field the user left alone into an empty one. */
    @Test
    fun blankFieldsFallBackInsteadOfEmptying() {
        val previous = Settings(nick = "tablet", cluster = "home")
        val stored = Settings(nick = "   ", cluster = " ").sanitized(previous)

        assertEquals("tablet", stored.nick)
        assertEquals("home", stored.cluster)
    }

    @Test
    fun withNoPreviousValueBlanksFallBackToTheDefaults() {
        val stored = Settings(nick = "", cluster = "").sanitized()

        assertEquals(Settings().nick, stored.nick)
        assertEquals(Codec.DEFAULT_CLUSTER, stored.cluster)
    }

    /** An empty key is a real choice — cluster-name-only framing — so it must survive. */
    @Test
    fun anEmptyKeyStaysEmpty() {
        assertEquals("", Settings(psk = "   ").sanitized().psk)
    }

    /**
     * The port field takes digits, so a port no socket can bind is one
     * keystroke away — and the two ways it fails are not alike. 99999 throws
     * on bind and leaves the node stopped, which at least says so. 0 binds an
     * ephemeral port instead, and the node advertises ":0": it still gossips
     * outward, so its own screen reads CONNECTED with a full peer list while
     * every peer has dropped it and bootstrap hands ":0" to each new joiner.
     * Both are refused in favour of the port the user already had, which is
     * what the desktop client does with the same input.
     */
    @Test
    fun anImpossiblePortIsRefusedInFavourOfThePreviousOne() {
        val previous = Settings(port = 3137, bootstrapPort = 9999)

        assertEquals(3137, Settings(port = 0).sanitized(previous).port)
        assertEquals(3137, Settings(port = 99999).sanitized(previous).port)
        assertEquals(3137, Settings(port = -1).sanitized(previous).port)
        assertEquals(3137, Settings(port = 65536).sanitized(previous).port)
        assertEquals(4000, Settings(port = 4000).sanitized(previous).port)
        assertEquals(65535, Settings(port = 65535).sanitized(previous).port)
        assertEquals(1, Settings(port = 1).sanitized(previous).port)
    }

    /** The rendezvous port binds a socket too, so it is held to the same range. */
    @Test
    fun anImpossibleBootstrapPortIsRefusedToo() {
        val previous = Settings(port = 3137, bootstrapPort = 9999)

        assertEquals(9999, Settings(bootstrapPort = 0).sanitized(previous).bootstrapPort)
        assertEquals(9999, Settings(bootstrapPort = 99999).sanitized(previous).bootstrapPort)
        assertEquals(9000, Settings(bootstrapPort = 9000).sanitized(previous).bootstrapPort)
    }

    /** With nothing to fall back to, an impossible port lands on the default rather than being kept. */
    @Test
    fun withNoPreviousValueAnImpossiblePortFallsBackToTheDefault() {
        assertEquals(Settings().port, Settings(port = 0).sanitized().port)
        assertEquals(Settings().bootstrapPort, Settings(bootstrapPort = 70000).sanitized().bootstrapPort)
    }

    @Test
    fun sanitizingIsIdempotentAndLeavesTheRestAlone() {
        val once = Settings(
            nick = "tablet ",
            port = 3137,
            cluster = "home ",
            bootstrapPort = 9999,
            tombstoneTtlSec = 600,
        ).sanitized()

        assertEquals(once, once.sanitized())
        assertEquals(3137, once.port)
        assertEquals(9999, once.bootstrapPort)
        assertEquals(600, once.tombstoneTtlSec)
    }

    /** Seeds are parsed after trimming, so a padded list still resolves. */
    @Test
    fun paddedSeedsStillParse() {
        val stored = Settings(seeds = " 192.168.1.10:9999 , 192.168.1.11:9999 ").sanitized()

        assertEquals(
            listOf("192.168.1.10:9999", "192.168.1.11:9999"),
            stored.seedList(),
        )
    }

    /**
     * Refusing a value is only half an answer.
     *
     * The form is seeded from storage, and the state flow only re-seeds it when
     * the stored settings actually change — so editing *just* the bad field
     * leaves the refused text sitting in the box with the node still on its old
     * port and nothing on screen to say so. Measured on a tablet: typing 0 into
     * Node port and pressing Apply stored 3137 and left the field reading 0.
     */
    @Test
    fun aRefusedValueIsNamedAlongsideWhatIsActuallyInEffect() {
        val kept = Settings(nick = "tablet", port = 3137, bootstrapPort = 9999, cluster = "lab")

        assertEquals(emptyList<String>(), kept.refusals(kept))

        // 0 binds an ephemeral port and the node then advertises ":0", which no
        // peer can dial; 99999 cannot bind at all.
        assertTrue(kept.copy(port = 0).refusals(kept).single().contains("Node port 0"))
        assertTrue(kept.copy(port = 0).refusals(kept).single().contains("keeping 3137"))
        assertTrue(kept.copy(port = 99999).refusals(kept).single().contains("Node port 99999"))
        assertTrue(kept.copy(bootstrapPort = 0).refusals(kept).single().contains("Bootstrap port 0"))
        assertTrue(kept.copy(nick = "   ").refusals(kept).single().contains("Nickname cannot be blank"))
        assertTrue(kept.copy(cluster = "").refusals(kept).single().contains("Cluster name cannot be blank"))

        assertEquals(2, kept.copy(port = 0, nick = "").refusals(kept).size)
    }

    /**
     * Trimming is deliberately not reported: the screen documents it, it is what
     * the user wanted, and naming it on every paste would bury the two messages
     * that matter.
     */
    @Test
    fun trimmingIsNotReportedAsARefusal() {
        val kept = Settings(nick = "tablet", port = 3137, bootstrapPort = 9999, cluster = "lab")
        val padded = kept.copy(psk = " secret ", cluster = " lab ")

        assertEquals(emptyList<String>(), padded.refusals(kept))
        assertEquals("secret", padded.sanitized(kept).psk)
    }

    /** Every refusal must correspond to something sanitized actually dropped. */
    @Test
    fun whatIsReportedIsWhatIsRefused() {
        val kept = Settings(nick = "tablet", port = 3137, bootstrapPort = 9999, cluster = "lab")
        for (typed in listOf(
            kept.copy(port = 0),
            kept.copy(port = 99999),
            kept.copy(bootstrapPort = 70000),
            kept.copy(nick = " "),
            kept.copy(cluster = ""),
        )) {
            assertTrue(typed.refusals(kept).isNotEmpty())
            assertEquals(
                "sanitized kept the old value, so the user must be told",
                kept.port.takeIf { typed.port !in Settings.VALID_PORTS } ?: typed.sanitized(kept).port,
                typed.sanitized(kept).port,
            )
        }
    }
}
