package controller

import (
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// newRunningController builds a controller whose real tview app runs against a
// simulation screen, so handlers that call QueueUpdateDraw have a live event
// loop to drain. No UDP socket is bound (Start is never called).
func newRunningController(t *testing.T, dataPath string) *Controller {
	t.Helper()
	c := NewController(false, ":9999", ":3137", "self", dataPath)
	c.view.App.SetScreen(tcell.NewSimulationScreen(""))
	go func() { _ = c.view.App.Run() }()
	t.Cleanup(func() { c.view.App.Stop() })
	// Let the event loop start draining the update queue before we enqueue.
	time.Sleep(100 * time.Millisecond)
	return c
}

// runHandler runs a QueueUpdateDraw-touching handler off the test goroutine
// with a watchdog, so a stalled event loop fails fast instead of hanging.
func runHandler(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler timed out — event loop not draining updates")
	}
}

func mustListenUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return conn
}

// End-to-end receive path for graceful leave: a Goodbye packet for a known
// peer drops it from the node set and posts a "left" line to the chat log.
func TestHandleGoodbyeDropsPeer(t *testing.T) {
	c := newRunningController(t, "")

	const peer = ":3138"
	c.model.AddPeer(peer)
	c.model.SetNick(peer, "bob")

	body, err := json.Marshal(pb.Goodbye{From: peer, Nick: "bob"})
	if err != nil {
		t.Fatalf("marshal goodbye: %v", err)
	}
	runHandler(t, func() { c.handleGoodbye(body) })

	if c.model.HasPeer(peer) {
		t.Fatalf("peer %s still present after Goodbye", peer)
	}

	var left string
	for _, line := range c.model.ChatLog() {
		if strings.Contains(line, peer) && strings.Contains(line, "left") {
			left = line
			break
		}
	}
	if left == "" {
		t.Fatalf("no 'left' line for %s in chat log: %v", peer, c.model.ChatLog())
	}
}

// An unknown peer's Goodbye is ignored: no phantom "left" line, no panic.
func TestHandleGoodbyeUnknownPeerIsNoop(t *testing.T) {
	c := newRunningController(t, "")

	body, _ := json.Marshal(pb.Goodbye{From: ":4000", Nick: "ghost"})
	runHandler(t, func() { c.handleGoodbye(body) })

	if n := len(c.model.ChatLog()); n != 0 {
		t.Fatalf("chat log = %d lines, want 0 for unknown-peer Goodbye: %v", n, c.model.ChatLog())
	}
}

// Replication receive path: a KindKV update is merged into the local store
// and signals a UI refresh. handleKV touches no tview widgets, so it needs no
// running app.
func TestHandleKVAppliesUpdate(t *testing.T) {
	c := NewController(false, ":9999", ":3137", "self", "")

	u := pb.KVUpdate{Action: pb.KVSet, Key: "k", Value: "v", Version: pb.Version{Counter: 3, Node: ":peer"}}
	body, _ := json.Marshal(u)

	updateCh := make(chan struct{}, 1)
	c.handleKV(body, updateCh)

	if got, ok := c.model.Store.Get("k"); !ok || got != "v" {
		t.Fatalf("handleKV did not apply update: (%q, %v)", got, ok)
	}
	select {
	case <-updateCh:
	default:
		t.Fatal("handleKV did not signal a UI refresh")
	}
}

// Joiner side of chat-history sync: a StateResponse carrying chat replaces the
// store and prepends the peer's history ahead of lines logged locally while
// the sync was in flight.
func TestStateResponseAppliesChatHistoryOnJoin(t *testing.T) {
	c := newRunningController(t, "")
	// A local system line arrives before the state sync completes.
	c.model.AppendChat("» :4000 joined")

	resp := pb.StateResponse{
		KV: []pb.KVUpdate{
			{Action: pb.KVSet, Key: "k", Value: "v", Version: pb.Version{Counter: 1, Node: ":peer"}},
		},
		Chat: []string{"peer-line-1", "peer-line-2"},
	}
	body, _ := json.Marshal(resp)

	updateCh := make(chan struct{}, 1)
	runHandler(t, func() { c.handleStateResponse(body, updateCh) })

	if got, ok := c.model.Store.Get("k"); !ok || got != "v" {
		t.Fatalf("store not merged from state response: (%q, %v)", got, ok)
	}
	want := []string{"peer-line-1", "peer-line-2", "» :4000 joined"}
	if got := c.model.ChatLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("chat after join = %v, want %v", got, want)
	}
}

// Responder side of chat-history sync, over real UDP: a StateRequest yields a
// StateResponse datagram carrying both the KV snapshot and recent chat.
func TestStateRequestIncludesChatHistory(t *testing.T) {
	responder := newRunningController(t, "")
	responder.model.Store.Set("shared", "value")
	responder.model.AppendChat("earlier message")

	joiner := mustListenUDP(t)
	defer joiner.Close()

	reqBody, _ := json.Marshal(pb.StateRequest{From: joiner.LocalAddr().String()})
	responder.handleStateRequest(reqBody)

	_ = joiner.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 65535)
	n, _, err := joiner.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read state response: %v", err)
	}
	kind, body, err := pb.SplitKind(buf[:n])
	if err != nil {
		t.Fatalf("split kind: %v", err)
	}
	if kind != pb.KindStateResponse {
		t.Fatalf("kind = %d, want StateResponse (%d)", kind, pb.KindStateResponse)
	}
	var resp pb.StateResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	var sharedVal string
	for _, u := range resp.KV {
		if u.Key == "shared" {
			sharedVal = u.Value
		}
	}
	if sharedVal != "value" {
		t.Fatalf("KV snapshot not carried: %v", resp.KV)
	}
	joined := strings.Join(resp.Chat, "\n")
	if !strings.Contains(joined, "earlier message") {
		t.Fatalf("chat history not carried in response: %v", resp.Chat)
	}
}
