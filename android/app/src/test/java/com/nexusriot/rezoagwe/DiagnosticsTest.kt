package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.GossipView
import com.nexusriot.rezoagwe.core.MetricsSnapshot
import com.nexusriot.rezoagwe.core.NodeDiagnostics
import com.nexusriot.rezoagwe.core.Peer
import com.nexusriot.rezoagwe.core.PeerDiagnostics
import com.nexusriot.rezoagwe.core.SendFailure
import com.nexusriot.rezoagwe.core.Severity
import com.nexusriot.rezoagwe.core.StoreDiagnostics
import com.nexusriot.rezoagwe.core.TopologyBuilder
import com.nexusriot.rezoagwe.core.asReport
import com.nexusriot.rezoagwe.core.IdConflict
import com.nexusriot.rezoagwe.core.healthChecks
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Each check is a claim about what a counter means, so each is worth pinning: a
 * wrong explanation sends someone looking at the network when the key is wrong.
 */
class DiagnosticsTest {
    private val now = 1_000_000L

    private fun healthy() = NodeDiagnostics(
        addr = "10.0.0.1:3137",
        nick = "phone",
        nodeId = "abc",
        cluster = "home",
        keyFingerprint = "deadbeef",
        pskSet = true,
        configuredPort = 3137,
        boundPort = 3137,
        streamListener = true,
        localIpv4 = "10.0.0.1",
        running = true,
        uptimeSec = 60,
        evictThresholdMs = 15_000,
        seeds = listOf("10.0.0.5:9999"),
        peers = listOf(
            PeerDiagnostics(
                addr = "10.0.0.2:3137",
                nick = "bob",
                lastSeenMs = now - 1_000,
                firstSeenMs = now - 60_000,
                packetsIn = 12,
                packetsOut = 14,
            ),
        ),
        store = StoreDiagnostics(keys = 3),
    )

    private fun titles(d: NodeDiagnostics) = healthChecks(d, now).map { it.title }

    private fun worst(d: NodeDiagnostics): Severity =
        healthChecks(d, now).maxOf { it.severity }

    @Test
    fun aWorkingNodeReportsHealthyFirst() {
        val checks = healthChecks(healthy(), now)
        assertEquals(Severity.OK, checks.first().severity)
        assertTrue(checks.first().detail.contains("1 peer"))
        assertFalse(checks.any { it.severity == Severity.WARN || it.severity == Severity.ERROR })
    }

    @Test
    fun authFailuresBlameTheKeyNotTheNetwork() {
        val d = healthy().copy(metrics = MetricsSnapshot(authFailures = 4))
        val check = healthChecks(d, now).first { it.title.contains("authentication") }
        assertEquals(Severity.ERROR, check.severity)
        assertTrue(check.detail.contains("pre-shared key"))
        assertTrue(check.detail.contains("deadbeef"))
    }

    @Test
    fun clockSkewIsReportedAsAClockProblem() {
        val d = healthy().copy(metrics = MetricsSnapshot(skewDrops = 2))
        val check = healthChecks(d, now).first { it.title.contains("clock skew") }
        assertEquals(Severity.ERROR, check.severity)
        assertTrue(check.detail.contains("clock"))
    }

    @Test
    fun aPeerThatNeverAnswersIsAnError() {
        val d = healthy().copy(
            peers = listOf(PeerDiagnostics(addr = "10.0.0.9:3137", packetsOut = 30, packetsIn = 0, lastSeenMs = now)),
        )
        val check = healthChecks(d, now).first { it.title.contains("never answered") }
        assertEquals(Severity.ERROR, check.severity)
        assertTrue(check.detail.contains("10.0.0.9:3137"))
    }

    @Test
    fun aPeerHalfWayToEvictionIsAWarning() {
        val d = healthy().copy(
            peers = listOf(
                PeerDiagnostics(addr = "10.0.0.2:3137", nick = "bob", lastSeenMs = now - 10_000, packetsIn = 5, packetsOut = 5),
            ),
        )
        val check = healthChecks(d, now).first { it.title.contains("going stale") }
        assertEquals(Severity.WARN, check.severity)
        assertTrue(check.detail.contains("bob"))
    }

    @Test
    fun noPeersWithNoSeedsExplainsHowToJoin() {
        val d = healthy().copy(peers = emptyList(), seeds = emptyList())
        val check = healthChecks(d, now).first { it.title == "No peers and no seeds" }
        assertEquals(Severity.WARN, check.severity)
        assertTrue(check.detail.contains("seed"))
    }

    @Test
    fun noPeersWithSeedsBlamesTheSeeds() {
        val d = healthy().copy(peers = emptyList())
        val check = healthChecks(d, now).first { it.title.contains("No peers learned") }
        assertTrue(check.detail.contains("10.0.0.5:9999"))
    }

    @Test
    fun advertisingLoopbackIsCalledOut() {
        val d = healthy().copy(addr = "127.0.0.1:3137")
        assertTrue(titles(d).any { it.contains("loopback") })
        assertEquals(Severity.WARN, worst(d))
    }

    @Test
    fun aMissingStreamListenerIsReportedWhileRunning() {
        val d = healthy().copy(streamListener = false)
        assertTrue(titles(d).any { it.contains("stream listener") })
        // Stopped, the port is not held at all, so there is nothing to report.
        assertFalse(titles(d.copy(running = false)).any { it.contains("stream listener") })
    }

    @Test
    fun aPartitionedClusterIsAnError() {
        val topology = TopologyBuilder.build(
            selfAddr = "10.0.0.1:3137",
            selfNick = "phone",
            peers = listOf(Peer("10.0.0.2:3137", lastSeenMs = now)),
            views = mapOf(
                "10.0.0.8:3137" to GossipView(listOf("10.0.0.9:3137"), atMs = now),
                "10.0.0.9:3137" to GossipView(listOf("10.0.0.8:3137"), atMs = now),
            ),
            nowMs = now,
        )
        val d = healthy().copy(topology = topology)
        val check = healthChecks(d, now).first { it.title.contains("partitioned") }
        assertEquals(Severity.ERROR, check.severity)
        assertTrue(check.detail.contains("diverges"))
    }

    @Test
    fun aStoppedNodeSaysSoAndNothingElseAlarming() {
        val d = healthy().copy(running = false, peers = emptyList())
        val checks = healthChecks(d, now)
        assertTrue(checks.any { it.title == "Node stopped" })
        assertFalse(checks.any { it.severity == Severity.ERROR })
    }

    @Test
    fun missingKeyIsInformationalNotAFailure() {
        val d = healthy().copy(pskSet = false)
        val check = healthChecks(d, now).first { it.title.contains("pre-shared key") }
        assertEquals(Severity.INFO, check.severity)
    }

    @Test
    fun strangeSourcesArePointedAtTheAdvertiseHost() {
        val d = healthy().copy(
            strangers = listOf(PeerDiagnostics(addr = "10.0.0.7:5000", known = false, packetsIn = 3)),
        )
        val check = healthChecks(d, now).first { it.title.contains("without being a peer") }
        assertTrue(check.detail.contains("10.0.0.7:5000"))
    }

    /**
     * "3 send error(s)" sent me looking at Wi-Fi for an hour. The datagrams were
     * never handed to the network at all — they were sent from the thread that
     * runs the UI, which Android refuses — so the check has to say that rather
     * than offer the network as the likely cause.
     */
    @Test
    fun aSendRefusedByTheUiThreadIsNotBlamedOnTheNetwork() {
        val d = healthy().copy(
            metrics = MetricsSnapshot(
                sendErrors = 3,
                lastSendError = SendFailure(
                    addr = "10.0.0.2:3137",
                    cause = "NetworkOnMainThreadException",
                ),
            ),
        )
        val check = healthChecks(d, now).first { it.title.contains("send error") }

        assertEquals("an app bug is worse than a flaky link", Severity.ERROR, check.severity)
        assertTrue(check.detail.contains("UI thread"))
        assertTrue("it should name the peer it could not reach", check.detail.contains("10.0.0.2:3137"))
        assertFalse("nothing here points at Wi-Fi", check.detail.contains("Wi-Fi"))
    }

    @Test
    fun anOrdinarySendFailureStillNamesWhatItWas() {
        val d = healthy().copy(
            metrics = MetricsSnapshot(
                sendErrors = 1,
                lastSendError = SendFailure(
                    addr = "10.0.0.9:3137",
                    cause = "SocketException",
                    message = "Network is unreachable",
                ),
            ),
        )
        val check = healthChecks(d, now).first { it.title.contains("send error") }

        assertEquals(Severity.WARN, check.severity)
        assertTrue(check.detail.contains("SocketException"))
        assertTrue(check.detail.contains("Network is unreachable"))
    }

    /** Older snapshots have no recorded cause; the check must still read sensibly. */
    @Test
    fun sendErrorsWithNoRecordedCauseFallBackToTheOldWording() {
        val d = healthy().copy(metrics = MetricsSnapshot(sendErrors = 2))
        val check = healthChecks(d, now).first { it.title.contains("send error") }

        assertEquals(Severity.WARN, check.severity)
        assertTrue(check.detail.contains("Wi-Fi"))
    }

    @Test
    fun theReportCarriesTheFactsAnotherPersonWouldAskFor() {
        val report = healthy().asReport(now)
        assertTrue(report.contains("10.0.0.1:3137"))
        assertTrue(report.contains("home"))
        assertTrue(report.contains("deadbeef"))
        assertTrue(report.contains("peers (1)"))
        assertTrue(report.contains("10.0.0.2:3137"))
        assertTrue(report.contains("checks"))
    }

    /**
     * An oversized frame and an unroutable peer are both "send error", and the
     * advice is opposite. Blaming Wi-Fi for the first is what the live run
     * found on the Go node: the counter said "message too long" and the prose
     * said to go and check a route that was perfectly fine.
     */
    @Test
    fun anOversizedFrameIsNotBlamedOnTheNetwork() {
        val d = healthy().copy(
            metrics = MetricsSnapshot(
                sendErrors = 7,
                lastSendError = SendFailure(
                    addr = "10.0.0.4:3137",
                    cause = "PacketTooLargeException",
                    message = "packet of 200000 bytes exceeds the 65507-byte datagram payload",
                ),
            ),
        )
        val check = healthChecks(d, now).first { it.title.contains("send error") }

        assertFalse("an oversized frame was blamed on Wi-Fi", check.detail.contains("Wi-Fi"))
        for (want in listOf("too large for one datagram", "stream", "divergent")) {
            assertTrue("detail should mention \"$want\": ${check.detail}", check.detail.contains(want))
        }
    }

    /**
     * With no stream listener the oversized frame had nowhere to go, and that
     * is the actionable half: the two checks have to point at each other
     * rather than leave the reader to join them up.
     */
    @Test
    fun anOversizedFrameWithNoStreamNamesTheListener() {
        val d = healthy().copy(
            streamListener = false,
            metrics = MetricsSnapshot(
                sendErrors = 1,
                lastSendError = SendFailure(addr = "10.0.0.4:3137", cause = "PacketTooLargeException"),
            ),
        )
        val checks = healthChecks(d, now)

        assertTrue(checks.first { it.title.contains("send error") }.detail.contains("no stream listener"))
        assertTrue(checks.first { it.title.contains("No stream listener") }.detail.contains("oversized frame"))
    }


    /**
     * A node whose store limits are turning peers away is the one node in a
     * cluster that cannot gossip its way back to agreement, and it used to be
     * the one reporting "Node looks healthy": the refusals were counted as
     * stale rejections, which no rule looks at because staleness is normal.
     */
    @Test
    fun refusedRemoteUpdatesAreAnError() {
        val d = healthy().copy(metrics = MetricsSnapshot(kvRefused = 189))
        val checks = healthChecks(d, now)
        val found = checks.firstOrNull { it.title.contains("refused") }
        assertNotNull("no check named the refusals: $checks", found)
        assertEquals(Severity.ERROR, found!!.severity)
        assertTrue(found.title.contains("189"))
        assertTrue("waiting does not fix it", found.detail.contains("not stale"))
        assertTrue(found.detail.contains("divergent"))
    }

    /**
     * Stale rejections stay unremarkable: they are last-write-wins working,
     * and a rule that fired on them would cry wolf on every healthy cluster.
     */
    @Test
    fun staleRejectionsAreNotAFinding() {
        val d = healthy().copy(metrics = MetricsSnapshot(kvRejectedStale = 5_000))
        val checks = healthChecks(d, now)
        assertTrue(
            "stale rejections alone made the node unhealthy: $checks",
            checks.none { it.severity == Severity.ERROR || it.severity == Severity.WARN },
        )
    }

    /**
     * Both guards refuse the frame, so nothing is broken — but neither has a
     * legitimate cause, and these are exactly the frames that used to raise an
     * author's sequence out of reach and silence them for good.
     */
    @Test
    fun malformedChatRepairIsReported() {
        val d = healthy().copy(
            metrics = MetricsSnapshot(chatUnsolicited = 3, chatSelfAttributed = 1),
        )
        val found = healthChecks(d, now).firstOrNull { it.title.contains("malformed repair") }
        assertNotNull("no check named the refused frames", found)
        assertEquals(Severity.WARN, found!!.severity)
        assertTrue(found.title.contains("4"))
        assertTrue(found.detail.contains("nobody asked for"))
        assertTrue(found.detail.contains("claiming to be"))
    }

    @Test
    fun aCleanClusterReportsNoMalformedRepair() {
        assertTrue(healthChecks(healthy(), now).none { it.title.contains("malformed repair") })
    }

    /**
     * Two nodes sharing an identity is the one divergence replication cannot
     * repair, so it has to be said out loud. Measured on a live cluster: the
     * two stayed split on a shared key for as long as they ran, and every tool
     * reported the node healthy.
     */
    @Test
    fun `a peer using this node's identity is an error that names it`() {
        val d = healthy().copy(
            nodeId = "00000000-dead-beef",
            impostors = listOf("192.168.18.79:3142"),
        )
        val check = healthChecks(d, now).first { it.title.contains("using this node's identity") }
        assertEquals(Severity.ERROR, check.severity)
        assertTrue(check.title.contains("192.168.18.79:3142"))
        assertTrue(check.detail.contains("state file"))
    }

    @Test
    fun `a single address change is not reported as a shared identity`() {
        val d = healthy().copy(
            idConflicts = listOf(IdConflict("peer-id", listOf("a:1", "b:1"), flips = 1, at = 0)),
        )
        assertTrue(
            "a node changing address was reported as a conflict",
            healthChecks(d, now).none { it.title.contains("is claimed by") },
        )
    }

    @Test
    fun `one id claimed by two live addresses is an error naming both`() {
        val d = healthy().copy(
            idConflicts = listOf(
                IdConflict(
                    "00000000-dead-beef",
                    listOf("192.168.18.79:3141", "192.168.18.79:3142"),
                    flips = 12,
                    at = 0,
                ),
            ),
        )
        val check = healthChecks(d, now).first { it.title.contains("is claimed by") }
        assertEquals(Severity.ERROR, check.severity)
        assertTrue(check.title.contains("3141"))
        assertTrue(check.title.contains("3142"))
    }

    /**
     * Tombstone GC is the one setting that can silently undo a delete: a node
     * away longer than the age comes back still holding the value and pushes
     * it back (DESIGN.md 5.5). Demonstrated on a live pair — the delete was
     * undone on both nodes and the consistency check reported them converged.
     *
     * This port exposes the age as a field in Settings, so recommending it
     * without saying what bounds it is telling a user to lose deletes. The
     * caveat was present in the Go port and missing from both GUI ports.
     */
    @Test
    fun `the tombstone advice says what bounds the GC age, not only the benefit`() {
        val d = healthy().let { it.copy(store = it.store.copy(tombstones = 7)) }
        val check = healthChecks(d, now).first { it.title.contains("tombstone(s) kept forever") }
        assertTrue(
            "the advice does not say what the age must exceed: ${check.detail}",
            check.detail.contains("longest"),
        )
    }
}
