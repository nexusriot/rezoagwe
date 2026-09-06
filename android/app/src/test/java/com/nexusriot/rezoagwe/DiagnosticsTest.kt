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
import com.nexusriot.rezoagwe.core.healthChecks
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
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
}
