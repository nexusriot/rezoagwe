package node

import (
	"net"
	"testing"
	"time"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

func TestSeenAtIsRecordedFromARoster(t *testing.T) {
	net := transport.NewMemNet(1)
	n := newTestNode(t, net, "10.0.0.1:3137")

	if got := n.SeenAt(); got != "" {
		t.Errorf("SeenAt = %q before any roster, want empty", got)
	}
	n.applyRoster(pb.BootstrapRoster{SeenAt: "203.0.113.7:3137"})
	if got := n.SeenAt(); got != "203.0.113.7:3137" {
		t.Errorf("SeenAt = %q, want the observed address", got)
	}
}

// The whole point: a node advertising something nobody can reach is told what
// to use instead, by the one component that can see it.
func TestAdvertiseMismatchIsReported(t *testing.T) {
	net := transport.NewMemNet(1)
	n := newTestNode(t, net, "10.0.0.1:3137")

	n.applyRoster(pb.BootstrapRoster{SeenAt: "203.0.113.7:3137"})
	if got := n.AdvertiseMismatch(); got != "203.0.113.7:3137" {
		t.Errorf("AdvertiseMismatch = %q, want the observed address", got)
	}
}

func TestAdvertiseMismatchIsSilentWhenTheyAgree(t *testing.T) {
	net := transport.NewMemNet(1)
	n := newTestNode(t, net, "10.0.0.1:3137")

	// The same host, and a port that differs because a stream is dialled from
	// an ephemeral one. Only the host is comparable, so this must be silent.
	n.applyRoster(pb.BootstrapRoster{SeenAt: "10.0.0.1:51234"})
	if got := n.AdvertiseMismatch(); got != "" {
		t.Errorf("AdvertiseMismatch = %q for the same host on a different port", got)
	}
}

// A seed that predates the field says nothing, and saying nothing must not
// read as a mismatch.
func TestAdvertiseMismatchIsSilentWithoutAnObservation(t *testing.T) {
	net := transport.NewMemNet(1)
	n := newTestNode(t, net, "10.0.0.1:3137")

	n.applyRoster(pb.BootstrapRoster{Peers: []pb.BootstrapPeer{{Addr: "10.0.0.2:3137"}}})
	if got := n.AdvertiseMismatch(); got != "" {
		t.Errorf("AdvertiseMismatch = %q with nothing observed", got)
	}
}

func TestSameHost(t *testing.T) {
	cases := []struct {
		advertised, observed string
		want                 bool
	}{
		{"10.0.0.1", "10.0.0.1", true},
		{"10.0.0.1", "10.0.0.2", false},
		{"", "10.0.0.1", false},          // a wildcard advertises nothing in particular
		{"127.0.0.1", "10.0.0.1", false}, // loopback on a LAN
		{"localhost", "127.0.0.1", true}, // a name that resolves to where we were seen
	}
	for _, c := range cases {
		if got := sameHost(c.advertised, c.observed); got != c.want {
			t.Errorf("sameHost(%q, %q) = %v, want %v", c.advertised, c.observed, got, c.want)
		}
	}
}

// ResolveAdvertise runs before anything else exists, so it has to work with no
// node, no loops and no transport of its own. The seed here is a bare listener
// speaking the real wire format, which is all a rendezvous service is from
// this side.
func TestResolveAdvertiseTakesTheHostAndKeepsTheBoundPort(t *testing.T) {
	seed, stop := fakeSeed(t, "203.0.113.7:51234")
	defer stop()

	got, err := ResolveAdvertise(Config{
		BootstrapAddrs: []string{seed},
		NodeAddr:       ":3137",
		Cluster:        "resolve-test",
		PSK:            "s3cret",
	}, 3*time.Second)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The host comes from the observation; the port is the one this node
	// binds, because the observed port was an ephemeral stream source.
	if got != "203.0.113.7:3137" {
		t.Errorf("resolved %q, want 203.0.113.7:3137", got)
	}
}

// A seed that predates the field answers a roster with no observation, which
// is not an error and not something to adopt.
func TestResolveAdvertiseToleratesAnOlderSeed(t *testing.T) {
	seed, stop := fakeSeed(t, "")
	defer stop()

	got, err := ResolveAdvertise(Config{
		BootstrapAddrs: []string{seed},
		NodeAddr:       ":3137",
		Cluster:        "resolve-test",
		PSK:            "s3cret",
	}, 3*time.Second)
	if err != nil {
		t.Errorf("an older seed returned %v, want nil", err)
	}
	if got != "" {
		t.Errorf("resolved %q from a seed that reported nothing", got)
	}
}

// An unreachable seed is not a reason to refuse to start.
func TestResolveAdvertiseReportsAnUnreachableSeed(t *testing.T) {
	got, err := ResolveAdvertise(Config{
		BootstrapAddrs: []string{"127.0.0.1:1"}, // nothing listens here
		NodeAddr:       ":3137",
	}, 500*time.Millisecond)
	if err == nil {
		t.Error("an unreachable seed reported no error")
	}
	if got != "" {
		t.Errorf("resolved %q from an unreachable seed", got)
	}
}

func TestResolveAdvertiseWithNoSeedsIsNotAnError(t *testing.T) {
	got, err := ResolveAdvertise(Config{NodeAddr: ":3137"}, time.Second)
	if err != nil {
		t.Errorf("no seeds returned %v, want nil", err)
	}
	if got != "" {
		t.Errorf("resolved %q with no seeds", got)
	}
}

// A seed with the wrong key answers nothing this node can read, and must not
// be mistaken for a seed that reported no address.
func TestResolveAdvertiseRefusesAnUnauthenticatedAnswer(t *testing.T) {
	ln, err := netListen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		transport.ReadFrameFrom(conn, time.Second)
		// A roster framed with a different key.
		other := pb.NewCodec("wrong", "resolve-test")
		pkt, _ := other.Encode(pb.KindBootstrapRoster, pb.BootstrapRoster{SeenAt: "203.0.113.7:9"})
		transport.WriteFrameTo(conn, time.Second, pkt)
	}()

	got, err := ResolveAdvertise(Config{
		BootstrapAddrs: []string{ln.Addr().String()},
		NodeAddr:       ":3137",
		Cluster:        "resolve-test",
		PSK:            "s3cret",
	}, 3*time.Second)
	if err == nil {
		t.Error("an answer that failed authentication was accepted")
	}
	if got != "" {
		t.Errorf("adopted %q from an unauthenticated answer", got)
	}
}

// fakeSeed is a rendezvous service as far as ResolveAdvertise can tell: one
// framed, authenticated roster carrying the given observation.
func fakeSeed(t *testing.T, seenAt string) (string, func()) {
	t.Helper()
	ln, err := netListen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	codec := pb.NewCodec("s3cret", "resolve-test")
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if _, err := transport.ReadFrameFrom(conn, time.Second); err != nil {
					return
				}
				pkt, err := codec.Encode(pb.KindBootstrapRoster, pb.BootstrapRoster{SeenAt: seenAt})
				if err != nil {
					return
				}
				transport.WriteFrameTo(conn, time.Second, pkt)
			}()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func netListen() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
