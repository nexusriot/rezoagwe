package model

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

func newModel(t *testing.T, dataPath string) *Model {
	t.Helper()
	return NewModel(Config{
		BootstrapAddrs: []string{":9999"},
		NodeAddr:       "127.0.0.1:3137",
		Nick:           "self",
		DataPath:       dataPath,
	})
}

// The Lamport clock must be persisted: after a restart, a new local write has
// to outrank writes the node made before restarting.
func TestClockSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.json")

	m1 := newModel(t, path)
	m1.Store.Set("k", "v1")
	last := m1.Store.Set("k", "v2")

	m2 := newModel(t, path)
	u := m2.Store.Set("k", "v3")
	if u.Version.Counter <= last.Version.Counter {
		t.Fatalf("post-restart write counter = %d, want > %d (clock not restored)",
			u.Version.Counter, last.Version.Counter)
	}
}

// Identity is persisted, so a restarted node keeps stamping its writes with the
// same version tiebreak instead of behaving like a brand new writer.
func TestNodeIDSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.json")

	m1 := newModel(t, path)
	if m1.NodeID == "" {
		t.Fatal("node id empty")
	}
	m2 := newModel(t, path)
	if m2.NodeID != m1.NodeID {
		t.Fatalf("node id changed across restart: %q → %q", m1.NodeID, m2.NodeID)
	}
	u := m2.Store.Set("k", "v")
	if u.Version.Node != m1.NodeID {
		t.Fatalf("write stamped with %q, want the persisted id %q", u.Version.Node, m1.NodeID)
	}
}

// A node that has never written anything still has to persist its identity, or
// it mints a new one on every restart.
func TestNodeIDPersistedWithoutWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.json")
	m := newModel(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not written for a fresh node: %v", err)
	}
	if NewModel(Config{NodeAddr: "127.0.0.1:3137", DataPath: path}).NodeID != m.NodeID {
		t.Fatal("identity not restored for a node that never wrote")
	}
}

func TestChatSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.json")

	m1 := newModel(t, path)
	m1.AppendChat(pb.ChatEntry{TS: 1, Sender: ":3138", Nick: "bob", Text: "hello"})

	m2 := newModel(t, path)
	log := m2.ChatLog()
	if len(log) != 1 || log[0].Text != "hello" || log[0].Nick != "bob" {
		t.Fatalf("chat not restored: %+v", log)
	}
}

func TestValidPeerAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:3137", true},
		{":3137", true}, // a wildcard-bound node advertises this
		{"host.example:1", true},
		{"[::1]:3137", true},
		{"garbage", false},
		{"127.0.0.1:0", false},
		{"127.0.0.1:99999", false},
		{"127.0.0.1:abc", false},
		{"127.0.0.1", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := ValidPeerAddr(tc.addr); got != tc.want {
			t.Errorf("ValidPeerAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// Gossip is hearsay: a malformed address must be refused at the door rather
// than sit in the node list until eviction.
func TestAddPeerRejectsGarbage(t *testing.T) {
	m := newModel(t, "")
	if m.AddPeer("not-an-address") {
		t.Fatal("accepted a malformed peer address")
	}
	if len(m.GetNodes()) != 0 {
		t.Fatalf("nodes = %v, want empty", m.GetNodes())
	}
}

// A node bound to the wildcard address must recognise the loopback form of its
// own address, or it gossips itself into its own peer list.
func TestAddPeerRejectsSelfAliases(t *testing.T) {
	m := NewModel(Config{NodeAddr: ":3137"})
	for _, addr := range []string{":3137", "127.0.0.1:3137", "localhost:3137"} {
		if m.AddPeer(addr) {
			t.Fatalf("added self via alias %q", addr)
		}
	}
	if len(m.GetNodes()) != 0 {
		t.Fatalf("nodes = %v, want empty", m.GetNodes())
	}
}

func TestEvictStalePeers(t *testing.T) {
	m := newModel(t, "")
	m.AddPeer("127.0.0.1:3138")
	m.SetNick("127.0.0.1:3138", "bob")

	if got := m.EvictStalePeers(time.Hour); len(got) != 0 {
		t.Fatalf("evicted a fresh peer: %v", got)
	}
	evicted := m.EvictStalePeers(0)
	if len(evicted) != 1 || evicted[0].Addr != "127.0.0.1:3138" || evicted[0].Nick != "bob" {
		t.Fatalf("evicted = %+v, want the peer with its nick", evicted)
	}
	if m.HasPeer("127.0.0.1:3138") {
		t.Fatal("peer still present after eviction")
	}
}

// Attributing a write means mapping the version's node id back to a nickname.
func TestWriterName(t *testing.T) {
	m := newModel(t, "")
	m.AddPeer("127.0.0.1:3138")
	m.SetNick("127.0.0.1:3138", "bob")
	m.SetNodeID("peer-id", "127.0.0.1:3138")

	if got := m.WriterName(m.NodeID); got != "you" {
		t.Fatalf("own writer name = %q, want you", got)
	}
	if got := m.WriterName("peer-id"); got != "bob" {
		t.Fatalf("peer writer name = %q, want bob", got)
	}
	if got := m.WriterName("abcdefgh-rest-of-a-uuid"); got != "abcdefgh" {
		t.Fatalf("unknown writer name = %q, want the shortened id", got)
	}
}

// Removing a peer must also forget its id mapping, or a stale id keeps
// resolving to an address nobody is at.
func TestRemovePeerForgetsID(t *testing.T) {
	m := newModel(t, "")
	m.AddPeer("127.0.0.1:3138")
	m.SetNodeID("peer-id", "127.0.0.1:3138")
	m.RemovePeer("127.0.0.1:3138")

	if got := m.AddrOfID("peer-id"); got != "" {
		t.Fatalf("id still resolves to %q after the peer was removed", got)
	}
}

func TestPrependChatKeepsLocalLinesLast(t *testing.T) {
	m := newModel(t, "")
	m.AppendChat(pb.ChatEntry{Text: "local", TS: 3, Kind: pb.ChatSystem})
	m.PrependChat([]pb.ChatEntry{{Text: "peer-1", TS: 1}, {Text: "peer-2", TS: 2}})

	log := m.ChatLog()
	want := []string{"peer-1", "peer-2", "local"}
	if len(log) != len(want) {
		t.Fatalf("chat = %d lines, want %d", len(log), len(want))
	}
	for i, w := range want {
		if log[i].Text != w {
			t.Fatalf("chat[%d] = %q, want %q", i, log[i].Text, w)
		}
	}
}

// A snapshot repeats history the node already holds: a joiner that syncs from
// two peers, or asks one peer twice, was shown the same conversation over and
// over.
func TestPrependChatDoesNotDuplicateHistory(t *testing.T) {
	m := newModel(t, "")
	history := []pb.ChatEntry{
		{Text: "one", TS: 1, Sender: "10.0.0.2:3137"},
		{Text: "two", TS: 2, Sender: "10.0.0.2:3137"},
	}
	m.PrependChat(history)
	m.PrependChat(history)
	m.PrependChat(history)

	if got := len(m.ChatLog()); got != 2 {
		t.Fatalf("chat = %d lines after three identical snapshots, want 2: %v", got, m.ChatLog())
	}
}

// Merged history has to end up in time order: a snapshot pulled after a local
// line was written must not push that line to the end of the log.
func TestPrependChatOrdersByTime(t *testing.T) {
	m := newModel(t, "")
	m.AppendChat(pb.ChatEntry{Text: "local-early", TS: 5})
	m.PrependChat([]pb.ChatEntry{{Text: "peer-later", TS: 9}, {Text: "peer-earlier", TS: 1}})

	want := []string{"peer-earlier", "local-early", "peer-later"}
	log := m.ChatLog()
	if len(log) != len(want) {
		t.Fatalf("chat = %d lines, want %d: %v", len(log), len(want), log)
	}
	for i, w := range want {
		if log[i].Text != w {
			t.Fatalf("chat[%d] = %q, want %q", i, log[i].Text, w)
		}
	}
}
