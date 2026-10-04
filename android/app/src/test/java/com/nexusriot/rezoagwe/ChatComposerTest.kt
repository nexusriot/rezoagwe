package com.nexusriot.rezoagwe

import java.io.File
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The chat box has to send on the keyboard's own key, not only on the arrow.
 *
 * A Compose text field with no [androidx.compose.ui.text.input.ImeAction] gets
 * a generic Done key that does nothing at all. On a real tablet that meant
 * typing a message and pressing the obvious key left the text sitting in the
 * box with no feedback, while the terminal client and the desktop app both
 * send on Enter — so the same gesture worked everywhere except the one front
 * end where a keyboard is the only way to type.
 *
 * Asserted against the source because the alternative is an instrumented test
 * for one attribute, and this is the attribute that was missing.
 */
class ChatComposerTest {
    private val source: String by lazy {
        val file = File("src/main/java/com/nexusriot/rezoagwe/ui/ChatScreen.kt")
        assertTrue("ChatScreen.kt is not where this test expects it", file.exists())
        file.readText()
    }

    @Test
    fun theComposerOffersASendActionOnTheKeyboard() {
        assertTrue(
            "the chat field needs KeyboardOptions(imeAction = ImeAction.Send), or the keyboard " +
                "shows a Done key that does nothing",
            Regex("""imeAction\s*=\s*ImeAction\.Send""").containsMatchIn(source),
        )
    }

    @Test
    fun theKeyboardsSendActionIsWiredToSomething() {
        assertTrue(
            "ImeAction.Send without KeyboardActions(onSend = …) renders a send key that is still inert",
            Regex("""KeyboardActions\s*\(\s*onSend\s*=""").containsMatchIn(source),
        )
    }

    /**
     * Both routes must run the same code. They were written twice once before,
     * which is how the keyboard path ended up with no code at all.
     */
    @Test
    fun theKeyboardAndTheButtonShareOneSendPath() {
        assertTrue(
            "the send button should reuse the same lambda the keyboard action calls, not a second copy",
            Regex("""onClick\s*=\s*send\b""").containsMatchIn(source) &&
                Regex("""onSend\s*=\s*\{\s*send\(\)\s*\}""").containsMatchIn(source),
        )
    }
}
