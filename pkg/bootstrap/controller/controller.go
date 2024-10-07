package controller

import (
	"github.com/gdamore/tcell/v2"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"

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
	controller := Controller{
		debug: debug,
		view:  v,
		model: m,
	}
	return &controller
}

func (c *Controller) HandleBootstrap(conn *net.UDPConn, wg *sync.WaitGroup) {
	buf := make([]byte, 1024)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			//fmt.Println("Error reading from UDP:", err)
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
			conn.WriteToUDP([]byte("REGISTERED"), addr)
			//fmt.Printf("Nodes: %s\n", c.model.GetNodes())
			c.fill()
		}
	}
}

func (c *Controller) fill() {
	c.view.List.Clear()
	for _, node := range c.model.GetNodes() {
		c.view.List.SetMainTextColor(tcell.Color31)
		c.view.List.AddItem(node, node, 0, func() {
		})
	}

}

func (c *Controller) Start() error {
	addr := net.UDPAddr{
		Port: c.model.BroadcastPort,
		// Todo: run on 127.0.0.1
		IP: net.ParseIP("0.0.0.0"),
	}
	var wg sync.WaitGroup
	conn, err := net.ListenUDP("udp", &addr)
	if err == nil {
		//fmt.Println("Error starting UDP server:", err)
		defer conn.Close()
		go func() {
			for {
				c.model.RemoveStaleNodes()
				time.Sleep(c.model.NodeTimeout / 2)
			}
		}()
		//fmt.Printf("Bootstrap node is listening on port %d\n", bn.broadcastPort)
		wg.Add(1)
		go c.HandleBootstrap(conn, &wg)
		c.view.List.SetChangedFunc(func(i int, s string, s2 string, r rune) {
			_, cur := c.view.List.GetItemText(i)
			cur = strings.TrimSpace(cur)
		})
		c.fill()
		c.view.App.Run()
		return nil
	} else {
		return err
	}
}
