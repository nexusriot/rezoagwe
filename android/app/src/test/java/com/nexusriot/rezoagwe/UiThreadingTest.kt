package com.nexusriot.rezoagwe

import java.io.File
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Nothing that puts a datagram on the wire may be called straight from a
 * Compose callback.
 *
 * Android refuses a socket operation on the main thread, and [NodeEngine.send]
 * catches the refusal and counts it, so the failure looks exactly like packet
 * loss. On a real device that meant every key written from the Keys screen was
 * never broadcast at all — it reached peers only on the next anti-entropy
 * round, ten seconds later — and *Stop node* never sent its goodbye, so the
 * cluster waited out the eviction timeout instead. Two screens happened to wrap
 * their calls in `launch(Dispatchers.IO)` and worked; the rest did not and did
 * not, and nothing failed loudly enough to say so.
 *
 * The rule is mechanical, so it is checked mechanically: every call listed in
 * [SENDING_CALLS] found under `ui/` or `service/` has to sit inside a block
 * opened with `Dispatchers.IO`.
 */
class UiThreadingTest {
    @Test
    fun everySendingCallFromTheUiIsDispatchedOffTheMainThread() {
        val files = sourceFiles()
        assertTrue("no UI sources found to check — is the source root still where it was?", files.isNotEmpty())

        val offences = files.flatMap { file ->
            findCalls(strip(file.readText())).filterNot { it.insideIoBlock }.map { "${file.name}: ${it.call}" }
        }

        assertTrue(
            "these calls send datagrams from the thread that runs the UI, where Android drops them " +
                "silently — wrap each in scope.launch(Dispatchers.IO) { … }:\n  " +
                offences.joinToString("\n  "),
            offences.isEmpty(),
        )
    }

    /** The check is worthless if it cannot recognise a violation, so prove it can. */
    @Test
    fun theCheckCatchesAnUndispatchedCall() {
        val source = """
            Button(onClick = { Runtime.startNode(context) }) { Text("Start") }
        """.trimIndent()
        assertTrue(findCalls(source).single().call == "Runtime.startNode(")
        assertTrue(!findCalls(source).single().insideIoBlock)
    }

    @Test
    fun theCheckAcceptsADispatchedCall() {
        val source = """
            Button(onClick = {
                scope.launch(Dispatchers.IO) { Runtime.startNode(context) }
            }) { Text("Start") }
        """.trimIndent()
        assertTrue(findCalls(source).single().insideIoBlock)
    }

    @Test
    fun theCheckSeesTheEndOfADispatchedBlock() {
        val source = """
            scope.launch(Dispatchers.IO) { node.set(k, v, 0) }
            node.delete(k)
        """.trimIndent()
        val calls = findCalls(source)
        assertTrue(calls.first { it.call == "node.set(" }.insideIoBlock)
        assertTrue(!calls.first { it.call == "node.delete(" }.insideIoBlock)
    }

    private data class Call(val call: String, val insideIoBlock: Boolean)

    /**
     * Walks the source tracking brace depth, remembering the depth at which a
     * block introduced by `Dispatchers.IO` opened. Crude next to a parser, but it
     * reads the same two things a reviewer does — is this one of those calls, and
     * is it inside one of those blocks.
     */
    private fun findCalls(source: String): List<Call> {
        val found = mutableListOf<Call>()
        val ioDepths = ArrayDeque<Int>()
        var depth = 0
        var pendingIo = false
        var i = 0
        while (i < source.length) {
            if (source.startsWith(DISPATCHER, i)) {
                pendingIo = true
                i += DISPATCHER.length
                continue
            }
            when (source[i]) {
                '{' -> {
                    depth++
                    if (pendingIo) {
                        ioDepths.addLast(depth)
                        pendingIo = false
                    }
                }
                '}' -> {
                    if (ioDepths.lastOrNull() == depth) ioDepths.removeLast()
                    depth--
                }
                else ->
                    SENDING_CALLS.firstOrNull { source.startsWith(it, i) }?.let {
                        found += Call(it, ioDepths.isNotEmpty())
                    }
            }
            i++
        }
        return found
    }

    /** Comments and string literals carry braces of their own; they are not code. */
    private fun strip(source: String): String = source
        .replace(Regex("""/\*.*?\*/""", RegexOption.DOT_MATCHES_ALL), " ")
        .replace(Regex("""//[^\n]*"""), " ")
        .replace(Regex(""""(\\.|[^"\\\n])*""""), "\"\"")

    private fun sourceFiles(): List<File> {
        val root = generateSequence(File(".").absoluteFile) { it.parentFile }
            .map { File(it, "app/src/main/java/com/nexusriot/rezoagwe") }
            .firstOrNull { it.isDirectory }
            ?: return emptyList()
        return listOf("ui", "service")
            .map { File(root, it) }
            .filter { it.isDirectory }
            .flatMap { it.walkTopDown().filter { f -> f.extension == "kt" } }
            .toList()
    }

    private companion object {
        const val DISPATCHER = "Dispatchers.IO"

        /**
         * Everything that reaches [com.nexusriot.rezoagwe.core.NodeEngine.send], plus
         * the [com.nexusriot.rezoagwe.core.Runtime] entry points that start or stop a
         * role and therefore bind, close or announce on a socket.
         */
        val SENDING_CALLS = listOf(
            "node.set(",
            "node.compareAndSet(",
            "node.delete(",
            "node.compareAndDelete(",
            "node.submit(",
            "node.sendChat(",
            "node.sendAction(",
            "node.sendDirect(",
            "node.renameSelf(",
            "node.requestStateFrom(",
            "node.addPeer(",
            "node.forgetPeer(",
            "node.gossipTo(",
            "node.antiEntropyRound(",
            "node.start(",
            "node.stop(",
            "Runtime.startNode(",
            "Runtime.stopNode(",
            "Runtime.startBootstrap(",
            "Runtime.stopBootstrap(",
            "Runtime.applySettings(",
        )
    }
}
