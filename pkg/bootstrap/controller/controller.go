package controller

import (
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
	v.Frame.AddText(fmt.Sprintf("Rezoagve Bootstrap 0.0.1 PoC"), true, tview.AlignCenter, tcell.ColorGreen)
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

func (c *Controller) fill(nodes []string) {
	c.view.App.QueueUpdateDraw(func() {
		c.view.List.Clear()
		c.view.List.SetMainTextColor(tcell.Color31)
		for _, node := range nodes {
			c.view.List.AddItem(node, node, 0, func() {
			})
		}
	})
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
	if err == nil {
		log.Errorf("Error starting UDP server: %s", err)
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
		c.view.List.SetChangedFunc(func(i int, s string, s2 string, r rune) {
			_, cur := c.view.List.GetItemText(i)
			cur = strings.TrimSpace(cur)
		})
		c.setInput()

		go func() {
			for {
				select {
				case _, ok := <-updateCh:
					if ok {
						c.fill(c.model.GetNodes())
					}
				default:
				}
			}
		}()
		return c.view.App.Run()
	} else {
		return err
	}
}
