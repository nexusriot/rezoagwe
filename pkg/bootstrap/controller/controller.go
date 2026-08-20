package controller

import (
	"fmt"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	log "github.com/sirupsen/logrus"

	"github.com/nexusriot/rezoagwe/pkg/bootstrap/server"
	"github.com/nexusriot/rezoagwe/pkg/bootstrap/view"
)

// Controller drives the bootstrap TUI over a running server.
type Controller struct {
	view    *view.View
	server  *server.Server
	cluster string
	port    int

	// Roster changes arrive on the server's packet goroutines and are coalesced
	// through a capacity-1 channel, because repainting blocks: QueueUpdateDraw
	// waits for the tview event loop, and Ctrl+Q runs the server shutdown *on*
	// that loop. A direct repaint from a packet handler could therefore land
	// exactly while shutdown waits for that handler to finish, and the two would
	// wait on each other forever.
	changed chan struct{}
	done    chan struct{}

	// screen is set only by tests, which drive the TUI against a simulation
	// screen and read the drawn cells back.
	screen tcell.SimulationScreen

	stopOnce sync.Once
}

func NewController(cfg server.Config, version string) (*Controller, error) {
	srv, err := server.New(cfg)
	if err != nil {
		return nil, err
	}
	v := view.NewView()
	v.Frame.AddText("Rezoagwe Bootstrap Node "+version, true, tview.AlignCenter, tcell.ColorGreen)
	c := &Controller{
		view:    v,
		server:  srv,
		cluster: cfg.Cluster,
		port:    cfg.Port,
		changed: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	// Repaint as soon as the roster changes, so a new node shows up at once
	// instead of waiting for the next status tick.
	srv.OnChange(c.signalChange)
	return c, nil
}

// formatAge renders a duration as "1h02m03s" / "2m05s" / "9s".
func formatAge(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// fill repaints the nodes list, showing each node's last-seen age as
// secondary text. Nodes close to the stale timeout are shown in red.
func (c *Controller) fill() {
	infos := c.server.Model.GetNodeInfos()
	timeout := c.server.Model.NodeTimeout
	c.view.App.QueueUpdateDraw(func() {
		c.view.List.Clear()
		c.view.List.SetMainTextColor(tcell.Color31)
		for _, n := range infos {
			age := time.Since(n.LastSeen)
			label := n.Addr
			if n.Nick != "" {
				label = fmt.Sprintf("%s — %s", n.Addr, n.Nick)
			}
			secondary := fmt.Sprintf("last seen %s ago", formatAge(age))
			if age > timeout*2/3 {
				secondary += " (stale soon)"
			}
			c.view.List.AddItem(label, secondary, 0, nil)
		}
	})
}

func (c *Controller) signalChange() {
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *Controller) refresh() {
	c.fill()
	c.refreshStatus()
}

// refreshLoop paints the first frame and repaints whenever the roster changes.
// It runs on its own goroutine so blocking until App.Run() starts draining the
// update queue is harmless.
func (c *Controller) refreshLoop() {
	c.refresh()
	for {
		select {
		case <-c.done:
			return
		case <-c.changed:
			c.refresh()
		}
	}
}

func (c *Controller) refreshStatus() {
	infos := c.server.Model.GetNodeInfos()
	uptime := formatAge(time.Since(c.server.Model.StartedAt))

	line := fmt.Sprintf(
		" [green]LISTENING[-]  [white]udp+tcp/:%d[-]  [white]cluster:[-][yellow]%s[-]  [white]nodes:[-][cyan]%d[-]  [white]timeout:[-][cyan]%s[-]  [white]up:[-][cyan]%s[-]",
		c.port,
		c.cluster,
		len(infos),
		c.server.Model.NodeTimeout,
		uptime,
	)

	c.view.App.QueueUpdateDraw(func() {
		c.view.Status.Clear()
		fmt.Fprint(c.view.Status, line)
	})
}

// statusLoop repaints the status bar and the nodes list once per second
// so last-seen ages and stale-node evictions stay current.
func (c *Controller) statusLoop() {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			c.refresh()
		}
	}
}

func (c *Controller) setInput() {
	c.view.App.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyCtrlQ:
			c.Stop()
			return nil
		}
		return event
	})
}

func (c *Controller) Stop() {
	c.stopOnce.Do(func() {
		log.Debugf("exit...")
		close(c.done)
		c.server.Stop()
		c.view.App.Stop()
	})
}

func (c *Controller) Start() error {
	c.server.Start()
	c.setInput()
	// The first paint has to come from a goroutine: it goes through
	// QueueUpdateDraw, which is synchronous and would deadlock if called on the
	// main goroutine before App.Run() starts the event loop.
	go c.refreshLoop()
	go c.statusLoop()
	return c.view.App.Run()
}
