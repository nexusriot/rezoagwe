package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.PersistState
import com.nexusriot.rezoagwe.core.Persister
import java.io.File
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

/**
 * What happens when the state file cannot be read.
 *
 * The node must still start — losing the store is bad and refusing to start is
 * worse — but it starts with an empty replica *and a freshly minted identity*,
 * and then rewrites the unreadable file with that new state. Its earlier writes
 * are outranked until anti-entropy refills it, and every chat line it has said
 * reads as coming from a different author. This port caught the exception and
 * returned null, so nothing anywhere could say any of that had happened.
 */
class PersistenceTest {
    @get:Rule
    val tmp = TemporaryFolder()

    @Test
    fun aCorruptFileIsReportedRatherThanSwallowed() {
        val file = File(tmp.root, "node.json")
        file.writeText("""{"kv": [ truncated""")

        val persister = Persister(file)
        assertNull("a corrupt file must not yield a state", persister.load())
        assertTrue("and the reason must be kept", persister.loadError.isNotEmpty())
    }

    @Test
    fun aFileThatReadsFineLeavesNoError() {
        // Every node has a state file, so an error recorded for the ordinary
        // case would raise a finding on every report and get the real one
        // ignored.
        val file = File(tmp.root, "node.json")
        val persister = Persister(file)
        persister.save(1, PersistState(nodeId = "abc", clock = 4))

        val reader = Persister(file)
        assertNotNull(reader.load())
        assertEquals("", reader.loadError)
    }

    @Test
    fun aMissingFileIsAFreshNodeAndNotAFailure() {
        val persister = Persister(File(tmp.root, "absent.json"))
        assertNull(persister.load())
        assertEquals("", persister.loadError)
    }

    /** A later good read has to clear an earlier failure, or the finding sticks forever. */
    @Test
    fun aSuccessfulReadClearsTheError() {
        val file = File(tmp.root, "node.json")
        file.writeText("not json")
        val persister = Persister(file)
        persister.load()
        assertTrue(persister.loadError.isNotEmpty())

        persister.save(1, PersistState(nodeId = "abc"))
        assertNotNull(persister.load())
        assertEquals("", persister.loadError)
    }
}
