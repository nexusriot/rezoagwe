package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.ui.PaneMode
import com.nexusriot.rezoagwe.ui.Screen
import com.nexusriot.rezoagwe.ui.companionOf
import com.nexusriot.rezoagwe.ui.paneModeFor
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Test

/**
 * Which layout a window gets. The sizes below are the real ones — a phone, a
 * 7-inch tablet, a 10-inch tablet, and the halves a split screen leaves.
 */
class LayoutTest {
    @Test
    fun aPhoneInPortraitGetsTabs() {
        assertEquals(PaneMode.COMPACT, paneModeFor(360, 800))
        assertEquals(PaneMode.COMPACT, paneModeFor(411, 891))
    }

    @Test
    fun aPhoneInLandscapeGetsTheRailToSaveHeight() {
        assertEquals(PaneMode.MEDIUM, paneModeFor(800, 360))
        assertEquals(PaneMode.MEDIUM, paneModeFor(891, 411))
    }

    @Test
    fun aSmallTabletInPortraitGetsTheRailAndOnePane() {
        assertEquals(PaneMode.MEDIUM, paneModeFor(600, 960))
        assertEquals(PaneMode.MEDIUM, paneModeFor(800, 1280))
    }

    @Test
    fun aTabletInLandscapeGetsTwoPanes() {
        assertEquals(PaneMode.EXPANDED, paneModeFor(1280, 800))
        assertEquals(PaneMode.EXPANDED, paneModeFor(960, 600))
    }

    @Test
    fun aSplitScreenHalfIsTreatedAsTheSizeItActuallyIs() {
        // Half of a 10-inch tablet is a phone-sized window and must not keep the
        // two-pane layout it had a moment earlier.
        assertEquals(PaneMode.MEDIUM, paneModeFor(640, 800))
        assertEquals(PaneMode.COMPACT, paneModeFor(400, 800))
    }

    @Test
    fun everyScreenHasACompanionThatIsNotItself() {
        Screen.entries.forEach { screen ->
            assertNotEquals(screen, companionOf(screen))
        }
    }

    @Test
    fun theTabOrderIsTheNavigationOrder() {
        // The rail and the tab strip are built from the same list, so the enum
        // order is the UI order and Keys stays the landing screen.
        assertEquals(Screen.KEYS, Screen.entries.first())
        assertEquals(Screen.SETTINGS, Screen.entries.last())
        assertEquals(8, Screen.entries.size)
    }
}
