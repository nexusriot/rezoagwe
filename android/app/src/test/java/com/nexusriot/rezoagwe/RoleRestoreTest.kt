package com.nexusriot.rezoagwe

import java.io.File
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The role the user left running has to come back when they open the app.
 *
 * `nodeWanted` is persisted precisely so a node that went away can be brought
 * back, but for a long time the only code that read it was
 * `NodeService.onStartCommand` — which cannot run after a force-stop or a
 * reboot, because those are exactly the cases where there is no service left to
 * restart. Measured on a tablet: force-stop, relaunch, and the header said
 * STOPPED while shared_prefs still said `nodeWanted=true`, with "Start node"
 * offered as though the user had never asked. The store survived; only the node
 * did not come back.
 *
 * The activity is the one component Android is guaranteed to create in that
 * case, so the check is that it still consults the persisted intent — asserted
 * against the source, because a unit test cannot launch an Activity here and a
 * rule nobody checks is the rule that quietly came undone in the first place.
 */
class RoleRestoreTest {

    @Test
    fun theActivityRestoresTheRolesTheUserLeftRunning() {
        val source = mainActivity()
        assertTrue(
            "MainActivity never reads the persisted intent: a node the user left running will " +
                "not come back after a force-stop or a reboot, which are the only times it is " +
                "gone and the service cannot restore it itself.",
            source.contains("anyRoleWanted"),
        )
        assertTrue(
            "MainActivity consults anyRoleWanted but never starts the service, so nothing acts " +
                "on the answer — NodeService.onStartCommand is what calls restoreWantedRoles.",
            source.contains("NodeService.ensureRunning"),
        )
    }

    /**
     * And it must not bind sockets on the way: `restoreWantedRoles` is
     * documented as off-main-thread only, so the activity has to go through the
     * service rather than call it directly.
     */
    @Test
    fun theActivityDoesNotRestoreRolesOnTheMainThread() {
        assertTrue(
            "MainActivity calls Runtime.restoreWantedRoles directly, which binds sockets on the " +
                "thread that runs the UI — start NodeService instead and let it do the work.",
            !mainActivity().contains("restoreWantedRoles"),
        )
    }

    private fun mainActivity(): String {
        val file = generateSequence(File(".").absoluteFile) { it.parentFile }
            .map { File(it, "app/src/main/java/com/nexusriot/rezoagwe/MainActivity.kt") }
            .firstOrNull { it.isFile }
        assertTrue("MainActivity.kt not found — has the source root moved?", file != null)
        return file!!.readText()
    }
}
