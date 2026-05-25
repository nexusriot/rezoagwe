package controller

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/golang/protobuf/proto"
	"github.com/rivo/tview"
	log "github.com/sirupsen/logrus"

	"github.com/nexusriot/rezoagwe/pkg/bootstrap/model"
	"github.com/nexusriot/rezoagwe/pkg/bootstrap/view"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

type Controller struct {
	debug bool
	view  *view.View
	model *model.Model
}

func NewController(
	debug bool,
	broadcastPort int,
	nodeTimeout time.Duration,

) *Controller {
	m := model.NewModel(broadcastPort, nodeTimeout)
	v := view.NewView()
	v.Frame.AddText("Rezoagwe Bootstrap Node v0.0.3", true, tview.AlignCenter, tcell.ColorGreen)
	controller := Controller{
		debug: debug,
		view:  v,
		model: m,
	}
	return &controller
}

func (c *Controller) HandleBootstrap(conn *net.UDPConn, wg *sync.WaitGroup, uch chan<- struct{}) {
	buf := make([]byte, 1024)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Errorf("Error reading from UDP: %s", err)
			continue
		}
		loaded := new(pb.BootstrapMessage)
		err = proto.Unmarshal(buf[:n], loaded)
		if err != nil {
			// TODO: -> log
			continue
		}

		if loaded.Action == pb.BootstrapAction_DISCOVER {
			nodes := c.model.GetNodes()
			response := strings.Join(nodes, ",")
			conn.WriteToUDP([]byte(response), addr)

		} else if loaded.Action == pb.BootstrapAction_REGISTER {
			nodeAddress := loaded.Host.GetHost()
			c.model.RegisterNode(nodeAddress)
			log.Debugf("Nodes: %s\n", c.model.GetNodes())
			uch <- struct{}{}
		}
	}
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
	infos := c.model.GetNodeInfos()
	timeout := c.model.NodeTimeout
	c.view.App.QueueUpdateDraw(func() {
		c.view.List.Clear()
		c.view.List.SetMainTextColor(tcell.Color31)
		for _, n := range infos {
			age := time.Since(n.LastSeen)
			secondary := fmt.Sprintf("last seen %s ago", formatAge(age))
			if age > timeout*2/3 {
				secondary += " (stale soon)"
			}
			c.view.List.AddItem(n.Addr, secondary, 0, nil)
		}
	})
}

func (c *Controller) refreshStatus() {
	infos := c.model.GetNodeInfos()
	uptime := formatAge(time.Since(c.model.StartedAt))

	stateTag := "[green]LISTENING[-]"
	line := fmt.Sprintf(
		" %s  [white]udp/:%d[-]  [white]nodes:[-][cyan]%d[-]  [white]timeout:[-][cyan]%s[-]  [white]up:[-][cyan]%s[-]",
		stateTag,
		c.model.BroadcastPort,
		len(infos),
		c.model.NodeTimeout,
		uptime,
	)

	c.view.App.QueueUpdateDraw(func() {
		c.view.Status.Clear()
		fmt.Fprint(c.view.Status, line)
	})
}

// statusLoop repaints the status bar and the nodes list once per second
// so last-seen ages and stale-node evictions stay current.
//
// The first paint runs from this goroutine on purpose: refreshStatus and
// fill go through QueueUpdateDraw, which is synchronous and would deadlock
// if called on the main goroutine before App.Run() starts the event loop.
func (c *Controller) statusLoop() {
	c.refreshStatus()
	c.fill()
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for range t.C {
		c.refreshStatus()
		c.fill()
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
	log.Debugf("exit...")
	c.view.App.Stop()
}

func (c *Controller) Start() error {

	addr := net.UDPAddr{
		Port: c.model.BroadcastPort,
		// Todo: run on 127.0.0.1
		IP: net.ParseIP("0.0.0.0"),
	}
	var wg sync.WaitGroup
	updateCh := make(chan struct{})
	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		log.Errorf("Error starting UDP server: %s", err)
		return err
	}
	defer conn.Close()
	go func() {
		for {
			c.model.RemoveStaleNodes()
			time.Sleep(c.model.NodeTimeout / 2)
		}
	}()
	log.Debugf("Bootstrap node is listening on port %d\n", c.model.BroadcastPort)
	wg.Add(1)
	go c.HandleBootstrap(conn, &wg, updateCh)
	c.setInput()

	// Repaint immediately on every REGISTER so a new node shows up at once
	// instead of waiting for the next 1 s status tick.
	go func() {
		for range updateCh {
			c.fill()
		}
	}()
	// Periodic refresh: status bar + last-seen ages + stale evictions.
	go c.statusLoop()
	return c.view.App.Run()
}
