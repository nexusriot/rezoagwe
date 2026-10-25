package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.*
import java.io.File
import java.nio.file.Files
import org.junit.Assert.*
import org.junit.Test

class ProfileHealthTest {
    @Test fun profilesRetainSourceWithoutCopyingToAnotherCluster() {
        val root = Files.createTempDirectory("rezo-profiles").toFile()
        try {
            File(root,"node.json").writeText("legacy")
            val a = Profiles.initialize(root,"alpha")
            assertEquals("legacy",File(a,"node.json").readText())
            val b = Profiles.initialize(root,"../beta")
            assertFalse(File(b,"node.json").exists())
            File(a,"node.json").writeText("updated")
            assertEquals("updated",File(Profiles.initialize(root,"alpha"),"node.json").readText())
        } finally { root.deleteRecursively() }
    }
    @Test fun corruptionIsRetainedAndWriteFailuresVisible() {
        val root = Files.createTempDirectory("rezo-health").toFile()
        try {
            val file = File(root,"node.json");file.writeText("{broken")
            val p = Persister(file);assertNull(p.load())
            assertEquals("{broken",File(p.health.recoveryFile).readText())
            p.save(1,PersistState(nodeId="new"))
            assertTrue(p.health.lastSuccessMs>0);assertFalse(p.health.pending)
            assertEquals("new",Persister(file).load()!!.nodeId)
            val blocker=File(root,"blocker");blocker.writeText("file")
            val failed=Persister(File(blocker,"node.json"));failed.save(1,PersistState())
            assertTrue(failed.health.pending);assertTrue(failed.health.error.isNotEmpty())
        } finally { root.deleteRecursively() }
    }

    // Read off the tablet's Diag screen: "Last save: 1792946341473 ms UTC",
    // with a stray leading space whenever there was no error to prefix.
    @Test
    fun `the saved-state line reads as a time, as on the desktop`() {
        val ok = persistenceDetail(PersistenceHealth(lastSuccessMs = 1_792_946_341_473))
        assertTrue(ok, ok.startsWith("Mode: snapshot;"))
        assertTrue(ok, ok.contains("Last save: 2026-10-25T"))
        assertTrue(persistenceDetail(PersistenceHealth()).contains("Last save: never"))
        assertTrue(persistenceDetail(PersistenceHealth(error = "disk full")).startsWith("disk full Mode:"))
    }

    // On the tablet an import from another cluster outranked every replica
    // here and the screen said nothing — beside it stood the previous file's
    // "Import failed", so it read as having failed too.
    @Test
    fun `an import says what it applied, and names a foreign cluster`() {
        val e = NodeEngine(NodeConfig(cluster = "hwlab"), dataFile = null,
            scope = kotlinx.coroutines.CoroutineScope(kotlinx.coroutines.Dispatchers.Unconfined))
        val foreign = e.exportFile().copy(cluster = "otherlab", entries = listOf(
            com.nexusriot.rezoagwe.proto.KVUpdate(action = com.nexusriot.rezoagwe.proto.KVAction.SET, key = "k", value = "v",
                version = com.nexusriot.rezoagwe.proto.Version(counter = 9, node = "n"))))
        val outcome = e.importFile(com.nexusriot.rezoagwe.proto.WireJson.encodeToString(ExportFile.serializer(), foreign), false)
        assertEquals(ImportOutcome(applied = 1, total = 1, cluster = "otherlab"), outcome)
        assertEquals("Imported 1 of 1 entry (merge by version), from cluster otherlab, not hwlab", outcome.message("hwlab"))
        assertEquals("Imported 0 of 2 entries (merge by version)", ImportOutcome(0, 2, "hwlab").message("hwlab"))
    }
}
