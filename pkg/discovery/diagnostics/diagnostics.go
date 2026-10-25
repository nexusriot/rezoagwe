// Package diagnostics turns a node's counters into the short list of things
// actually wrong with it.
//
// Every rule here is a claim about the protocol — "auth failures mean a key
// mismatch", "one-way traffic is a firewall, not loss" — and a claim like that
// is worth a test, which is why the rules are a pure function of a snapshot
// rather than something the engine does to itself.
package diagnostics

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	"github.com/nexusriot/rezoagwe/pkg/discovery/topology"
	"github.com/nexusriot/rezoagwe/pkg/metrics"
)

// Severity is how much attention a finding deserves.
type Severity string

const (
	SeverityOK    Severity = "ok"
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// SendErrorRecentWindow is how long a send failure stays a warning.
//
// Gossip runs every few seconds, so a machine that has sent nothing failing in
// this long is one whose network came back. Past that the count is history: it
// stays on the screen, as information, instead of holding a node that works at
// "warning" forever.
const SendErrorRecentWindow = 2 * time.Minute

// Check is one finding.
type Check struct {
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
}

// Peer is one peer as the diagnostics see it: membership plus the traffic that
// actually flowed, which is the pair that separates "quiet" from "unreachable".
type Peer struct {
	Addr     string `json:"addr"`
	Nick     string `json:"nick,omitempty"`
	LastSeen int64  `json:"last_seen"`
	metrics.PeerCount
}

// Stranger is a source address that sent packets without being a peer.
type Stranger struct {
	Addr      string `json:"addr"`
	LastSeen  int64  `json:"last_seen"`
	PacketsIn uint64 `json:"packets_in"`
	Rejected  uint64 `json:"rejected"`
}

// ChatGap is one author's chat delivery record: lines still being asked for,
// lines given up on, and lines a backlog pull actually recovered.
type ChatGap struct {
	Addr        string `json:"addr"`
	Nick        string `json:"nick,omitempty"`
	Outstanding int    `json:"outstanding"`
	Lost        uint64 `json:"lost"`
	Recovered   uint64 `json:"recovered"`
}

// IDConflictFlips is how often one node id must change hands before it is
// reported. A node that genuinely moved address rebinds once and stays; two
// nodes sharing an identity rebind on every heartbeat either one sends.
const IDConflictFlips = 3

// shortID abbreviates a node id for a one-line title.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// IDConflict is one node id claimed by two different addresses.
type IDConflict struct {
	ID    string   `json:"id"`
	Addrs []string `json:"addrs"`
	Flips uint64   `json:"flips"`
	At    int64    `json:"at"`
}

// Snapshot is everything the rules read. It is a plain struct so a test can
// fabricate any cluster condition without running one.
type Snapshot struct {
	ReplicationWarnings []string                `json:"replication_warnings,omitempty"`
	Persistence         model.PersistenceHealth `json:"persistence"`
	Addr                string                  `json:"addr"`
	BindAddr            string                  `json:"bind_addr"`
	AdvertiseAddr       string                  `json:"advertise_addr,omitempty"`
	LocalIPv4           string                  `json:"local_ipv4,omitempty"`
	Nick                string                  `json:"nick"`
	NodeID              string                  `json:"node_id"`
	Cluster             string                  `json:"cluster"`
	KeyFingerprint      string                  `json:"key_fingerprint"`
	PSKSet              bool                    `json:"psk_set"`
	Version             string                  `json:"version,omitempty"`
	UptimeSec           int64                   `json:"uptime_sec"`
	StreamListener      bool                    `json:"stream_listener"`
	// MDNS reports whether LAN discovery is running, which changes what "no
	// peers" means and therefore what to do about it.
	MDNS bool `json:"mdns,omitempty"`
	// SeenAt is where a rendezvous service observed this node's packets coming
	// from, and AdvertiseMismatch is that address when it names a different
	// host from the one the node advertises. Empty means they agree, or that
	// no bootstrap has answered yet.
	SeenAt            string `json:"seen_at,omitempty"`
	AdvertiseMismatch string `json:"advertise_mismatch,omitempty"`
	// DataPath is the state file, and LoadError why it could not be read.
	// A node that cannot read its state file starts empty and with a new
	// identity, then overwrites the file that was the evidence.
	DataPath  string     `json:"data_path,omitempty"`
	LoadError string     `json:"load_error,omitempty"`
	Seeds     []string   `json:"seeds"`
	Peers     []Peer     `json:"peers"`
	ChatGaps  []ChatGap  `json:"chat_gaps,omitempty"`
	Strangers []Stranger `json:"strangers"`
	// IDConflicts are node ids seen claimed by more than one address, and
	// Impostors peer addresses seen claiming this node's own id.
	IDConflicts []IDConflict     `json:"id_conflicts,omitempty"`
	Impostors   []string         `json:"impostors,omitempty"`
	Metrics     metrics.Snapshot `json:"-"`
	Gauges      metrics.Gauges   `json:"gauges"`
	Topology    topology.Graph   `json:"topology"`

	GossipInterval    time.Duration `json:"-"`
	HeartbeatInterval time.Duration `json:"-"`
	EvictThreshold    time.Duration `json:"-"`
	TombstoneTTL      time.Duration `json:"-"`
}

// Checks runs every rule and returns the findings, worst first.
func Checks(d Snapshot, now time.Time) []Check {
	var out []Check
	add := func(s Severity, title, detail string) {
		out = append(out, Check{Severity: s, Title: title, Detail: detail})
	}
	for _, warning := range d.ReplicationWarnings {
		add(SeverityWarn, "Replication limits differ", warning)
	}
	if d.Persistence.Enabled {
		p := d.Persistence
		detail := persistenceDetail(p.Mode, p.Durable, p.LastSuccess, p.Pending, p.RecoveryFile)
		if p.Error != "" {
			add(SeverityError, "State is not safely saved", p.Error+". "+detail)
		} else if p.Pending {
			add(SeverityInfo, "State save pending", detail)
		} else {
			add(SeverityOK, "State saved", detail)
		}
	}
	m := d.Metrics

	// Reported before anything else that could be a *consequence* of it: a node
	// that started empty will also look like one that has fallen behind.
	if d.LoadError != "" {
		add(SeverityError, "State file could not be read",
			fmt.Sprintf("%s did not parse (%s), so this node started with an empty store and a new "+
				"identity. The original is retained for recovery, or further saves are blocked if retention failed. Its earlier writes "+
				"are outranked until anti-entropy refills it from a peer, and every chat line it has "+
				"said before now reads as coming from a different author. Without -logfile the only "+
				"other trace of this was a discarded log line.", d.DataPath, d.LoadError))
	}

	if m.AuthFailures > 0 {
		add(SeverityError, fmt.Sprintf("%d packet(s) failed authentication", m.AuthFailures),
			fmt.Sprintf("Something on this network frames packets with a different key. Check that every "+
				"node shares the pre-shared key and the cluster name %q (key fingerprint %s).",
				d.Cluster, d.KeyFingerprint))
	}
	for _, addr := range d.Impostors {
		add(SeverityError, fmt.Sprintf("%s is using this node's identity", addr),
			fmt.Sprintf("That peer announces node id %s, which is this node's. A node generates its "+
				"id once and keeps it, so the two are running from copies of one state file. Every "+
				"version both write is stamped with the same id, so their writes to a shared key "+
				"compare equal, neither replica accepts the other, and the values stay different "+
				"for good. Stop one and delete its state file so it starts with a fresh identity.",
				d.NodeID))
	}
	for _, c := range d.IDConflicts {
		if c.Flips < IDConflictFlips {
			// One rebinding is what a node changing address looks like.
			continue
		}
		add(SeverityError,
			fmt.Sprintf("node id %s is claimed by %s and %s", shortID(c.ID), c.Addrs[0], c.Addrs[1]),
			fmt.Sprintf("The claim has moved between those addresses %d times. A node that changed "+
				"address would rebind once; repeated rebinding means both are live and sharing one "+
				"identity, so their writes compare equal and any key they both set stays split "+
				"permanently. Stop one and delete its state file.", c.Flips))
	}
	if m.SkewDrops > 0 {
		add(SeverityError, fmt.Sprintf("%d packet(s) dropped for clock skew", m.SkewDrops),
			"A peer's clock differs from this machine's by more than the accepted window. Packets "+
				"outside it are refused as replays, so the two cannot talk until a clock is corrected.")
	}
	if m.MalformedDrops > 0 {
		add(SeverityWarn, fmt.Sprintf("%d malformed packet(s)", m.MalformedDrops),
			"Authenticated frames whose body did not parse. Usually a peer running an older or newer "+
				"protocol version.")
	}
	if m.KVRefused > 0 {
		// The only condition here that this node can never gossip its way out
		// of: a peer holds entries this store has declined to accept, so the
		// two stay divergent until a limit moves. It outranks the packet-level
		// checks precisely because nothing retries its way past it — the
		// anti-entropy sweep will offer the same entries every ten seconds for
		// as long as the node runs.
		detail := "This node refused remote updates its store would not hold — a value over " +
			"-max-value-bytes, a key past -max-keys, or an entry too large to replicate at all. " +
			"They are not stale and will not arrive later: the peers that hold them stay " +
			"divergent from this node until a limit is raised or keys are freed. " +
			"GET /consistency names the buckets that differ."
		if d.Gauges.Keys > 0 {
			detail += fmt.Sprintf(" This store currently holds %d key(s).", d.Gauges.Keys)
		}
		add(SeverityError, fmt.Sprintf("%d remote update(s) refused by this node's limits", m.KVRefused), detail)
	}
	if m.ChatUnsolicited > 0 || m.ChatSelfAttributed > 0 {
		// Both are refusals, so nothing is broken here — but neither has a
		// legitimate cause. A backlog only ever answers a request this node
		// made, and no peer ever echoes this node its own lines, so a peer
		// doing either is running something that does not match the protocol.
		// Worth saying out loud: left unchecked these are exactly the frames
		// that used to raise an author's sequence out of reach and silence
		// them here for good.
		add(SeverityWarn, fmt.Sprintf("%d chat frame(s) refused as malformed repair",
			m.ChatUnsolicited+m.ChatSelfAttributed),
			fmt.Sprintf("%d backlog line(s) nobody asked for and %d line(s) claiming to be "+
				"written by this node. Both were dropped, and neither has a legitimate "+
				"cause: a backlog only answers a request this node made, and no peer echoes "+
				"a node its own lines. Some peer is running something that does not match "+
				"the protocol.", m.ChatUnsolicited, m.ChatSelfAttributed))
	}
	if len(d.ChatGaps) > 0 {
		// Chat is the one payload with no digest behind it, so a line that
		// never arrived leaves no trace in the store to compare. Before the
		// sequence existed this was not a quiet failure, it was an invisible
		// one: the author's log said sent, every receiver simply never had it,
		// and no counter moved at either end.
		var lost, outstanding uint64
		var worst string
		for _, g := range d.ChatGaps {
			lost += g.Lost
			outstanding += uint64(g.Outstanding)
			if g.Lost > 0 && worst == "" {
				worst = g.Nick
				if worst == "" {
					worst = g.Addr
				}
			}
		}
		switch {
		case lost > 0:
			detail := fmt.Sprintf("%d chat line(s) will not arrive: the author no longer holds "+
				"them, or never answered a request for them. Unlike a key, a chat line has no "+
				"digest to repair it from a third party — only its author can re-send it, and "+
				"only while it is still in that node's ring.", lost)
			if worst != "" {
				detail += fmt.Sprintf(" The earliest were from %s.", worst)
			}
			add(SeverityWarn, fmt.Sprintf("%d chat line(s) lost", lost), detail)
		case outstanding > 0:
			add(SeverityInfo, fmt.Sprintf("%d chat line(s) not yet arrived", outstanding),
				"A gap in an author's sequence. Most are a reordering that closes on its own; "+
					"the rest are asked for on the next gossip tick.")
		}
	}
	if m.ReplayDrops > 0 {
		add(SeverityInfo, fmt.Sprintf("%d replayed packet(s) ignored", m.ReplayDrops),
			"The same nonce arrived twice. Duplicated datagrams are normal on a lossy network; a "+
				"steadily climbing count is not.")
	}
	if m.SendErrors > 0 {
		// The last failure decides what this says. An oversized frame and an
		// unroutable peer both arrive here as "send error", and the advice is
		// opposite: one is a payload this node built too big to send, the
		// other is the network. Blaming the route for the first sends the
		// reader to check an address that is perfectly fine.
		detail := "Datagrams could not leave this machine — usually a peer address that no longer " +
			"routes, or an interface dropping while the node stays up."
		// Whether anything is wrong *now* is a separate question from whether
		// anything ever went wrong. The counter only climbs, so one blip —
		// Wi-Fi roaming, an interface bouncing, a peer that went away before
		// its eviction — left the node warning for the rest of its life while
		// every packet since had gone out fine. A warning that cannot clear is
		// one the reader learns to scroll past, which costs the real ones
		// their only chance of being read.
		recent := m.LastSendError == nil || m.LastSendError.At == 0 ||
			now.Sub(time.Unix(m.LastSendError.At, 0)) <= SendErrorRecentWindow
		if e := m.LastSendError; e != nil && e.Oversize {
			detail = "A frame too large for one datagram could not be sent. Nothing is wrong with " +
				"the route: an entry this big travels over a stream instead, and that did not work."
			if !d.StreamListener {
				detail += " This node has no stream listener, which is why — see the check below."
			} else {
				detail += " The peer's stream port is the thing to look at: a firewall that passes " +
					"UDP and blocks TCP looks exactly like this."
			}
			detail += " Until it does, that entry cannot reach that peer by any path, and the " +
				"replicas stay divergent however long anti-entropy runs."
		}
		if e := m.LastSendError; e != nil {
			detail += fmt.Sprintf(" Last: %s to %s (%s).", e.Kind, e.Addr, e.Message)
		}
		if !recent {
			detail = fmt.Sprintf("Nothing has failed to leave this machine in the last %s, so this "+
				"is history rather than a fault to chase.", SendErrorRecentWindow) +
				fmt.Sprintf(" Last: %s to %s (%s).", m.LastSendError.Kind, m.LastSendError.Addr, m.LastSendError.Message)
			add(SeverityInfo, fmt.Sprintf("%d send error(s), none recent", m.SendErrors), detail)
		} else {
			add(SeverityWarn, fmt.Sprintf("%d send error(s)", m.SendErrors), detail)
		}
	}
	// Counted since the beginning and explained by nothing. The stream is the
	// only path for a value too big for a datagram and for a whole-store sync
	// on join, so a node whose streams fail is not slightly degraded: an
	// oversized entry cannot reach that peer by any path, and the replicas stay
	// divergent for as long as the key lives.
	if m.StreamErrors > 0 {
		add(SeverityWarn, fmt.Sprintf("%d stream (TCP) error(s)", m.StreamErrors),
			"A stream to a peer or a seed failed. Datagrams and streams share a port number but not "+
				"a protocol, so a firewall that passes UDP and blocks TCP looks exactly like this. "+
				"Until it works, a joiner falls back to the bounded datagram state sync, and any "+
				"entry too large for one datagram cannot replicate at all.")
	}
	if !d.StreamListener {
		detail := "Another process holds the TCP port, so large state syncs fall back to datagrams " +
			"and a big keyspace may take several gossip rounds to converge."
		if e := m.LastSendError; e != nil && e.Oversize {
			detail += " It is also why the oversized frame above had nowhere to go: the stream is " +
				"the only path for an entry that does not fit a datagram."
		}
		add(SeverityWarn, fmt.Sprintf("No stream listener on %s", d.BindAddr), detail)
	}

	// A node seconds old has not failed to do anything yet: a peer answers on
	// its own heartbeat, and a seed replies when it feels like it. Flagging
	// either before then turns the first moments of every start into a red
	// screen.
	settled := d.UptimeSec >= int64(max(6, int(d.HeartbeatInterval.Seconds())*2))

	if settled && len(d.Peers) == 0 {
		if len(d.Seeds) == 0 {
			if d.MDNS {
				// Telling a node that is already looking to configure a seed
				// is advice it has taken: discovery is on and has found
				// nothing, which is a different problem with different causes.
				add(SeverityWarn, "LAN discovery has found nobody",
					"Nothing on this network answered as a member of this cluster. Either no other "+
						"node is running, or the ones that are differ in -cluster or -psk, or the "+
						"network does not carry multicast — which is common on guest Wi-Fi and "+
						"between subnets. A -bootstrap seed works where multicast does not.")
			} else {
				add(SeverityWarn, "No peers and no seeds",
					"A node with no bootstrap seed only ever learns peers that contact it first. Give it "+
						"-bootstrap, run a rendezvous service and point the others at it, or use -mdns "+
						"to find them on the LAN.")
			}
		} else {
			add(SeverityWarn, fmt.Sprintf("No peers learned from %d seed(s)", len(d.Seeds)),
				fmt.Sprintf("The seeds %s answered nothing. Check the bootstrap is running, that this "+
					"machine is on the same network, and that the cluster name and key match.",
					strings.Join(d.Seeds, ", ")))
		}
	}

	// The rendezvous service is the one component that sees where this node's
	// packets actually come from, so when it has told us, that outranks both
	// heuristics below: they can say "that address looks wrong" and only this
	// can say which address is right.
	if d.AdvertiseMismatch != "" {
		advertised := d.Addr
		suggestion := d.AdvertiseMismatch
		if host, _, err := net.SplitHostPort(d.AdvertiseMismatch); err == nil {
			if _, port, err := net.SplitHostPort(d.BindAddr); err == nil {
				// A stream is dialled from an ephemeral port, so the port
				// observed means nothing; the port this node binds is the one
				// peers need.
				suggestion = net.JoinHostPort(host, port)
			}
		}
		add(SeverityError, "Advertising an address the cluster does not see this node at",
			fmt.Sprintf("Peers are told to reach this node at %s, and the rendezvous service received "+
				"its packets from %s. Every peer resolves what it is told, so they are reaching "+
				"somewhere else — usually their own loopback. Pass -advertise %s.",
				advertised, d.AdvertiseMismatch, suggestion))
	} else if loopback(d.Addr) {
		add(SeverityWarn, "Advertising a loopback address",
			fmt.Sprintf("Peers are told to reach this node at %s, which resolves to whichever machine "+
				"reads it — their own. Pass -advertise with an address that routes.", d.Addr))
	} else if d.AdvertiseAddr != "" && d.LocalIPv4 != "" && !strings.HasPrefix(d.AdvertiseAddr, d.LocalIPv4) {
		add(SeverityInfo, "Advertise address is overridden",
			fmt.Sprintf("Peers are told %s, while this machine's own address is %s. That is right "+
				"behind a port forward and wrong on a plain LAN.", d.AdvertiseAddr, d.LocalIPv4))
	}

	if len(d.Strangers) > 0 {
		add(SeverityInfo, fmt.Sprintf("%d address(es) sent packets without being a peer", len(d.Strangers)),
			fmt.Sprintf("Traffic from %s — a node whose advertised address differs from the one its "+
				"packets come from, which is what a NAT or a wrong -advertise looks like.",
				join(strangerAddrs(d.Strangers), 3)))
	}

	if stale := stalePeers(d, now); len(stale) > 0 {
		add(SeverityWarn, fmt.Sprintf("%d peer(s) going stale", len(stale)),
			fmt.Sprintf("No packet from %s for over half the eviction window. They will be dropped "+
				"from the cluster shortly.", join(stale, 3)))
	}

	if settled {
		var silent []string
		for _, p := range d.Peers {
			if p.PacketsIn == 0 && p.PacketsOut > 0 {
				silent = append(silent, p.Addr)
			}
		}
		if len(silent) > 0 {
			add(SeverityError, fmt.Sprintf("%d peer(s) never answered", len(silent)),
				fmt.Sprintf("This node sends to %s and receives nothing back. One-way UDP like this "+
					"is a firewall or a wrong advertised port, not packet loss.", join(silent, 3)))
		}
	}

	if groups := topology.Components(d.Topology); len(groups) > 1 {
		sizes := make([]string, 0, len(groups))
		for _, g := range groups {
			sizes = append(sizes, fmt.Sprint(len(g)))
		}
		add(SeverityError, fmt.Sprintf("Cluster looks partitioned into %d groups", len(groups)),
			fmt.Sprintf("Known nodes split as %s. Each group converges on its own and diverges from "+
				"the others until a link is restored.", strings.Join(sizes, " | ")))
	}

	if one := topology.Unconfirmed(d.Topology); len(one) > 0 {
		add(SeverityInfo, fmt.Sprintf("%d link(s) claimed by one end only", len(one)),
			"Gossip carries each node's own peer list, so a link either end has not yet reported "+
				"stays unconfirmed. Normal shortly after a node joins.")
	}

	// A node whose gossip loop runs late is evicted by peers that are perfectly
	// happy with it. This was measured on a tablet: a ~100 MB store delayed the
	// loop past the 15 s eviction window and the phone flapped in and out of
	// the cluster until the large keys were deleted. Nothing warned about it,
	// because every counter in the system was clean — the node was doing all
	// the right work, just too slowly to stay a member while doing it.
	if late := d.Metrics.GossipLate.Max; late > 0 && d.EvictThreshold > 0 {
		worst := time.Duration(late * float64(time.Second))
		round := time.Duration(d.Metrics.GossipRound.Max * float64(time.Second))
		switch {
		case worst >= d.EvictThreshold:
			add(SeverityError, fmt.Sprintf("Gossip ran %s late, past the %s eviction threshold",
				worst.Round(time.Millisecond), d.EvictThreshold),
				fmt.Sprintf("A tick this late means peers stopped hearing from this node for longer "+
					"than they wait before evicting it, so it drops out of clusters that are working. "+
					"The slowest round itself took %s. The usual cause is a store large enough that "+
					"walking it delays the loop — see -max-value-bytes.",
					round.Round(time.Millisecond)))
		case worst >= d.EvictThreshold/2:
			add(SeverityWarn, fmt.Sprintf("Gossip ran %s late", worst.Round(time.Millisecond)),
				fmt.Sprintf("Peers evict after %s of silence, and the worst tick used %.0f%% of that "+
					"budget before the round even started. The slowest round took %s.",
					d.EvictThreshold, 100*late/d.EvictThreshold.Seconds(),
					round.Round(time.Millisecond)))
		}
	}

	if d.TombstoneTTL == 0 && d.Gauges.Tombstones > 0 {
		add(SeverityInfo, fmt.Sprintf("%d tombstone(s) kept forever", d.Gauges.Tombstones),
			"Deletes replicate as tombstones and are never reclaimed while GC is off. Set "+
				"-tombstone-ttl if deletes are frequent — it has to exceed the longest partition "+
				"you expect to heal.")
	}

	// The other half of that advice, which nothing said once it was taken. A
	// reclaimed tombstone is the only record that a key was deleted, so a node
	// that was away longer than the age comes back still holding the value and
	// pushes it to a replica with nothing left to refuse it — the delete is
	// undone everywhere and the consistency check calls the result converged,
	// because it is. Reproduced on two nodes with an 8s age.
	//
	// It cannot be detected after the fact: a node cannot tell a resurrection
	// from someone writing the key again. Naming the exposure while it is
	// configured is the only honest moment to do it.
	if d.TombstoneTTL > 0 {
		add(SeverityInfo, fmt.Sprintf("Tombstones are reclaimed after %s", d.TombstoneTTL),
			fmt.Sprintf("A delete is remembered for %s and then forgotten. A node that is away "+
				"for longer comes back still holding the value, and the delete is undone on "+
				"every replica with nothing to report it — the cluster converges on the "+
				"resurrected value. Keep the age above the longest outage a node can return "+
				"from, or leave GC off and let tombstones accumulate.", d.TombstoneTTL))
	}

	if !d.PSKSet {
		add(SeverityInfo, "No pre-shared key",
			"The framing key comes from the cluster name alone. That keeps two clusters on one "+
				"network apart but provides no secrecy: anyone on the network can read and write.")
	}

	// A healthy node should say so plainly, but only when nothing above
	// contradicts it.
	healthy := true
	for _, c := range out {
		if c.Severity == SeverityWarn || c.Severity == SeverityError {
			healthy = false
			break
		}
	}
	if healthy {
		out = append([]Check{{
			Severity: SeverityOK,
			Title:    "Node looks healthy",
			Detail: fmt.Sprintf("%d peer(s), %d key(s), no dropped packets.",
				len(d.Peers), d.Gauges.Keys),
		}}, out...)
	}
	return out
}

// Report renders the snapshot and its findings as text, for pasting into a bug
// report where a JSON blob would not be read.
func Report(d Snapshot, now time.Time) string {
	var b strings.Builder
	age := func(ts int64) string {
		if ts == 0 {
			return "never"
		}
		return fmt.Sprintf("%ds ago", now.Unix()-ts)
	}
	m := d.Metrics

	fmt.Fprintf(&b, "rezoagwe node diagnostics\n")
	fmt.Fprintf(&b, "addr            %s\n", d.Addr)
	fmt.Fprintf(&b, "nick            %s\n", d.Nick)
	fmt.Fprintf(&b, "node id         %s\n", d.NodeID)
	psk := "no psk"
	if d.PSKSet {
		psk = "psk set"
	}
	fmt.Fprintf(&b, "cluster         %s (key %s, %s)\n", d.Cluster, d.KeyFingerprint, psk)
	fmt.Fprintf(&b, "uptime          %ds\n", d.UptimeSec)
	// "(none)" here read as "advertising nothing", when a node with no
	// -advertise is advertising the address it bound.
	advertising := d.AdvertiseAddr + " (-advertise)"
	if d.AdvertiseAddr == "" {
		advertising = d.BindAddr + " (from -node)"
	}
	fmt.Fprintf(&b, "addresses       bind %s, advertising %s, local %s\n",
		d.BindAddr, advertising, orNone(d.LocalIPv4))
	fmt.Fprintf(&b, "streams         %t\n", d.StreamListener)
	fmt.Fprintf(&b, "seeds           %s\n", orNone(strings.Join(d.Seeds, ", ")))
	fmt.Fprintf(&b, "intervals       gossip %s, heartbeat %s, evict %s\n",
		d.GossipInterval, d.HeartbeatInterval, d.EvictThreshold)
	fmt.Fprintf(&b, "store           %d keys, %d tombstones, %d value bytes, clock %d\n",
		d.Gauges.Keys, d.Gauges.Tombstones, d.Gauges.ValueBytes, d.Gauges.Clock)
	fmt.Fprintf(&b, "traffic         %d sent / %d received, %dB / %dB\n",
		m.PacketsSent, m.PacketsRecv, m.BytesSent, m.BytesRecv)
	fmt.Fprintf(&b, "drops           auth %d, replay %d, skew %d, malformed %d\n",
		m.AuthFailures, m.ReplayDrops, m.SkewDrops, m.MalformedDrops)
	fmt.Fprintf(&b, "anti-entropy    %d rounds, %d pushed, %d pulled\n",
		m.AERounds, m.AEPushed, m.AEPulled)
	if m.GossipRound.Count > 0 {
		fmt.Fprintf(&b, "timings         gossip round p50 %s / max %s, late max %s",
			secs(m.GossipRound.Quantile(0.5)), secs(m.GossipRound.Max), secs(m.GossipLate.Max))
		if m.StateSync.Count > 0 {
			fmt.Fprintf(&b, ", state sync max %s", secs(m.StateSync.Max))
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "\npeers (%d)\n", len(d.Peers))
	for _, p := range d.Peers {
		fmt.Fprintf(&b, "  %s  %s  in %d/%dB  out %d/%dB  seen %s\n",
			p.Addr, orDash(p.Nick), p.PacketsIn, p.BytesIn, p.PacketsOut, p.BytesOut, age(p.LastSeen))
	}
	if len(d.Strangers) > 0 {
		fmt.Fprintf(&b, "\nunknown sources (%d)\n", len(d.Strangers))
		for _, s := range d.Strangers {
			fmt.Fprintf(&b, "  %s  in %d  rejected %d  seen %s\n",
				s.Addr, s.PacketsIn, s.Rejected, age(s.LastSeen))
		}
	}

	fmt.Fprintf(&b, "\ntopology: %d nodes, %d links, %d component(s)\n",
		len(d.Topology.Nodes), len(d.Topology.Links), len(topology.Components(d.Topology)))
	for _, l := range d.Topology.Links {
		fmt.Fprintf(&b, "  %s -- %s (%s)\n", l.A, l.B, l.Kind)
	}

	b.WriteString("\nchecks\n")
	for _, c := range Checks(d, now) {
		fmt.Fprintf(&b, "  [%s] %s: %s\n", strings.ToUpper(string(c.Severity)), c.Title, c.Detail)
	}
	return b.String()
}

// Worst is the highest severity among the checks, which is what a status bar
// has room for.
func Worst(checks []Check) Severity {
	rank := map[Severity]int{SeverityOK: 0, SeverityInfo: 1, SeverityWarn: 2, SeverityError: 3}
	worst := SeverityOK
	for _, c := range checks {
		if rank[c.Severity] > rank[worst] {
			worst = c.Severity
		}
	}
	return worst
}

func stalePeers(d Snapshot, now time.Time) []string {
	if d.EvictThreshold <= 0 {
		return nil
	}
	half := int64(d.EvictThreshold.Seconds() / 2)
	var out []string
	for _, p := range d.Peers {
		if p.LastSeen > 0 && now.Unix()-p.LastSeen > half {
			out = append(out, label(p))
		}
	}
	sort.Strings(out)
	return out
}

func label(p Peer) string {
	if p.Nick != "" {
		return p.Nick
	}
	return p.Addr
}

func strangerAddrs(s []Stranger) []string {
	out := make([]string, 0, len(s))
	for _, v := range s {
		out = append(out, v.Addr)
	}
	return out
}

// join lists at most n items, so a finding about forty peers stays one line.
func join(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:n], ", "), len(items)-n)
}

func loopback(addr string) bool {
	host, _, found := strings.Cut(addr, ":")
	if !found {
		return false
	}
	return host == "" || host == "127.0.0.1" || host == "localhost" || host == "::1" ||
		strings.HasPrefix(host, "127.")
}

// secs renders a duration in seconds at a readable precision.
func secs(v float64) string {
	return time.Duration(v * float64(time.Second)).Round(time.Millisecond).String()
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// persistenceDetail is the saved-state line, worded the same in all three
// implementations. It used to print the raw epoch ("1792948669209 ms UTC") and
// an empty recovery-file name when there was none.
func persistenceDetail(mode string, durable bool, lastSuccessMs int64, pending bool, recovery string) string {
	last := "never"
	if lastSuccessMs > 0 {
		last = time.UnixMilli(lastSuccessMs).UTC().Format(time.RFC3339)
	}
	if recovery == "" {
		recovery = "none"
	}
	return fmt.Sprintf("Mode: %s; durable: %t; Last save: %s; pending: %t; recovery: %s", mode, durable, last, pending, recovery)
}
