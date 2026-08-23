package com.nexusriot.rezoagwe.core

import kotlin.math.PI
import kotlin.math.cos
import kotlin.math.sin

/** What a node is to us: ourselves, a peer we talk to, or one we only heard about. */
enum class NodeRole { SELF, DIRECT, INDIRECT }

/**
 * How much we know about a link.
 *
 * A gossip packet carries the sender's own peer list, so a link can be asserted
 * by one end only. Telling that apart from a link both ends agree on is what
 * makes a half-open cluster visible instead of looking healthy.
 */
enum class LinkKind { DIRECT, MUTUAL, OBSERVED }

data class GraphNode(
    val addr: String,
    val label: String,
    val role: NodeRole,
    val lastSeenMs: Long = 0,
    val degree: Int = 0,
    /** Peers this node advertised the last time it gossiped, -1 when it never has. */
    val advertised: Int = -1,
)

data class GraphLink(val a: String, val b: String, val kind: LinkKind)

/** One peer's advertised view of the cluster, as of when it last gossiped to us. */
data class GossipView(
    val peers: List<String> = emptyList(),
    val atMs: Long = 0,
)

data class GraphPoint(val x: Float, val y: Float)

/**
 * The cluster as this node can see it: who exists, and who claims to talk to whom.
 *
 * Everything here is derived from packets already exchanged — no extra protocol —
 * so the picture is exactly as complete as gossip has made it, which is itself
 * the diagnostic.
 */
data class Topology(
    val nodes: List<GraphNode> = emptyList(),
    val links: List<GraphLink> = emptyList(),
    val generatedAtMs: Long = 0,
) {
    val self: GraphNode? get() = nodes.firstOrNull { it.role == NodeRole.SELF }

    fun node(addr: String): GraphNode? = nodes.firstOrNull { it.addr == addr }

    fun neighboursOf(addr: String): List<String> =
        links.mapNotNull {
            when (addr) {
                it.a -> it.b
                it.b -> it.a
                else -> null
            }
        }.distinct().sorted()

    /**
     * Groups of nodes that can reach each other through known links. More than
     * one group means the cluster is split: each half converges internally and
     * silently diverges from the other, which is the failure a KV store cannot
     * report on its own.
     */
    fun components(): List<List<String>> {
        val remaining = nodes.map { it.addr }.toMutableSet()
        val adjacency = HashMap<String, MutableSet<String>>()
        for (link in links) {
            if (link.a == link.b) continue
            adjacency.getOrPut(link.a) { mutableSetOf() }.add(link.b)
            adjacency.getOrPut(link.b) { mutableSetOf() }.add(link.a)
        }
        val groups = mutableListOf<List<String>>()
        while (remaining.isNotEmpty()) {
            val seed = remaining.first()
            val group = mutableListOf<String>()
            val queue = ArrayDeque<String>()
            queue.add(seed)
            remaining.remove(seed)
            while (queue.isNotEmpty()) {
                val current = queue.removeFirst()
                group.add(current)
                for (next in adjacency[current].orEmpty()) {
                    if (remaining.remove(next)) queue.add(next)
                }
            }
            groups.add(group.sorted())
        }
        return groups.sortedWith(compareByDescending<List<String>> { it.size }.thenBy { it.firstOrNull() ?: "" })
    }

    /** Links only one end has told us about; each is a peer relationship we cannot confirm. */
    fun unconfirmed(): List<GraphLink> = links.filter { it.kind == LinkKind.OBSERVED }
}

object TopologyBuilder {
    /**
     * Assembles the graph from what the engine already holds: our own peer table
     * and the peer lists our peers have gossiped.
     */
    fun build(
        selfAddr: String,
        selfNick: String,
        peers: List<Peer>,
        views: Map<String, GossipView>,
        nowMs: Long,
    ): Topology {
        val self = normalize(selfAddr)
        val direct = peers.associateBy { normalize(it.addr) }

        val claims = HashMap<String, MutableSet<String>>()
        fun claim(from: String, to: String) {
            if (from.isEmpty() || to.isEmpty() || from == to) return
            claims.getOrPut(from) { mutableSetOf() }.add(to)
        }
        for (peer in direct.keys) claim(self, peer)
        for ((owner, view) in views) {
            val from = normalize(owner)
            for (target in view.peers) claim(from, normalize(target))
        }

        val addrs = LinkedHashSet<String>()
        addrs.add(self)
        addrs.addAll(direct.keys)
        for ((from, targets) in claims) {
            addrs.add(from)
            addrs.addAll(targets)
        }

        val links = mutableListOf<GraphLink>()
        val seenPairs = HashSet<String>()
        for ((from, targets) in claims) {
            for (to in targets) {
                val pair = if (from < to) "$from|$to" else "$to|$from"
                if (!seenPairs.add(pair)) continue
                val bothWays = claims[to]?.contains(from) == true
                val touchesSelf = from == self || to == self
                links += GraphLink(
                    a = from,
                    b = to,
                    kind = when {
                        touchesSelf -> LinkKind.DIRECT
                        bothWays -> LinkKind.MUTUAL
                        else -> LinkKind.OBSERVED
                    },
                )
            }
        }

        val degrees = HashMap<String, Int>()
        for (link in links) {
            degrees[link.a] = (degrees[link.a] ?: 0) + 1
            degrees[link.b] = (degrees[link.b] ?: 0) + 1
        }

        val viewsByAddr = views.mapKeys { normalize(it.key) }
        // For a node we have never exchanged packets with, "last seen" is the last
        // time anyone mentioned it — the only freshness we have for it.
        val mentionedAt = HashMap<String, Long>()
        for ((owner, view) in viewsByAddr) {
            for (target in view.peers) {
                val at = maxOf(mentionedAt[normalize(target)] ?: 0, view.atMs)
                mentionedAt[normalize(target)] = at
            }
            mentionedAt[owner] = maxOf(mentionedAt[owner] ?: 0, view.atMs)
        }
        val nodes = addrs.map { addr ->
            val peer = direct[addr]
            val view = viewsByAddr[addr]
            GraphNode(
                addr = addr,
                label = when {
                    addr == self -> selfNick.ifEmpty { "this node" }
                    peer != null && peer.nick.isNotEmpty() -> peer.nick
                    else -> addr
                },
                role = when {
                    addr == self -> NodeRole.SELF
                    peer != null -> NodeRole.DIRECT
                    else -> NodeRole.INDIRECT
                },
                lastSeenMs = peer?.lastSeenMs ?: view?.atMs ?: mentionedAt[addr] ?: 0,
                degree = degrees[addr] ?: 0,
                advertised = view?.peers?.size ?: -1,
            )
        }.sortedWith(compareBy({ it.role.ordinal }, { it.addr }))

        return Topology(nodes = nodes, links = links, generatedAtMs = nowMs)
    }

    private fun normalize(addr: String): String = NodeEngine.normalizeAddr(addr.trim())
}

/**
 * Places nodes for drawing: this node in the middle, peers on the first ring,
 * nodes we only heard about further out.
 *
 * Coordinates are normalised to `[-1, 1]` so the layout is independent of the
 * canvas, and the order is derived from the sorted address so a node does not
 * jump around between frames.
 */
object GraphLayout {
    /** Nodes per shell before a further one is started, so labels stay readable. */
    private const val SHELL_CAPACITY = 10

    /** Where the innermost shell sits, leaving the middle to this node. */
    private const val INNER_RADIUS = 0.45f

    fun place(topology: Topology): Map<String, GraphPoint> {
        val result = LinkedHashMap<String, GraphPoint>()
        val self = topology.nodes.filter { it.role == NodeRole.SELF }.map { it.addr }
        val direct = topology.nodes.filter { it.role == NodeRole.DIRECT }.map { it.addr }.sorted()
        val indirect = topology.nodes.filter { it.role == NodeRole.INDIRECT }.map { it.addr }.sorted()

        self.forEach { result[it] = GraphPoint(0f, 0f) }

        // Peers first, then the nodes we only heard about: shells never mix the
        // two, so distance from the middle means "how far from us", not just
        // "how many nodes fitted".
        val shells = chunk(direct) + chunk(indirect)
        shells.forEachIndexed { index, shell ->
            val radius = radiusOf(index, shells.size)
            // Every other shell is rotated half a slot so a node never sits
            // exactly behind the one on the shell inside it.
            val offset = if (index % 2 == 0) 0.0 else PI / shell.size
            shell.forEachIndexed { slot, addr ->
                val angle = -PI / 2 + offset + 2 * PI * slot / shell.size
                result[addr] = GraphPoint((radius * cos(angle)).toFloat(), (radius * sin(angle)).toFloat())
            }
        }
        return result
    }

    private fun chunk(addrs: List<String>): List<List<String>> =
        if (addrs.isEmpty()) emptyList() else addrs.chunked(SHELL_CAPACITY)

    private fun radiusOf(index: Int, shells: Int): Float = when {
        shells <= 1 -> (INNER_RADIUS + 1f) / 2
        else -> INNER_RADIUS + (1f - INNER_RADIUS) * index / (shells - 1)
    }
}
