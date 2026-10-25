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
}
