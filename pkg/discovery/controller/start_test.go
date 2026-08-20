package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/rezoagwe/pkg/discovery/node"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

// Start is the one path every user takes, and it is the one path that cannot be
// exercised by driving handlers directly: it paints, wires input, and only then
// hands the main goroutine to the tview event loop. Anything that waits on that
// loop *before* App.Run() deadlocks, and the symptom is a node that starts with
// no visible TUI at all.
func TestStartRunsTheEventLoop(t *testing.T) {
	net := transport.NewMemNet(1)
	c, err := NewController(node.Config{
		NodeAddr:          "10.0.0.1:3137",
		Nick:              "self",
		Transport:         net.Node("10.0.0.1:3137"),
		GossipInterval:    time.Hour,
		HeartbeatInterval: time.Hour,
		EvictThreshold:    time.Hour,
		SweepInterval:     time.Hour,
	}, "test")
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	screen := tcell.NewSimulationScreen("")
	c.view.App.SetScreen(screen)

	done := make(chan error, 1)
	go func() { done <- c.Start() }()

	// The screen is read directly rather than through QueueUpdate: a probe that
	// went through the event loop would itself block when the bug is present,
	// turning a clear assertion failure into a hung test.
	want := []string{"Rezoagwe Discovery Node", "Keys", "Nodes", "Chat", "DEGRADED"}
	rendered, ok := waitForScreen(c.view.App, screen, 5*time.Second, want...)
	if !ok {
		t.Fatalf("TUI never drew %v — Start blocked before the event loop began.\nScreen:\n%s",
			want, rendered)
	}

	c.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

// waitForScreen waits until every fragment has been drawn, returning the last
// frame it saw either way.
//
// The cells are read *on* the tview event-loop goroutine — the same goroutine
// that draws them, since tcell hands out its live cell buffer rather than a copy
// — and each read is bounded, so an event loop that never starts fails the test
// promptly instead of hanging it.
func waitForScreen(app *tview.Application, screen tcell.SimulationScreen, timeout time.Duration, fragments ...string) (string, bool) {
	rendered, alive := readScreen(app, screen, timeout)
	if !alive {
		return "", false
	}
	deadline := time.Now().Add(timeout)
	for {
		missing := false
		for _, f := range fragments {
			if !strings.Contains(rendered, f) {
				missing = true
				break
			}
		}
		if !missing {
			return rendered, true
		}
		if time.Now().After(deadline) {
			return rendered, false
		}
		time.Sleep(20 * time.Millisecond)
		if rendered, alive = readScreen(app, screen, timeout); !alive {
			return rendered, false
		}
	}
}

func readScreen(app *tview.Application, screen tcell.SimulationScreen, timeout time.Duration) (string, bool) {
	ch := make(chan string, 1)
	go func() {
		var text string
		app.QueueUpdate(func() { text = screenText(screen) })
		ch <- text
	}()
	select {
	case text := <-ch:
		return text, true
	case <-time.After(timeout):
		return "", false
	}
}

// screenText flattens the drawn cells into lines, so a test can assert on what
// a user would actually see.
func screenText(screen tcell.SimulationScreen) string {
	cells, width, height := screen.GetContents()
	var b strings.Builder
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			runes := cells[y*width+x].Runes
			if len(runes) == 0 {
				b.WriteRune(' ')
				continue
			}
			b.WriteRune(runes[0])
		}
		b.WriteRune('\n')
	}
	return b.String()
}
