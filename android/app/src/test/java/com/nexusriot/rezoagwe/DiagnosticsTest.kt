package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.GossipView
import com.nexusriot.rezoagwe.core.MetricsSnapshot
import com.nexusriot.rezoagwe.core.NodeDiagnostics
import com.nexusriot.rezoagwe.core.Peer
import com.nexusriot.rezoagwe.core.PeerDiagnostics
import com.nexusriot.rezoagwe.core.SendFailure
import com.nexusriot.rezoagwe.core.SEND_ERROR_RECENT_WINDOW_MS
import com.nexusriot.rezoagwe.core.Severity
import com.nexusriot.rezoagwe.core.StoreDiagnostics
import com.nexusriot.rezoagwe.core.TopologyBuilder
import com.nexusriot.rezoagwe.core.asReport
import com.nexusriot.rezoagwe.core.DeviceReport
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
     * The send-error counter only climbs, and on a phone the two questions
     * "did anything ever fail?" and "is anything wrong now?" come apart
     * constantly: Wi-Fi power saving, roaming between access points and Doze
     * each make a send fail for a moment. Observed on hardware — a tablet
     * replicating happily in a four-node cluster still warned about failures
     * minutes old, beside Go and desktop nodes calling the same cluster
     * healthy. A warning that cannot clear is one the reader learns to scroll
     * past, which costs the real ones their only chance of being read.
     */
    @Test
    fun aSendErrorAgesOutOfTheWarnings() {
        val d = healthy().copy(
            metrics = MetricsSnapshot(
                sendErrors = 2,
                lastSendError = SendFailure(
                    addr = "10.0.0.2:3137",
                    cause = "ErrnoException",
                    message = "sendto failed: ENETUNREACH (Network is unreachable)",
                    atMs = now - SEND_ERROR_RECENT_WINDOW_MS - 1_000,
                ),
            ),
        )
        val checks = healthChecks(d, now)
        val check = checks.first { it.title.contains("send error") }

        assertEquals("an old send error still warns", Severity.INFO, check.severity)
        assertTrue("the detail does not say it is history", check.detail.contains("history"))
        assertTrue("the count was dropped", check.title.contains("2 send error"))
        assertTrue(
            "the node still does not read as healthy",
            checks.none { it.severity == Severity.WARN || it.severity == Severity.ERROR },
        )
    }

    /** The fix must not silence the case the check exists for. */
    @Test
    fun aSendErrorHappeningNowIsStillAWarning() {
        val d = healthy().copy(
            metrics = MetricsSnapshot(
                sendErrors = 1,
                lastSendError = SendFailure(
                    addr = "10.0.0.2:3137",
                    cause = "ErrnoException",
                    message = "sendto failed: ENETUNREACH (Network is unreachable)",
                    atMs = now - 1_000,
                ),
            ),
        )
        val check = healthChecks(d, now).first { it.title.contains("send error") }
        assertEquals(Severity.WARN, check.severity)
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

    /**
     * `deviceReport` is collected because "the usual causes are not in the
     * protocol at all: no network, a metered link, or Doze" — and then it was
     * read by nothing. With Wi-Fi off on a tablet the Device card said
     * `network none` while Health blamed "a peer address that no longer routes,
     * or Wi-Fi dropping while the node stays up" and told the reader to check a
     * bootstrap that was perfectly fine. The phone's own state has to be an
     * input to the checks, not just something to display.
     */
    @Test
    fun `no network is named as the cause rather than guessed at`() {
        val d = healthy().copy(
            peers = emptyList(),
            metrics = MetricsSnapshot(
                sendErrors = 22,
                lastSendError = SendFailure(addr = "10.0.0.5:9999", message = "ENETUNREACH"),
            ),
        )
        val offline = DeviceReport(transport = "none")

        val checks = healthChecks(d, now, offline)
        val titles = checks.map { it.title }
        assertTrue("no check named the missing network: $titles", titles.any { it.contains("no network") })
        assertEquals(
            "a device with no network is an error, not a warning",
            Severity.ERROR,
            checks.first { it.title.contains("no network") }.severity,
        )

        // And the checks that used to guess now defer to it rather than sending
        // the reader after the bootstrap and the key.
        val seeds = checks.first { it.title.contains("seed") }
        assertTrue("the seed check still blames the configuration: ${seeds.detail}",
            seeds.detail.contains("no network"))
        assertFalse(seeds.detail.contains("cluster name"))

        val sends = checks.first { it.title.contains("send error") }
        assertTrue("the send-error check still guesses: ${sends.detail}",
            sends.detail.contains("no network"))
    }

    /** On a device that *is* connected, nothing changes. */
    @Test
    fun `a connected device raises no network check`() {
        val online = DeviceReport(transport = "Wi-Fi", validated = true)
        assertFalse(healthChecks(healthy(), now, online).any { it.title.contains("no network") })
        assertFalse(healthChecks(healthy(), now, null).any { it.title.contains("no network") })
    }

    /** Doze is the Android-specific reason a node stops gossiping with nothing wrong in the protocol. */
    @Test
    fun `battery saver is raised, since it stops the gossip timers`() {
        val saving = DeviceReport(transport = "Wi-Fi", powerSaveMode = true)
        val check = healthChecks(healthy(), now, saving).firstOrNull { it.title.contains("Battery saver") }
        assertNotNull("battery saver was collected and never surfaced", check)
        assertEquals(Severity.WARN, check!!.severity)
    }

    /**
     * Counted since the beginning and explained by nothing: on a tablet with a
     * wrong key, five stream errors against an unreachable seed raised not one
     * word. The stream is the only path for a value too big for a datagram and
     * for a whole-store sync on join.
     */
    @Test
    fun `stream errors are explained, not just counted`() {
        val d = healthy().copy(metrics = MetricsSnapshot(streamErrors = 5))
        val check = healthChecks(d, now).firstOrNull { it.title.contains("stream (TCP) error") }
        assertNotNull("stream errors raised nothing", check)
        assertEquals(Severity.WARN, check!!.severity)
        assertTrue(check.detail.contains("firewall that passes UDP and blocks TCP"))

        assertFalse(healthChecks(healthy(), now).any { it.title.contains("stream (TCP)") })
    }

    /**
     * A state file that cannot be read must not stop the node — losing the
     * store is bad and refusing to start is worse — but the node starts empty
     * and with a new identity, and then rewrites the file that was the
     * evidence. This port swallowed the exception outright, so none of that
     * reached any screen.
     */
    @Test
    fun anUnreadableStateFileIsReported() {
        val checks = healthChecks(
            healthy().copy(
                dataFile = "/data/user/0/com.nexusriot.rezoagwe/files/node.json",
                loadError = "Unexpected JSON token at offset 7",
            ),
            now,
        )
        val check = checks.first { it.title == "State file could not be read" }
        assertEquals(Severity.ERROR, check.severity)
        assertTrue(check.detail.contains("node.json"))
        assertTrue(check.detail.contains("Unexpected JSON token at offset 7"))
        // The two consequences nobody guesses from "the file was bad".
        assertTrue(check.detail.contains("new identity"))
        assertTrue(check.detail.contains("different author"))
    }

    /**
     * Every node has a state file, so a finding for the ordinary case would put
     * an error on every report and get the real one ignored.
     */
    @Test
    fun aStateFileThatReadsFineIsNotReported() {
        assertTrue(
            healthChecks(healthy(), now).none { it.title == "State file could not be read" },
        )
    }

    /**
     * With the radio off, the address a rendezvous saw before the outage says
     * where this node *used to be*.
     *
     * A device with no network advertises loopback because it has nowhere else
     * to advertise. Reporting the stale observation then tells someone whose
     * Wi-Fi has dropped to hard-code the address they had — wrong now, and
     * still wrong on the next network they join. Measured on a tablet with
     * Wi-Fi switched off mid-round: the useful advice ("reconnect") had been
     * replaced by the useless one.
     */
    @Test
    fun `a stale observed address is not reported while the device is offline`() {
        val d = healthy().copy(
            addr = "127.0.0.1:3137",
            advertiseMismatch = "192.168.18.60:48812",
        )
        val checks = healthChecks(d, now, DeviceReport(transport = "none"))

        assertFalse(
            "the rendezvous saw that address before the radio went off",
            checks.any { it.title.contains("does not see this node at") },
        )
        // And the rule that gives the right answer is back.
        val loopback = checks.first { it.title.contains("loopback") }
        assertTrue(loopback.detail.contains("Connect to a network"))
    }

    /** Connected, the observation is current and still outranks the guess. */
    @Test
    fun `an observed address is reported while the device is connected`() {
        val d = healthy().copy(
            addr = "10.9.9.9:3137",
            advertiseMismatch = "192.168.18.60:48812",
        )
        val checks = healthChecks(d, now, DeviceReport(transport = "Wi-Fi", validated = true))

        val check = checks.first { it.title.contains("does not see this node at") }
        assertEquals(Severity.ERROR, check.severity)
        assertTrue(check.detail.contains("192.168.18.60"))
    }

    /**
     * GC off warns that a node away too long will undo a delete. GC on is that
     * same hazard, configured — and said nothing at all, which is the
     * asymmetry: the advice appeared only while it was not being followed.
     *
     * It cannot be reported after the fact. A node that reclaimed a tombstone
     * and then accepted a set for that key cannot tell a resurrection from
     * someone writing it again, so the only honest moment is while the age is
     * configured. Reproduced on two nodes with an 8s age: the delete came back
     * on every replica and the consistency check called it converged.
     */
    @Test
    fun `tombstone GC names the exposure it creates`() {
        val checks = healthChecks(healthy().copy(tombstoneTtlSec = 30), now)
        val check = checks.first { it.title.contains("Tombstones are reclaimed") }
        assertEquals(Severity.INFO, check.severity)
        assertTrue(check.title.contains("30s"))
        assertTrue(check.detail.contains("away for longer"))
        assertTrue(check.detail.contains("undone"))
    }

    @Test
    fun `GC off says the opposite and not both`() {
        val d = healthy().copy(
            tombstoneTtlSec = 0,
            store = healthy().store.copy(tombstones = 3),
        )
        val checks = healthChecks(d, now)
        assertTrue(checks.any { it.title.contains("kept forever") })
        assertFalse(checks.any { it.title.contains("Tombstones are reclaimed") })
    }

    /** The exposure is the setting, not the tombstones it happens to hold. */
    @Test
    fun `the GC exposure is reported before anything is deleted`() {
        val d = healthy().copy(tombstoneTtlSec = 3600, store = healthy().store.copy(tombstones = 0))
        assertTrue(healthChecks(d, now).any { it.title.contains("Tombstones are reclaimed") })
    }
}
