package node

import (
	"net"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/diagnostics"
	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
)

// Diagnostics assembles everything the health rules read.
//
// It is built here rather than inside the diagnostics package so that package
// stays a pure function of a struct: every rule is a claim about the protocol,
// and a claim is only testable if the state it reads can be fabricated.
func (n *Node) Diagnostics() diagnostics.Snapshot {
	snap := n.Metrics.Snapshot()

	peers := make([]diagnostics.Peer, 0)
	for _, p := range n.Peers() {
		dp := diagnostics.Peer{Addr: p.Addr, Nick: p.Nick, LastSeen: p.LastSeen}
		if c, ok := n.Metrics.PeerCounters(model.NormalizeAddr(p.Addr)); ok {
			dp.PeerCount = c
		}
		peers = append(peers, dp)
	}

	strangers := make([]diagnostics.Stranger, 0)
	for _, s := range n.Strangers() {
		strangers = append(strangers, diagnostics.Stranger{
			Addr:      s.Addr,
			LastSeen:  s.LastSeen,
			PacketsIn: s.PacketsIn,
			Rejected:  s.Rejected,
		})
	}

	advertise := ""
	if n.cfg.AdvertiseAddr != n.cfg.NodeAddr {
		advertise = n.cfg.AdvertiseAddr
	}

	return diagnostics.Snapshot{
		Addr:              n.cfg.AdvertiseAddr,
		BindAddr:          n.cfg.NodeAddr,
		AdvertiseAddr:     advertise,
		LocalIPv4:         LocalIPv4(),
		Nick:              n.Model.Nick(),
		NodeID:            n.Model.NodeID,
		Cluster:           n.cfg.Cluster,
		KeyFingerprint:    n.codec.KeyFingerprint(),
		PSKSet:            n.cfg.PSK != "",
		Version:           n.cfg.Version,
		UptimeSec:         int64(n.Uptime().Seconds()),
		StreamListener:    n.tr.StreamsAvailable(),
		Seeds:             append([]string(nil), n.cfg.BootstrapAddrs...),
		Peers:             peers,
		Strangers:         strangers,
		Metrics:           snap,
		Gauges:            snap.Gauges,
		Topology:          n.Topology(),
		GossipInterval:    n.cfg.GossipInterval,
		HeartbeatInterval: n.cfg.HeartbeatInterval,
		EvictThreshold:    n.cfg.EvictThreshold,
		TombstoneTTL:      n.cfg.TombstoneTTL,
	}
}

// HealthChecks runs the rules over a fresh snapshot.
func (n *Node) HealthChecks() []diagnostics.Check {
	return diagnostics.Checks(n.Diagnostics(), time.Now())
}

// DiagnosticsReport renders the snapshot and its findings as text.
func (n *Node) DiagnosticsReport() string {
	return diagnostics.Report(n.Diagnostics(), time.Now())
}

// LocalIPv4 is this machine's first routable IPv4 address, or "" when there is
// none. It exists to answer "you are advertising X, but you are at Y", which is
// the single most common reason a LAN cluster never forms.
func LocalIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}
