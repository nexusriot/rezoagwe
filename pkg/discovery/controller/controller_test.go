package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/nexusriot/rezoagwe/pkg/discovery/node"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// newRunningController builds a controller whose real tview app runs against a
// simulation screen, so handlers that call QueueUpdateDraw have a live event
// loop to drain. The node engine sits on an in-memory network and is never
// started, so nothing reaches a socket.
func newRunningController(t *testing.T) *Controller {
	t.Helper()
	net := transport.NewMemNet(1)
	c, err := NewController(node.Config{
		NodeAddr:  "10.0.0.1:3137",
		Nick:      "self",
		Transport: net.Node("10.0.0.1:3137"),
	}, "test")
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	c.view.App.SetScreen(tcell.NewSimulationScreen(""))
	go func() { _ = c.view.App.Run() }()
	t.Cleanup(func() { c.view.App.Stop() })
	// Let the event loop start draining the update queue before we enqueue.
	time.Sleep(100 * time.Millisecond)
	return c
}

// runHandler runs a refresh helper off the test goroutine with a watchdog, so a
// stalled event loop fails fast instead of hanging. These helpers queue their
// own widget updates, so they are safe to call from anywhere.
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

// onUI runs fn on the tview event loop. Anything that touches a widget
// directly — opening a modal, reading a list row — has to go through here: in
// the running program those paths are key handlers, which the event loop
// already owns.
func onUI(t *testing.T, c *Controller, fn func()) {
	t.Helper()
	runHandler(t, func() { c.view.App.QueueUpdateDraw(fn) })
}

func TestKeyListRendersStore(t *testing.T) {
	c := newRunningController(t)
	c.node.Set("alpha", "one", 0)
	c.node.Set("beta", "two", time.Minute)

	runHandler(t, c.fillStore)

	var count int
	var main, secondary string
	onUI(t, c, func() {
		count = c.view.List.GetItemCount()
		if count > 1 {
			main, secondary = c.view.List.GetItemText(1)
		}
	})

	if count != 2 {
		t.Fatalf("list has %d items, want 2", count)
	}
	if !strings.HasPrefix(main, "beta") {
		t.Fatalf("second row = %q, want beta", main)
	}
	// A key with a TTL is marked, and its remaining lifetime shown.
	if !strings.Contains(main, ttlMarker) {
		t.Fatalf("row %q is missing the expiry marker", main)
	}
	if !strings.Contains(secondary, "expires in") {
		t.Fatalf("row detail = %q, want a remaining lifetime", secondary)
	}
}

// The marker must not become part of the key when the row is acted on.
func TestSelectedKeyStripsExpiryMarker(t *testing.T) {
	c := newRunningController(t)
	c.node.Set("beta", "two", time.Minute)
	runHandler(t, c.fillStore)

	var got string
	onUI(t, c, func() { got = c.currentSelectedKey() })
	if got != "beta" {
		t.Fatalf("selected key = %q, want beta", got)
	}
}

func TestFilterNarrowsTheList(t *testing.T) {
	c := newRunningController(t)
	c.node.Set("alpha", "one", 0)
	c.node.Set("beta", "two", 0)

	runHandler(t, func() { c.setFilter("alph") })
	var count int
	onUI(t, c, func() { count = c.view.List.GetItemCount() })
	if count != 1 {
		t.Fatalf("filtered list has %d items, want 1", count)
	}

	runHandler(t, func() { c.setFilter("two") }) // filters on values too
	onUI(t, c, func() { count = c.view.List.GetItemCount() })
	if count != 1 {
		t.Fatalf("value filter matched %d items, want 1", count)
	}
}

func TestFeedRendersChatAndTogglesToActivity(t *testing.T) {
	c := newRunningController(t)
	c.node.Model.AppendChat(pb.ChatEntry{
		TS: time.Now().Unix(), Sender: "10.0.0.2:3137", Nick: "bob", Text: "hello",
	})

	runHandler(t, c.fillFeed)
	var body string
	onUI(t, c, func() { body = c.view.Feed.GetText(true) })
	if !strings.Contains(body, "bob") || !strings.Contains(body, "hello") {
		t.Fatalf("chat pane = %q", body)
	}

	runHandler(t, func() { c.toggleFeed() })
	if !c.activityShown() {
		t.Fatal("toggle did not switch to the activity feed")
	}
	onUI(t, c, func() { body = c.view.Feed.GetText(true) })
	if strings.Contains(body, "hello") {
		t.Fatalf("activity view still shows chat: %q", body)
	}
}

// Own messages, emotes and system lines must be distinguishable.
func TestRenderChatVariants(t *testing.T) {
	c := newRunningController(t)
	now := time.Now().Unix()

	own := c.renderChat(pb.ChatEntry{TS: now, Sender: c.node.Addr(), Text: "mine"})
	if !strings.Contains(own, "you") {
		t.Fatalf("own message = %q, want a 'you' tag", own)
	}
	system := c.renderChat(pb.ChatEntry{TS: now, Text: "bob joined", Kind: pb.ChatSystem})
	if !strings.Contains(system, "»") {
		t.Fatalf("system line = %q", system)
	}
	action := c.renderChat(pb.ChatEntry{
		TS: now, Sender: "10.0.0.2:3137", Nick: "bob", Text: "waves", Kind: pb.ChatAction,
	})
	if !strings.Contains(action, "*") || !strings.Contains(action, "waves") {
		t.Fatalf("action line = %q", action)
	}
	dm := c.renderChat(pb.ChatEntry{
		TS: now, Sender: "10.0.0.2:3137", Nick: "bob", Text: "psst", Kind: pb.ChatDirect,
	})
	if !strings.Contains(dm, "dm") {
		t.Fatalf("direct message line = %q", dm)
	}
}

// Colour tags in a message must not be interpreted as markup.
func TestChatMarkupIsEscaped(t *testing.T) {
	c := newRunningController(t)
	line := c.renderChat(pb.ChatEntry{
		TS: time.Now().Unix(), Sender: "10.0.0.2:3137", Text: "[red]not a colour[-]",
	})
	if !strings.Contains(line, "[red[") {
		t.Fatalf("message markup was not escaped: %q", line)
	}
}

func TestHistoryModalListsVersions(t *testing.T) {
	c := newRunningController(t)
	c.node.Set("k", "v1", 0)
	c.node.Set("k", "v2", 0)
	runHandler(t, c.fillStore)

	var open bool
	onUI(t, c, func() {
		c.history()
		open = c.view.Pages.HasPage("modal")
		c.closeModal()
	})
	if !open {
		t.Fatal("history modal did not open")
	}
}

func TestMetricsModalOpens(t *testing.T) {
	c := newRunningController(t)
	c.node.Set("k", "v", 0)

	var open bool
	onUI(t, c, func() {
		c.metrics()
		open = c.view.Pages.HasPage("modal")
		c.closeModal()
	})
	if !open {
		t.Fatal("metrics modal did not open")
	}
}

func TestHelpModalOpens(t *testing.T) {
	c := newRunningController(t)
	var open bool
	onUI(t, c, func() {
		c.help()
		open = c.view.Pages.HasPage("modal")
		c.closeModal()
	})
	if !open {
		t.Fatal("help modal did not open")
	}
}

// A guarded save against a stale version must be refused and reported, not
// applied silently.
func TestGuardedWriteRefusedOnStaleVersion(t *testing.T) {
	c := newRunningController(t)
	c.node.Set("k", "current", 0)
	stale := pb.Version{Counter: 1, Node: "someone-else"}

	runHandler(t, func() { c.store("k", "overwrite", 0, true, &stale) })

	if v, _ := c.node.Get("k"); v != "current" {
		t.Fatalf("value = %q, want the guarded write to be refused", v)
	}
	var reported bool
	for _, e := range c.node.Chat() {
		if strings.Contains(e.Text, "refused") {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("refusal not reported in the feed: %+v", c.node.Chat())
	}
}

func TestStatusBarShowsPeersAndKeys(t *testing.T) {
	c := newRunningController(t)
	c.node.Set("k", "v", 0)
	c.node.Model.AddPeer("10.0.0.2:3137")

	runHandler(t, c.refreshStatus)
	var status string
	onUI(t, c, func() { status = c.view.Status.GetText(true) })

	if !strings.Contains(status, "CONNECTED") {
		t.Fatalf("status = %q, want CONNECTED with a peer known", status)
	}
	if !strings.Contains(status, "keys:1") {
		t.Fatalf("status = %q, want the key count", status)
	}
}
