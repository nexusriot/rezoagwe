package node

import (
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/discovery/diagnostics"
	"github.com/nexusriot/rezoagwe/pkg/discovery/topology"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// A node told to advertise a different address must put that address, not its
// bind address, into everything a peer reads.
func TestAdvertisedAddressIsWhatPeersAreTold(t *testing.T) {
	net := transport.NewMemNet(1)
	n, err := New(Config{
		NodeAddr:          "10.0.0.1:3137",
		AdvertiseAddr:     "203.0.113.7:3137",
		Nick:              "alice",
		Transport:         net.Node("10.0.0.1:3137"),
		GossipInterval:    idleInterval,
		HeartbeatInterval: idleInterval,
		EvictThreshold:    idleInterval,
		SweepInterval:     idleInterval,
	}, nil)
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	n.Start()
	t.Cleanup(n.Stop)

	if n.Addr() != "203.0.113.7:3137" {
		t.Fatalf("Addr() = %q, want the advertised address", n.Addr())
	}
	if n.BindAddr() != "10.0.0.1:3137" {
		t.Fatalf("BindAddr() = %q, want the bind address", n.BindAddr())
	}
	if h := n.helloMessage(); h.From != "203.0.113.7:3137" {
		t.Fatalf("Hello.From = %q, want the advertised address", h.From)
	}
	if n.Status().Addr != "203.0.113.7:3137" {
		t.Fatalf("Status.Addr = %q", n.Status().Addr)
	}

	// And it must still recognise itself under either spelling.
	if n.Model.AddPeer("203.0.113.7:3137") || n.Model.AddPeer("10.0.0.1:3137") {
		t.Fatal("the node added itself as a peer")
	}
}

func TestAdvertiseDefaultsToTheBindAddress(t *testing.T) {
	n := newTestNode(t, transport.NewMemNet(1), "10.0.0.1:3137")
	if n.Addr() != n.BindAddr() {
		t.Fatalf("Addr() = %q, BindAddr() = %q; want them equal by default", n.Addr(), n.BindAddr())
	}
}

func TestDiagnosticsSnapshotDescribesTheNode(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)
	a.Set("k", "value", 0)
	a.send(b.Addr(), pb.KindHello, a.helloMessage())

	d := a.Diagnostics()
	if d.Addr != a.Addr() || d.Nick == "" || d.NodeID == "" {
		t.Fatalf("identity fields = %+v", d)
	}
	if d.KeyFingerprint == "" {
		t.Fatal("no key fingerprint, so two operators cannot compare keys")
	}
	if d.Gauges.Keys != 1 || d.Gauges.ValueBytes != 5 {
		t.Fatalf("gauges = %+v", d.Gauges)
	}
	if len(d.Peers) != 1 || d.Peers[0].Addr != b.Addr() {
		t.Fatalf("peers = %+v", d.Peers)
	}
	if d.Peers[0].PacketsOut == 0 {
		t.Fatal("per-peer traffic was not attributed")
	}
	if len(d.Topology.Nodes) != 2 {
		t.Fatalf("topology has %d nodes, want 2", len(d.Topology.Nodes))
	}
	if !d.StreamListener {
		t.Fatal("the in-memory transport reported no stream support")
	}
}

func TestHealthChecksRunOverALiveNode(t *testing.T) {
	n := newTestNode(t, transport.NewMemNet(1), "10.0.0.1:3137")
	checks := n.HealthChecks()
	if len(checks) == 0 {
		t.Fatal("no checks produced")
	}
	report := n.DiagnosticsReport()
	if !strings.Contains(report, "rezoagwe node diagnostics") || !strings.Contains(report, "checks") {
		t.Fatalf("report = %s", report)
	}
}

// Once a stranger introduces itself, it is a peer and must stop being reported
// as an anomaly.
func TestAStrangerThatBecomesAPeerIsNoLongerReported(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")
	a.noteStranger("10.0.0.8:3137", "")
	if len(a.Strangers()) != 1 {
		t.Fatal("stranger not recorded")
	}
	a.Model.AddPeer("10.0.0.8:3137")
	if len(a.Strangers()) != 0 {
		t.Fatalf("still reported after joining: %+v", a.Strangers())
	}
}

// The rules must agree with the engine: a real two-node cluster is healthy.
func TestALiveTwoNodeClusterPassesItsOwnChecks(t *testing.T) {
	net := transport.NewMemNet(1)
	a, b := newPairedNodes(t, net)
	a.send(b.Addr(), pb.KindHello, a.helloMessage())
	b.send(a.Addr(), pb.KindHello, b.helloMessage())
	waitFor(t, "the hello exchange", func() bool {
		d := a.Diagnostics()
		return len(d.Peers) == 1 && d.Peers[0].PacketsIn > 0
	})

	d := a.Diagnostics()
	d.UptimeSec = 600 // past the settling window
	for _, c := range diagnostics.Checks(d, time.Now()) {
		if c.Severity == diagnostics.SeverityError {
			t.Fatalf("healthy cluster reported an error: %s — %s", c.Title, c.Detail)
		}
	}
	if len(topology.Components(d.Topology)) != 1 {
		t.Fatal("a connected pair looks partitioned")
	}
}

// The bug this guards, found only by the containerised suite: peers are keyed
// by the address they advertise, and their packets arrive from a resolved IP.
// Attributing receives to the source address left every peer's inbound count at
// zero, which the diagnostics then read as "this peer never answered" — an
// ERROR against a demonstrably healthy cluster.
func TestReceivedTrafficIsAttributedToTheAdvertisedAddress(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")

	// A peer known by a name that is not where its packets come from, which is
	// what a hostname-advertising cluster looks like.
	a.Model.AddPeer("peer-b:3137")

	sender := newTestNode(t, net, "10.0.0.2:3137")
	sender.send("10.0.0.1:3137", pb.KindHello, pb.Hello{From: "peer-b:3137", Nick: "bob"})

	waitFor(t, "the packet to be attributed", func() bool {
		c, ok := a.Metrics.PeerCounters("peer-b:3137")
		return ok && c.PacketsIn > 0
	})

	// And it must not have been filed under the source address instead.
	if c, ok := a.Metrics.PeerCounters("10.0.0.2:3137"); ok && c.PacketsIn > 0 {
		t.Fatalf("traffic was also attributed to the source address: %+v", c)
	}
}

// The same mismatch made every healthy peer a permanent "unknown source".
func TestAPeerKnownByAnotherNameIsNotAStranger(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")
	a.Model.AddPeer("peer-b:3137")

	sender := newTestNode(t, net, "10.0.0.2:3137")
	sender.send("10.0.0.1:3137", pb.KindHello, pb.Hello{From: "peer-b:3137"})

	waitFor(t, "the packet to arrive", func() bool {
		c, ok := a.Metrics.PeerCounters("peer-b:3137")
		return ok && c.PacketsIn > 0
	})
	if s := a.Strangers(); len(s) != 0 {
		t.Fatalf("a known peer arriving from another address was recorded as a stranger: %+v", s)
	}
}

// A body that names nobody the node knows still has to be recorded, or the NAT
// signature the check exists for disappears with the false positives.
func TestAnUnclaimedSourceIsStillAStranger(t *testing.T) {
	net := transport.NewMemNet(1)
	a := newTestNode(t, net, "10.0.0.1:3137")
	sender := newTestNode(t, net, "10.0.0.8:3137")

	sender.send("10.0.0.1:3137", pb.KindHello, pb.Hello{From: "192.168.99.1:3137"})

	waitFor(t, "the unknown source to be recorded", func() bool {
		return len(a.Strangers()) > 0
	})
	if got := a.Strangers()[0].Addr; got != "10.0.0.8:3137" {
		t.Fatalf("stranger = %q, want the source address", got)
	}
}
