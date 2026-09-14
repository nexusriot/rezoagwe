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
	"sort"
	"strings"
	"time"

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

// Snapshot is everything the rules read. It is a plain struct so a test can
// fabricate any cluster condition without running one.
type Snapshot struct {
	Addr           string           `json:"addr"`
	BindAddr       string           `json:"bind_addr"`
	AdvertiseAddr  string           `json:"advertise_addr,omitempty"`
	LocalIPv4      string           `json:"local_ipv4,omitempty"`
	Nick           string           `json:"nick"`
	NodeID         string           `json:"node_id"`
	Cluster        string           `json:"cluster"`
	KeyFingerprint string           `json:"key_fingerprint"`
	PSKSet         bool             `json:"psk_set"`
	Version        string           `json:"version,omitempty"`
	UptimeSec      int64            `json:"uptime_sec"`
	StreamListener bool             `json:"stream_listener"`
	Seeds          []string         `json:"seeds"`
	Peers          []Peer           `json:"peers"`
	Strangers      []Stranger       `json:"strangers"`
	Metrics        metrics.Snapshot `json:"-"`
	Gauges         metrics.Gauges   `json:"gauges"`
	Topology       topology.Graph   `json:"topology"`

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
	m := d.Metrics

	if m.AuthFailures > 0 {
		add(SeverityError, fmt.Sprintf("%d packet(s) failed authentication", m.AuthFailures),
			fmt.Sprintf("Something on this network frames packets with a different key. Check that every "+
				"node shares the pre-shared key and the cluster name %q (key fingerprint %s).",
				d.Cluster, d.KeyFingerprint))
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
	if m.ReplayDrops > 0 {
		add(SeverityInfo, fmt.Sprintf("%d replayed packet(s) ignored", m.ReplayDrops),
			"The same nonce arrived twice. Duplicated datagrams are normal on a lossy network; a "+
				"steadily climbing count is not.")
	}
	if m.SendErrors > 0 {
		detail := "Datagrams could not leave this machine — usually a peer address that no longer " +
			"routes, or an interface dropping while the node stays up."
		if e := m.LastSendError; e != nil {
			detail += fmt.Sprintf(" Last: %s to %s (%s).", e.Kind, e.Addr, e.Message)
		}
		add(SeverityWarn, fmt.Sprintf("%d send error(s)", m.SendErrors), detail)
	}
	if !d.StreamListener {
		add(SeverityWarn, fmt.Sprintf("No stream listener on %s", d.BindAddr),
			"Another process holds the TCP port, so large state syncs fall back to datagrams and a "+
				"big keyspace may take several gossip rounds to converge.")
	}

	// A node seconds old has not failed to do anything yet: a peer answers on
	// its own heartbeat, and a seed replies when it feels like it. Flagging
	// either before then turns the first moments of every start into a red
	// screen.
	settled := d.UptimeSec >= int64(max(6, int(d.HeartbeatInterval.Seconds())*2))

	if settled && len(d.Peers) == 0 {
		if len(d.Seeds) == 0 {
			add(SeverityWarn, "No peers and no seeds",
				"A node with no bootstrap seed only ever learns peers that contact it first. Give it "+
					"-bootstrap, or run a rendezvous service and point the others at it.")
		} else {
			add(SeverityWarn, fmt.Sprintf("No peers learned from %d seed(s)", len(d.Seeds)),
				fmt.Sprintf("The seeds %s answered nothing. Check the bootstrap is running, that this "+
					"machine is on the same network, and that the cluster name and key match.",
					strings.Join(d.Seeds, ", ")))
		}
	}

	if loopback(d.Addr) {
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

	if d.TombstoneTTL == 0 && d.Gauges.Tombstones > 0 {
		add(SeverityInfo, fmt.Sprintf("%d tombstone(s) kept forever", d.Gauges.Tombstones),
			"Deletes replicate as tombstones and are never reclaimed while GC is off. Set "+
				"-tombstone-ttl if deletes are frequent — it has to exceed the longest partition "+
				"you expect to heal.")
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
