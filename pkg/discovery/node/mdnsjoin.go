package node

import (
	"net"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/nexusriot/rezoagwe/pkg/discovery/mdns"
	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// mdnsBrowseWait is how long a browse listens for answers. Responders delay
// a multicast reply by up to 100 ms to avoid answering in unison, so anything
// much shorter than this systematically misses the quieter half of the LAN.
const mdnsBrowseWait = 750 * time.Millisecond

// startMDNS advertises this node and begins looking for others.
//
// Seeds are the setup step most likely to defeat a new user, and the one the
// phone app feels most: a node on a LAN has to be told an address that the
// network could tell it. This does not replace bootstrap — a cluster spanning
// subnets still needs a rendezvous — it removes the need for one on a single
// LAN, where multicast reaches every node anyway.
func (n *Node) startMDNS() error {
	host, port := splitHostPort(n.cfg.AdvertiseAddr)
	cfg := mdns.Config{
		Instance: n.Model.NodeID,
		Port:     port,
		Cluster:  n.cfg.Cluster,
		Nick:     n.Model.Nick(),
	}
	// A node told what to advertise is advertising it for a reason; publishing
	// an address worked out from the interfaces instead would reintroduce the
	// problem -advertise was added to solve.
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
		cfg.IP = ip
	}
	srv, err := mdns.New(cfg)
	if err != nil {
		return err
	}
	n.mdns = srv
	n.loop(n.mdnsLoop)
	return nil
}

// mdnsLoop announces this node and folds discovered peers into the peer table.
func (n *Node) mdnsLoop() {
	defer n.mdns.Close()
	n.mdns.Announce()
	n.adoptMDNS()

	t := time.NewTicker(n.cfg.GossipInterval)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-t.C:
			// Announcing on every tick keeps a node findable by anyone that
			// started after it, without them having to ask.
			n.mdns.Announce()
			n.adoptMDNS()
		}
	}
}

// adoptMDNS browses and adds whatever it finds that belongs to this cluster.
func (n *Node) adoptMDNS() {
	found := n.mdns.Browse(mdnsBrowseWait)
	strangers := 0
	for _, e := range found {
		// The cluster name in a TXT record is a hint, not an authentication:
		// a node that lies about it still has to produce a valid MAC before
		// anything it says is believed. Filtering here only avoids talking to
		// nodes that will certainly refuse us.
		//
		// A missing cluster is not a match. Every rezoagwe node publishes one,
		// so an entry without it is something else on the LAN that happens to
		// answer — and treating "unknown" as "probably ours" is how a phone's
		// service-discovery record ended up in the peer table.
		if e.Cluster != n.cfg.Cluster {
			strangers++
			continue
		}
		if n.Model.IsSelf(e.Addr) || n.Model.HasPeerNormalized(e.Addr) {
			continue
		}
		if !model.ValidPeerAddr(e.Addr) {
			continue
		}
		if n.Model.AddPeer(e.Addr) {
			who := e.Nick
			if who == "" {
				who = e.Addr
			}
			n.logActivity("found %s on the LAN", who)
			log.Debugf("mdns: adopted %s (%s)", e.Addr, e.Nick)
			n.send(e.Addr, pb.KindHello, n.helloMessage())
			n.ev.PeersChanged()
		}
	}
	if strangers > 0 {
		// A LAN with rezoagwe nodes on it that this node cannot join is worth
		// saying out loud: it is almost always a -cluster or -psk typo.
		log.Debugf("mdns: ignored %d node(s) in another cluster", strangers)
	}
}

// splitHostPort parses an advertised address, tolerating the ":3137" form.
func splitHostPort(addr string) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, 0
	}
	return host, port
}
