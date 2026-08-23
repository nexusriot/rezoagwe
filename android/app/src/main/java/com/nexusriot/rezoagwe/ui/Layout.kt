package com.nexusriot.rezoagwe.ui

/** The screens, in the order they appear in the tab strip and the rail. */
enum class Screen(val title: String) {
    KEYS("Keys"),
    CHAT("Chat"),
    PEERS("Peers"),
    GRAPH("Graph"),
    ACTIVITY("Activity"),
    DIAGNOSTICS("Diag"),
    BOOTSTRAP("Bootstrap"),
    SETTINGS("Settings"),
}

/**
 * How much room there is to work with.
 *
 * COMPACT is a phone in portrait: one screen at a time, tabs across the top.
 * MEDIUM has room for a navigation rail beside the content — a tablet in
 * portrait, or a phone in landscape where vertical space is the scarce one.
 * EXPANDED is wide enough to show two screens at once.
 */
enum class PaneMode { COMPACT, MEDIUM, EXPANDED }

/** Below this the tab strip is the only navigation that fits. */
private const val RAIL_MIN_WIDTH_DP = 600

/** Two panes need enough width that neither ends up narrower than a phone. */
private const val TWO_PANE_MIN_WIDTH_DP = 880

/** A landscape phone is short: the rail buys back the height the tab strip costs. */
private const val SHORT_HEIGHT_DP = 480

fun paneModeFor(widthDp: Int, heightDp: Int): PaneMode = when {
    // Height matters as much as width: a big phone in landscape is wide enough for
    // two panes and far too short for them.
    widthDp >= TWO_PANE_MIN_WIDTH_DP && heightDp > SHORT_HEIGHT_DP -> PaneMode.EXPANDED
    widthDp >= RAIL_MIN_WIDTH_DP -> PaneMode.MEDIUM
    widthDp > heightDp && heightDp <= SHORT_HEIGHT_DP -> PaneMode.MEDIUM
    else -> PaneMode.COMPACT
}

/**
 * What to show in the second pane next to [main].
 *
 * The pairings are chosen so the side pane answers the question the main one
 * raises — peers while chatting, the graph while looking at peers — and never
 * shows the same screen twice.
 */
fun companionOf(main: Screen): Screen = when (main) {
    Screen.KEYS -> Screen.ACTIVITY
    Screen.CHAT -> Screen.PEERS
    Screen.PEERS -> Screen.GRAPH
    Screen.GRAPH -> Screen.DIAGNOSTICS
    Screen.ACTIVITY -> Screen.GRAPH
    Screen.DIAGNOSTICS -> Screen.GRAPH
    Screen.BOOTSTRAP -> Screen.PEERS
    Screen.SETTINGS -> Screen.DIAGNOSTICS
}
