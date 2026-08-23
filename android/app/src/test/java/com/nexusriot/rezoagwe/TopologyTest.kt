package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.GossipView
import com.nexusriot.rezoagwe.core.GraphLayout
import com.nexusriot.rezoagwe.core.LinkKind
import com.nexusriot.rezoagwe.core.NodeRole
import com.nexusriot.rezoagwe.core.Peer
import com.nexusriot.rezoagwe.core.TopologyBuilder
import kotlin.math.abs
import kotlin.math.hypot
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The graph is derived, not received: every claim it makes about the cluster comes
 * from peer lists gossip happened to carry. These pin what that derivation is
 * allowed to conclude.
 */
class TopologyTest {
    private val self = "10.0.0.1:3137"

    private fun peer(addr: String, nick: String = "", lastSeen: Long = 1_000) =
        Peer(addr = addr, nick = nick, lastSeenMs = lastSeen, firstSeenMs = 500)

    @Test
    fun aLoneNodeIsItsOwnGraph() {
        val t = TopologyBuilder.build(self, "phone", emptyList(), emptyMap(), 1_000)
        assertEquals(1, t.nodes.size)
        assertEquals(NodeRole.SELF, t.nodes.first().role)
        assertEquals("phone", t.nodes.first().label)
        assertTrue(t.links.isEmpty())
        assertEquals(1, t.components().size)
    }

    @Test
    fun peersAreDirectAndLinkedToUs() {
        val t = TopologyBuilder.build(
            self,
            "phone",
            listOf(peer("10.0.0.2:3137", "bob")),
            emptyMap(),
            1_000,
        )
        assertEquals(2, t.nodes.size)
        val bob = t.node("10.0.0.2:3137")
        assertNotNull(bob)
        assertEquals(NodeRole.DIRECT, bob!!.role)
        assertEquals("bob", bob.label)
        assertEquals(1, t.links.size)
        assertEquals(LinkKind.DIRECT, t.links.first().kind)
    }

    @Test
    fun aGossipedAddressWeDoNotPeerWithIsIndirect() {
        // The case that makes the graph worth drawing: bob still lists carol, but
        // this node has evicted her, so she is known without being reachable.
        val t = TopologyBuilder.build(
            self,
            "phone",
            listOf(peer("10.0.0.2:3137", "bob")),
            mapOf("10.0.0.2:3137" to GossipView(listOf("10.0.0.3:3137"), atMs = 900)),
            1_000,
        )
        val carol = t.node("10.0.0.3:3137")
        assertNotNull(carol)
        assertEquals(NodeRole.INDIRECT, carol!!.role)
        assertEquals("10.0.0.3:3137", carol.label)
        assertEquals(900, carol.lastSeenMs)
        // Only bob claims the link, so it stays unconfirmed.
        val link = t.links.single { it.a.contains("10.0.0.3") || it.b.contains("10.0.0.3") }
        assertEquals(LinkKind.OBSERVED, link.kind)
        assertEquals(1, t.unconfirmed().size)
    }

    @Test
    fun aLinkBothEndsClaimIsMutual() {
        val t = TopologyBuilder.build(
            self,
            "phone",
            listOf(peer("10.0.0.2:3137"), peer("10.0.0.3:3137")),
            mapOf(
                "10.0.0.2:3137" to GossipView(listOf(self, "10.0.0.3:3137"), atMs = 900),
                "10.0.0.3:3137" to GossipView(listOf(self, "10.0.0.2:3137"), atMs = 950),
            ),
            1_000,
        )
        val between = t.links.single {
            setOf(it.a, it.b) == setOf("10.0.0.2:3137", "10.0.0.3:3137")
        }
        assertEquals(LinkKind.MUTUAL, between.kind)
        assertTrue(t.unconfirmed().isEmpty())
        // Our own links are never deduced, so they outrank a gossiped claim.
        assertEquals(2, t.links.count { it.kind == LinkKind.DIRECT })
    }

    @Test
    fun degreesAndNeighboursCountEveryKnownLink() {
        val t = TopologyBuilder.build(
            self,
            "phone",
            listOf(peer("10.0.0.2:3137"), peer("10.0.0.3:3137")),
            mapOf("10.0.0.2:3137" to GossipView(listOf("10.0.0.3:3137"), atMs = 900)),
            1_000,
        )
        assertEquals(listOf("10.0.0.2:3137", "10.0.0.3:3137"), t.neighboursOf(self))
        assertEquals(2, t.node("10.0.0.2:3137")!!.degree)
    }

    @Test
    fun aSplitClusterShowsAsTwoComponents() {
        // We know of a pair that talks to each other and to nobody we can reach.
        val t = TopologyBuilder.build(
            self,
            "phone",
            listOf(peer("10.0.0.2:3137")),
            mapOf(
                "10.0.0.9:3137" to GossipView(listOf("10.0.0.8:3137"), atMs = 900),
                "10.0.0.8:3137" to GossipView(listOf("10.0.0.9:3137"), atMs = 900),
            ),
            1_000,
        )
        val components = t.components()
        assertEquals(2, components.size)
        assertEquals(2, components[0].size)
        assertTrue(components.any { it.containsAll(listOf("10.0.0.8:3137", "10.0.0.9:3137")) })
        assertTrue(components.any { it.containsAll(listOf(self, "10.0.0.2:3137")) })
    }

    @Test
    fun addressAliasesAreOneNode() {
        // A peer advertising the wildcard address is the same node as one on
        // loopback; two circles for one node would be a lie about the cluster size.
        val t = TopologyBuilder.build(
            "0.0.0.0:3137",
            "phone",
            listOf(peer("localhost:4000")),
            mapOf("127.0.0.1:4000" to GossipView(listOf("0.0.0.0:3137"), atMs = 900)),
            1_000,
        )
        assertEquals(2, t.nodes.size)
        assertEquals(1, t.links.size)
        assertNotNull(t.node("127.0.0.1:3137"))
        assertNull(t.node("0.0.0.0:3137"))
    }

    @Test
    fun ourOwnAddressNeverBecomesAPeer() {
        val t = TopologyBuilder.build(
            self,
            "phone",
            emptyList(),
            mapOf("10.0.0.2:3137" to GossipView(listOf(self), atMs = 900)),
            1_000,
        )
        assertEquals(NodeRole.SELF, t.node(self)!!.role)
        assertEquals(1, t.nodes.count { it.role == NodeRole.SELF })
    }

    @Test
    fun layoutPutsUsInTheMiddleAndPeersOnARing() {
        val peers = (2..6).map { peer("10.0.0.$it:3137") }
        val t = TopologyBuilder.build(self, "phone", peers, emptyMap(), 1_000)
        val places = GraphLayout.place(t)

        assertEquals(t.nodes.size, places.size)
        val centre = places.getValue(self)
        assertEquals(0f, centre.x, 0.0001f)
        assertEquals(0f, centre.y, 0.0001f)

        val radii = peers.map { hypot(places.getValue(it.addr).x, places.getValue(it.addr).y) }
        radii.forEach { assertEquals(radii.first(), it, 0.001f) }
        assertTrue("peers must sit off centre: $radii", radii.first() > 0.4f)
        assertTrue("peers must stay inside the canvas: $radii", radii.first() <= 1f)
    }

    @Test
    fun layoutSeparatesDirectPeersFromGossipedOnes() {
        val t = TopologyBuilder.build(
            self,
            "phone",
            listOf(peer("10.0.0.2:3137")),
            mapOf("10.0.0.2:3137" to GossipView(listOf("10.0.0.3:3137"), atMs = 900)),
            1_000,
        )
        val places = GraphLayout.place(t)
        val direct = hypot(places.getValue("10.0.0.2:3137").x, places.getValue("10.0.0.2:3137").y)
        val indirect = hypot(places.getValue("10.0.0.3:3137").x, places.getValue("10.0.0.3:3137").y)
        assertTrue("gossiped nodes belong further out: $direct vs $indirect", indirect > direct)
    }

    @Test
    fun layoutIsStableAndNeverOverlapsWithinAShell() {
        val peers = (2..9).map { peer("10.0.0.$it:3137") }
        val t = TopologyBuilder.build(self, "phone", peers, emptyMap(), 1_000)
        val first = GraphLayout.place(t)
        val second = GraphLayout.place(t)
        assertEquals(first, second)

        val points = peers.map { first.getValue(it.addr) }
        for (i in points.indices) {
            for (j in i + 1 until points.size) {
                val gap = hypot(points[i].x - points[j].x, points[i].y - points[j].y)
                assertTrue("nodes must not land on each other: $gap", gap > 0.05f)
            }
        }
    }

    @Test
    fun layoutHandlesMoreNodesThanOneShellHolds() {
        val peers = (1..24).map { peer("10.1.0.$it:3137") }
        val t = TopologyBuilder.build(self, "phone", peers, emptyMap(), 1_000)
        val places = GraphLayout.place(t)
        assertEquals(25, places.size)
        places.values.forEach {
            assertTrue("point off canvas: $it", hypot(it.x, it.y) <= 1.0001f)
            assertTrue("point is not a number: $it", !it.x.isNaN() && !it.y.isNaN())
        }
        // Three shells for 24 peers: the outermost has to reach the edge.
        val outermost = places.values.maxOf { hypot(it.x, it.y) }
        assertTrue("outer shell should use the canvas: $outermost", abs(outermost - 1f) < 0.001f)
    }
}
