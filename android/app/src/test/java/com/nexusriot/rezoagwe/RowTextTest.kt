package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.NodeEngine
import com.nexusriot.rezoagwe.ui.displayKey
import com.nexusriot.rezoagwe.ui.displayValue
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * A key is arbitrary UTF-8: a newline or a tab in one is legal. Left
 * unescaped, a key holding a newline drew across two lines and read as two
 * keys — next to a delete button. The value on the row had always been
 * escaped this way; the key was the one field that was not.
 */
class RowTextTest {
    @Test
    fun `a key is escaped for a one-line row, like its value`() {
        assertEquals("line1 ⏎ line2 tabbed", displayKey("line1\nline2\ttabbed"))
        assertEquals("first ⏎ second", displayValue("first\nsecond"))
    }

    /** Two keys that differ only in a control character must not render alike. */
    @Test
    fun `two keys differing only in a control character render differently`() {
        assertNotEquals(displayKey("a\nb"), displayKey("a b"))
    }

    @Test
    fun `an empty key and an empty value each say so`() {
        assertEquals("(empty key)", displayKey(""))
        assertEquals("(empty)", displayValue(""))
    }

    /** Nothing bounds a key's length, and a long one pushes the row's controls off screen. */
    @Test
    fun `a long key is capped`() {
        val capped = displayKey("x".repeat(500))
        assertEquals(120, capped.length)
        assertTrue(capped.endsWith("..."))
    }

    /**
     * The activity feed is a one-line-per-event log, and both a key and a
     * value are arbitrary UTF-8. A newline in either broke the entry in two,
     * so one event read as two. Only the value was ever previewed.
     */
    @Test
    fun `an activity preview stays on one line`() {
        val got = NodeEngine.preview("line1\nline2\ttabbed")
        assertFalse("preview still breaks the line: $got", got.contains("\n") || got.contains("\t"))
        assertTrue(got.contains("⏎"))
    }
}
