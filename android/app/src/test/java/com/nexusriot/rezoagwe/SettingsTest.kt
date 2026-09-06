package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.Settings
import com.nexusriot.rezoagwe.proto.Codec
import org.junit.Assert.assertEquals
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
}
