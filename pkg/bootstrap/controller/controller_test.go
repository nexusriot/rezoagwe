package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/nexusriot/rezoagwe/pkg/bootstrap/server"
	"github.com/nexusriot/rezoagwe/pkg/transport"
)

func newRunning(t *testing.T) *Controller {
	t.Helper()
	net := transport.NewMemNet(1)
	c, err := NewController(server.Config{
		Port:        9999,
		NodeTimeout: time.Hour,
		Transport:   net.Node("10.0.0.99:9999"),
	}, "test")
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	c.screen = tcell.NewSimulationScreen("")
	c.view.App.SetScreen(c.screen)
	return c
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

// Start hands the main goroutine to the tview event loop, so nothing before it
// may wait on that loop. The symptom of getting this wrong is a service that
// starts with no visible TUI.
func TestStartRunsTheEventLoop(t *testing.T) {
	c := newRunning(t)

	done := make(chan error, 1)
	go func() { done <- c.Start() }()

	want := []string{"Rezoagwe Bootstrap Node", "Nodes", "LISTENING"}
	rendered, ok := waitForScreen(c.view.App, c.screen, 5*time.Second, want...)
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

// Shutdown runs on the event loop and waits for the server's goroutines. If a
// roster change repainted synchronously from one of those goroutines, the two
// would wait on each other and Ctrl+Q would hang.
func TestShutdownIsNotBlockedByRosterChurn(t *testing.T) {
	c := newRunning(t)

	done := make(chan error, 1)
	go func() { done <- c.Start() }()
	time.Sleep(200 * time.Millisecond)

	// Register continuously while quitting, so a change notification is in
	// flight exactly when shutdown begins.
	stopChurn := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stopChurn:
				return
			default:
				c.server.Model.RegisterNode("10.0.0.1:3137", "alice")
				c.signalChange()
			}
		}
	}()

	c.Stop()
	close(stopChurn)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown deadlocked against a roster repaint")
	}
}

// The roster has to reach the screen: a new node should appear without waiting
// for the next status tick.
func TestRosterIsPainted(t *testing.T) {
	c := newRunning(t)
	go func() { _ = c.Start() }()
	time.Sleep(200 * time.Millisecond)

	c.server.Model.RegisterNode("10.0.0.1:3137", "alice")
	c.signalChange()

	rendered, ok := waitForScreen(c.view.App, c.screen, 3*time.Second, "10.0.0.1:3137 — alice", "last seen")
	c.Stop()
	if !ok {
		t.Fatalf("registered node never appeared on screen:\n%s", rendered)
	}
}
