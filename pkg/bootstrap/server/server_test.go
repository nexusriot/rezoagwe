package server

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/bootstrap/model"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

const bootAddr = "10.0.0.99:9999"

// client is a bare node stand-in: a transport plus a codec, so the tests
// exercise the real wire rather than the server's internals.
type client struct {
	tr    *transport.MemTransport
	codec *pb.Codec
	addr  string
}

func newServer(t *testing.T, net *transport.MemNet, dataPath, psk string) *Server {
	t.Helper()
	s, err := New(Config{
		Port:        9999,
		NodeTimeout: time.Hour,
		DataPath:    dataPath,
		PSK:         psk,
		Transport:   net.Node(bootAddr),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	t.Cleanup(s.Stop)
	return s
}

func newClient(t *testing.T, net *transport.MemNet, addr, psk string) *client {
	t.Helper()
	c := &client{tr: net.Node(addr), codec: pb.NewCodec(psk, ""), addr: addr}
	t.Cleanup(func() { c.tr.Close() })
	return c
}

func (c *client) send(t *testing.T, kind pb.MessageKind, v interface{}) {
	t.Helper()
	pkt, err := c.codec.Encode(kind, v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := c.tr.Send(bootAddr, pkt); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func (c *client) awaitRoster(t *testing.T) pb.BootstrapRoster {
	t.Helper()
	select {
	case pkt := <-c.tr.Packets():
		kind, body, err := c.codec.Decode(pkt.Data)
		if err != nil {
			t.Fatalf("decode roster: %v", err)
		}
		if kind != pb.KindBootstrapRoster {
			t.Fatalf("kind = %s, want roster", kind)
		}
		var roster pb.BootstrapRoster
		if err := json.Unmarshal(body, &roster); err != nil {
			t.Fatalf("unmarshal roster: %v", err)
		}
		return roster
	case <-time.After(2 * time.Second):
		t.Fatal("no roster reply")
		return pb.BootstrapRoster{}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRegisterThenDiscover(t *testing.T) {
	net := transport.NewMemNet(1)
	s := newServer(t, net, "", "")

	alice := newClient(t, net, "10.0.0.1:3137", "")
	bob := newClient(t, net, "10.0.0.2:3137", "")

	alice.send(t, pb.KindBootstrapRegister, pb.BootstrapRegister{From: alice.addr, Nick: "alice"})
	waitFor(t, "alice to register", func() bool { return len(s.Model.GetNodes()) == 1 })

	bob.send(t, pb.KindBootstrapDiscover, pb.BootstrapDiscover{From: bob.addr})
	roster := bob.awaitRoster(t)

	if len(roster.Peers) != 1 {
		t.Fatalf("roster = %+v, want just alice", roster.Peers)
	}
	if roster.Peers[0].Addr != alice.addr || roster.Peers[0].Nick != "alice" {
		t.Fatalf("roster entry = %+v, want alice with her nickname", roster.Peers[0])
	}
}

// A node has no use for its own address in the roster, and adding it would make
// it gossip with itself.
func TestRosterExcludesRequester(t *testing.T) {
	net := transport.NewMemNet(2)
	newServer(t, net, "", "")
	alice := newClient(t, net, "10.0.0.1:3137", "")

	alice.send(t, pb.KindBootstrapRegister, pb.BootstrapRegister{From: alice.addr})
	alice.send(t, pb.KindBootstrapDiscover, pb.BootstrapDiscover{From: alice.addr})

	for _, p := range alice.awaitRoster(t).Peers {
		if p.Addr == alice.addr {
			t.Fatal("roster contains the requester")
		}
	}
}

// Asking for the roster is itself proof of life, so a joiner whose REGISTER was
// lost still ends up known.
func TestDiscoverRegistersTheAsker(t *testing.T) {
	net := transport.NewMemNet(3)
	s := newServer(t, net, "", "")
	alice := newClient(t, net, "10.0.0.1:3137", "")

	alice.send(t, pb.KindBootstrapDiscover, pb.BootstrapDiscover{From: alice.addr})
	alice.awaitRoster(t)

	waitFor(t, "the asker to be registered", func() bool {
		return len(s.Model.GetNodes()) == 1
	})
}

// The datagram roster is capped by the MTU; the stream path is not.
func TestDiscoverOverStream(t *testing.T) {
	net := transport.NewMemNet(4)
	s := newServer(t, net, "", "")
	// Enough peers that the encoded roster cannot fit in a datagram.
	const peers = 2000
	for i := 0; i < peers; i++ {
		// A distinct prefix from the requester's, whose entry the roster omits.
		s.Model.RegisterNode(fmt.Sprintf("172.%d.%d.%d:3137", i/65536, (i/256)%256, i%256), "node")
	}

	alice := newClient(t, net, "10.0.0.1:3137", "")
	conn, err := alice.tr.Dial(bootAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req, _ := alice.codec.Encode(pb.KindBootstrapDiscover, pb.BootstrapDiscover{From: alice.addr})
	go transport.WriteFrame(conn, req)

	conn.SetDeadline(time.Now().Add(2 * time.Second))
	frame, err := transport.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read roster frame: %v", err)
	}
	kind, body, err := alice.codec.Decode(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if kind != pb.KindBootstrapRoster {
		t.Fatalf("kind = %s, want roster", kind)
	}
	var roster pb.BootstrapRoster
	if err := json.Unmarshal(body, &roster); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(roster.Peers) != peers {
		t.Fatalf("roster = %d peers, want %d (a stream has no datagram ceiling)", len(roster.Peers), peers)
	}
	if len(frame) <= 65535 {
		t.Fatalf("frame = %d bytes; the test is no longer proving anything about size", len(frame))
	}
}

// Without a shared key anyone on the network could inject bogus peers.
func TestUnauthenticatedPacketsIgnored(t *testing.T) {
	net := transport.NewMemNet(5)
	s := newServer(t, net, "", "correct-key")

	intruder := newClient(t, net, "10.0.0.66:3137", "wrong-key")
	intruder.send(t, pb.KindBootstrapRegister, pb.BootstrapRegister{From: intruder.addr})

	time.Sleep(50 * time.Millisecond)
	if n := len(s.Model.GetNodes()); n != 0 {
		t.Fatalf("registered %d nodes from an unauthenticated packet", n)
	}
}

// A restarting rendezvous service keeps serving the roster it knew, instead of
// making every node wait out a re-registration before anyone can join.
func TestRosterSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roster.json")

	net1 := transport.NewMemNet(6)
	s1 := newServer(t, net1, path, "")
	s1.Model.RegisterNode("10.0.0.1:3137", "alice")
	s1.Stop()

	net2 := transport.NewMemNet(7)
	s2 := newServer(t, net2, path, "")
	infos := s2.Model.GetNodeInfos()
	if len(infos) != 1 || infos[0].Addr != "10.0.0.1:3137" || infos[0].Nick != "alice" {
		t.Fatalf("roster after restart = %+v, want alice", infos)
	}
	// The service was down, so nobody could have checked in: a restored node
	// must not be immediately stale.
	if removed := s2.Model.RemoveStaleNodes(); len(removed) != 0 {
		t.Fatalf("restored node evicted immediately: %+v", removed)
	}
}

func TestStaleNodesAreDropped(t *testing.T) {
	// Straight against the model: a zero timeout makes every node instantly
	// stale, and a running server's sweep loop would be reading the timeout
	// concurrently.
	m := model.NewModel(9999, 0, "")
	m.RegisterNode("10.0.0.1:3137", "alice")

	if removed := m.RemoveStaleNodes(); len(removed) != 1 {
		t.Fatalf("removed = %+v, want the stale node", removed)
	}
	if n := len(m.GetNodes()); n != 0 {
		t.Fatalf("nodes = %d after eviction, want 0", n)
	}
}
