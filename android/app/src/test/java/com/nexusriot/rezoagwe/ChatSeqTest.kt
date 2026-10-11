package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.ChatOutcome
import com.nexusriot.rezoagwe.core.ChatTracker
import com.nexusriot.rezoagwe.core.MAX_PULL_ATTEMPTS
import com.nexusriot.rezoagwe.core.MAX_TRACKED_AUTHORS
import com.nexusriot.rezoagwe.core.MAX_TRACKED_GAPS
import com.nexusriot.rezoagwe.proto.ChatBacklog
import com.nexusriot.rezoagwe.proto.ChatEntry
import com.nexusriot.rezoagwe.proto.ChatMessage
import com.nexusriot.rezoagwe.proto.MAX_CHAT_PULL
import com.nexusriot.rezoagwe.proto.WireJson
import kotlinx.serialization.decodeFromString
import kotlinx.serialization.encodeToString
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The sequence rules are protocol: the Go, Kotlin and JavaScript trackers have
 * to agree or two nodes disagree about whether a line was ever said. These
 * mirror `chatseq_test.go` case for case.
 */
class ChatSeqTest {

    private fun msg(node: String, seq: Long, text: String = "x") =
        ChatMessage(sender = "10.0.0.2:3137", node = node, text = text, seq = seq)

    @Test
    fun `numbering in order is all accepted and claims nothing`() {
        val tr = ChatTracker()
        for (s in 1L..5L) {
            assertEquals(ChatOutcome.ACCEPT, tr.observe(msg("a", s)))
        }
        assertEquals(0, tr.outstanding())
        assertTrue(tr.stats().isEmpty())
    }

    /** The case the whole feature exists for. */
    @Test
    fun `a line that never arrives is named`() {
        val tr = ChatTracker()
        tr.observe(msg("a", 1))
        tr.observe(msg("a", 4))

        assertEquals(2, tr.outstanding())
        val plans = tr.pullPlans()
        assertEquals(1, plans.size)
        assertEquals("10.0.0.2:3137", plans[0].addr)
        assertEquals(listOf(2L, 3L), plans[0].seqs)
    }

    @Test
    fun `a reordered line closes its own gap and is still shown`() {
        val tr = ChatTracker()
        tr.observe(msg("a", 1))
        tr.observe(msg("a", 3))

        assertEquals(ChatOutcome.FILLED_GAP, tr.observe(msg("a", 2)))
        assertEquals(0, tr.outstanding())
        assertEquals(1L, tr.stats().single().recovered)
    }

    /** Re-delivery is what the repair path does, so recognising it is not optional. */
    @Test
    fun `a duplicate is dropped`() {
        val tr = ChatTracker()
        tr.observe(msg("a", 1))
        tr.observe(msg("a", 2))

        assertEquals(ChatOutcome.DUPLICATE, tr.observe(msg("a", 2)))
        assertEquals(ChatOutcome.DUPLICATE, tr.observe(msg("a", 1)))
    }

    @Test
    fun `joining mid-conversation claims no gap`() {
        val tr = ChatTracker()
        assertEquals(ChatOutcome.ACCEPT, tr.observe(msg("a", 97, "we were already talking")))
        assertEquals(0, tr.outstanding())
    }

    /**
     * Keyed by address alone, a restart would read as the whole conversation
     * rewinding and everything after it would be ignored as a duplicate. The
     * node id is what keeps the two apart.
     */
    @Test
    fun `a restart without a state file is a new author`() {
        val tr = ChatTracker()
        for (s in 1L..9L) tr.observe(msg("old-id", s))

        assertEquals(ChatOutcome.ACCEPT, tr.observe(msg("new-id", 1, "hello again")))
        assertEquals(ChatOutcome.ACCEPT, tr.observe(msg("new-id", 2, "still here")))
        assertEquals(0, tr.outstanding())
    }

    @Test
    fun `an author that moves address is still one author`() {
        val tr = ChatTracker()
        tr.observe(ChatMessage(sender = "10.0.0.2:3137", node = "a", seq = 1))
        assertEquals(
            ChatOutcome.ACCEPT,
            tr.observe(ChatMessage(sender = "10.0.0.2:4000", node = "a", seq = 2)),
        )
        assertEquals(0, tr.outstanding())

        tr.observe(ChatMessage(sender = "10.0.0.2:4000", node = "a", seq = 4))
        assertEquals("10.0.0.2:4000", tr.pullPlans().single().addr)
    }

    @Test
    fun `an unsequenced sender is never gap-checked`() {
        val tr = ChatTracker()
        repeat(5) {
            assertEquals(
                ChatOutcome.ACCEPT,
                tr.observe(ChatMessage(sender = "10.0.0.9:3137", text = "legacy")),
            )
        }
        assertEquals(0, tr.outstanding())
        assertTrue(tr.pullPlans().isEmpty())
    }

    /** "Gone" and "still waiting" are different answers. */
    @Test
    fun `asking stops once the author says the line is gone`() {
        val tr = ChatTracker()
        tr.observe(msg("a", 1))
        tr.observe(msg("a", 4))

        tr.markUnavailable("10.0.0.2:3137", listOf(2L, 3L))
        assertEquals(0, tr.outstanding())
        assertTrue(tr.pullPlans().isEmpty())
        assertEquals(2L, tr.stats().single().lost)
    }

    @Test
    fun `an author that never answers is eventually given up on`() {
        val tr = ChatTracker()
        tr.observe(msg("a", 1))
        tr.observe(msg("a", 3))

        repeat(MAX_PULL_ATTEMPTS) { assertEquals(1, tr.pullPlans().size) }
        assertTrue(tr.pullPlans().isEmpty())
        assertEquals(1L, tr.stats().single().lost)
    }

    @Test
    fun `one request is one datagram`() {
        val tr = ChatTracker()
        tr.observe(msg("a", 1))
        tr.observe(msg("a", (MAX_CHAT_PULL * 3).toLong()))

        val plan = tr.pullPlans().single()
        assertEquals(MAX_CHAT_PULL, plan.seqs.size)
        assertEquals(2L, plan.seqs.first())
    }

    @Test
    fun `the gap set is bounded`() {
        val tr = ChatTracker()
        tr.observe(msg("a", 1))
        tr.observe(msg("a", (MAX_TRACKED_GAPS * 4).toLong()))

        assertTrue(tr.outstanding() <= MAX_TRACKED_GAPS)
        assertTrue(tr.stats().single().lost > 0)
    }

    /**
     * The sequence is protocol, so this port has to read exactly the bytes Go
     * writes — and write bytes Go can read. A gap in one port that is a
     * duplicate in another is two nodes disagreeing about whether a line was
     * ever said.
     */
    @Test
    fun `a sequenced chat message decodes the bytes Go writes`() {
        val goJson = """{"sender":"10.0.0.1:3137","nick":"alice","text":"hello",""" +
            """"ts":1700000000,"seq":7,"node":"node-a"}"""
        val m = WireJson.decodeFromString<ChatMessage>(goJson)
        assertEquals(7L, m.seq)
        assertEquals("node-a", m.node)
        assertEquals("hello", m.text)

        // A peer older than the field sends neither, and must read as
        // unsequenced rather than failing — that is what keeps a rolling
        // upgrade talking.
        val legacy = WireJson.decodeFromString<ChatMessage>("""{"sender":"a","nick":"","text":"hi","ts":1}""")
        assertEquals(0L, legacy.seq)
        assertEquals("", legacy.node)
    }

    @Test
    fun `a chat backlog decodes the bytes Go writes, empty collections included`() {
        val full = WireJson.decodeFromString<ChatBacklog>(
            """{"from":"a","messages":[{"sender":"a","nick":"","text":"x","ts":1,"seq":2,"node":"n"}],""" +
                """"unavailable":[3,4]}""",
        )
        assertEquals(1, full.messages.size)
        assertEquals(2L, full.messages[0].seq)
        assertEquals(listOf(3L, 4L), full.unavailable)

        // Go omits an empty slice entirely, and marshals a nil one as null for
        // any field without omitempty. An answer of "nothing to send" has to
        // decode either way, or the gap it was answering never closes.
        val omitted = WireJson.decodeFromString<ChatBacklog>("""{"from":"a"}""")
        assertTrue(omitted.messages.isEmpty())
        assertTrue(omitted.unavailable.isEmpty())

        val nulls = WireJson.decodeFromString<ChatBacklog>("""{"from":"a","messages":null,"unavailable":null}""")
        assertTrue(nulls.messages.isEmpty())
        assertTrue(nulls.unavailable.isEmpty())
    }

    @Test
    fun `an unsequenced message is encoded without seq or node`() {
        val out = WireJson.encodeToString(ChatMessage(sender = "a", text = "hi", ts = 1))
        assertTrue("seq 0 must not reach the wire: $out", !out.contains("seq"))
        assertTrue("an empty node id must not reach the wire: $out", !out.contains("node"))
    }

    private fun entry(node: String, seq: Long, text: String = "x") =
        ChatEntry(ts = 1, sender = "10.0.0.1:3137", node = node, text = text, seq = seq)

    /**
     * The baseline has to survive a restart, because a restart is exactly when
     * a line is most likely to have been missed: the node was not running.
     */
    @Test
    fun `seeding from the stored log survives a restart`() {
        val tr = ChatTracker()
        tr.seed((1L..5L).map { entry("alice", it) })
        assertEquals(0, tr.outstanding())

        // Line 6 was said while this node was down; 7 arrives once it is back.
        tr.observe(ChatMessage(sender = "10.0.0.1:3137", node = "alice", seq = 7))
        assertEquals(1, tr.outstanding())
        assertEquals(listOf(6L), tr.pullPlans().single().seqs)
    }

    /** A snapshot is a bounded slice of someone else's ring. */
    @Test
    fun `seeding never invents a gap`() {
        val tr = ChatTracker()
        tr.seed(listOf(entry("alice", 40), entry("alice", 44)))
        assertEquals(0, tr.outstanding())
        assertTrue(tr.pullPlans().isEmpty())

        tr.observe(ChatMessage(sender = "10.0.0.1:3137", node = "alice", seq = 45))
        assertEquals(0, tr.outstanding())
    }

    @Test
    fun `seeding closes a gap the snapshot happens to contain`() {
        val tr = ChatTracker()
        tr.observe(ChatMessage(sender = "10.0.0.1:3137", node = "alice", seq = 1))
        tr.observe(ChatMessage(sender = "10.0.0.1:3137", node = "alice", seq = 3))
        assertEquals(1, tr.outstanding())

        tr.seed(listOf(entry("alice", 2, "the missing one")))
        assertEquals(0, tr.outstanding())
        assertEquals(1L, tr.stats().single().recovered)
    }

    /** An entry from before the field carries no author id. */
    @Test
    fun `seeding tolerates an entry with no author id`() {
        val tr = ChatTracker()
        tr.seed(listOf(ChatEntry(ts = 1, sender = "10.0.0.1:3137", text = "old", seq = 9)))
        tr.observe(ChatMessage(sender = "10.0.0.1:3137", seq = 11))
        assertEquals(1, tr.outstanding())
    }

    /**
     * A backlog may only fill a gap this node is already waiting for.
     *
     * Found on hardware: one backlog carrying a line attributed to another
     * peer, with a large sequence, was accepted — which raised that author's
     * high-water mark so far that every genuine line it sent afterwards sorted
     * below it and was dropped as a duplicate. One datagram silenced an author
     * at that node permanently.
     */
    @Test
    fun `nothing is expected unless this node asked for it`() {
        val tr = ChatTracker()
        tr.observe(msg("alice", 1))
        tr.observe(msg("alice", 3))   // gap at 2

        assertTrue("the gap we asked for", tr.expecting("alice", "10.0.0.2:3137", 2))
        assertFalse("a sequence nobody asked for", tr.expecting("alice", "10.0.0.2:3137", 900001))
        assertFalse("a line already held", tr.expecting("alice", "10.0.0.2:3137", 1))
        assertFalse("an author never heard from", tr.expecting("mallory", "10.0.0.2:3137", 2))
    }

    /** Only the author can answer for its own lines; anyone else is hearsay. */
    @Test
    fun `a gap is only expected from the author that owns it`() {
        val tr = ChatTracker()
        tr.observe(msg("alice", 1))
        tr.observe(msg("alice", 3))

        assertTrue(tr.expecting("alice", "10.0.0.2:3137", 2))
        assertFalse("someone else offering alice's line", tr.expecting("alice", "10.0.0.9:3137", 2))
    }

    /**
     * A heartbeat says where an author's numbering stands, so the baseline is
     * set when two nodes meet rather than when one of them first speaks.
     *
     * Measured on a real cluster: a peer that had never spoken said three
     * lines while one node was off the air, and that node reported nothing —
     * the first line it heard afterwards became the baseline and the three
     * were gone.
     */
    @Test
    fun `an announcement sets the baseline on meeting`() {
        val tr = ChatTracker()
        tr.announce("bob", "10.0.0.2:3137", 4)
        assertEquals("meeting a peer claimed a conversation we were not in", 0, tr.outstanding())

        tr.observe(ChatMessage(sender = "10.0.0.2:3137", node = "bob", seq = 5))
        assertEquals(0, tr.outstanding())

        tr.announce("bob", "10.0.0.2:3137", 8)
        assertEquals(3, tr.outstanding())
        assertEquals(listOf(6L, 7L, 8L), tr.pullPlans().single().seqs)
    }

    @Test
    fun `lines from an author never heard before are still noticed`() {
        val tr = ChatTracker()
        tr.announce("bob", "10.0.0.2:3137", 0)
        tr.announce("bob", "10.0.0.2:3137", 3)
        tr.observe(ChatMessage(sender = "10.0.0.2:3137", node = "bob", seq = 4))
        assertEquals("the quiet author's lines are still invisible", 3, tr.outstanding())
    }

    @Test
    fun `a stale announcement never rewinds the mark`() {
        val tr = ChatTracker()
        tr.announce("bob", "10.0.0.2:3137", 9)
        tr.announce("bob", "10.0.0.2:3137", 4)
        assertEquals(0, tr.outstanding())
        tr.observe(ChatMessage(sender = "10.0.0.2:3137", node = "bob", seq = 10))
        assertEquals("the mark was rewound", 0, tr.outstanding())
    }

    /**
     * The gaps inside an author were bounded from the start; the authors were
     * not. Every heartbeat from every peer names one, and a node that restarts
     * without a state file comes back with a new id, so the set grew with
     * cluster churn and nothing in the protocol ever shrank it.
     */
    @Test
    fun `the number of authors followed is bounded`() {
        val tr = ChatTracker()
        repeat(MAX_TRACKED_AUTHORS * 3) { i ->
            tr.announce("author-$i", "10.0.0.1:${3000 + i}", 1)
        }
        // An author dropped and heard from again is simply met anew, which
        // claims nothing rather than inventing a gap.
        tr.announce("author-0", "10.0.0.1:3000", 50)
        assertEquals("a re-met author claimed a gap", 0, tr.outstanding())
    }

    @Test
    fun `an author that keeps talking is never evicted`() {
        val tr = ChatTracker()
        tr.announce("busy", "10.0.0.2:3137", 1)
        repeat(MAX_TRACKED_AUTHORS * 2) { i ->
            tr.announce("other-$i", "10.0.0.1:${3000 + i}", 1)
            tr.observe(ChatMessage(sender = "10.0.0.2:3137", node = "busy", seq = (i + 2).toLong()))
        }
        // Still followed: its next line continues rather than starting over.
        tr.observe(ChatMessage(sender = "10.0.0.2:3137", node = "busy", seq = (MAX_TRACKED_AUTHORS * 2 + 5).toLong()))
        assertTrue("the one author that never stopped talking was evicted", tr.outstanding() > 0)
    }
}
