package controller

import (
	"fmt"
	"github.com/gdamore/tcell/v2"
	"github.com/golang/protobuf/proto"
	"github.com/rivo/tview"
	log "github.com/sirupsen/logrus"
	"net"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	"github.com/nexusriot/rezoagwe/pkg/discovery/view"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

type Controller struct {
	debug bool
	view  *view.View
	model *model.Model
}

func NewController(
	debug bool,
	bootstrapAddr,
	nodeAddr string,
) *Controller {
	m := model.NewModel(bootstrapAddr, nodeAddr)
	v := view.NewView()
	v.Frame.AddText(fmt.Sprintf("Rezoagve Discovery Node v.0.0.1 PoC"), true, tview.AlignCenter, tcell.ColorGreen)
	controller := Controller{
		debug: debug,
		view:  v,
		model: m,
	}
	return &controller
}

func (c *Controller) HandleConnection(conn *net.UDPConn) {

	buf := make([]byte, 1024)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Errorf("Error reading from UDP: %s", err)
			continue
		}

		loaded := new(pb.Payload)
		err = proto.Unmarshal(buf[:n], loaded)

		if err != nil {
			log.Errorf("Error Unmarshal message: %s", err)
			continue
		}

		switch loaded.Action {

		case pb.DiscoveryAction_SET:
			c.model.Store.Set(loaded.Key, string(loaded.Value))

		case pb.DiscoveryAction_DELETE:
			c.model.Store.Delete(loaded.Key)

		default:
			log.Errorf("Unknown discovery action: %s", loaded.Action)
			continue
		}
		c.propagate(loaded)
	}
}

func (c *Controller) Start() error {

	// move to network methods
	c.model.RegisterNode()

	discoveredNodes := c.model.DiscoverNodes()
	for _, node := range discoveredNodes {
		if node != "" && node != c.model.NodeAddr {
			c.model.Nodes.Store(node, true)
		}
	}
	addr, err := net.ResolveUDPAddr("udp", c.model.NodeAddr)
	if err != nil {
		log.Panicf("Error resolving address: %s", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Panicf("Error starting UDP server: %s", err)
	}
	defer conn.Close()
	log.Infof("UDP node is listening on %s", addr)

	go c.HandleConnection(conn)

	c.setInput()
	return c.view.App.Run()
}

func (c *Controller) create() *tcell.EventKey {
	createForm := c.view.NewCreateForm(fmt.Sprintf("New key"))
	createForm.AddButton("Save", func() {
		key := createForm.GetFormItem(0).(*tview.InputField).GetText()
		value := createForm.GetFormItem(1).(*tview.InputField).GetText()
		if key != "" {
			log.Debugf("Creating record: key: %s, value: %s", key, value)
			c.model.Store.Set(key, value)
		}
	})
	createForm.AddButton("Quit", func() {
		c.view.Pages.RemovePage("modal")
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(createForm, 60, 11), true, true)
	return nil
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
	c.view.List.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyRune:
			switch event.Rune() {
			case 'c':
				return c.create()
			}
		}
		return event
	})

}
func (c *Controller) Stop() {
	log.Debugf("exit...")
	c.view.App.Stop()
}

func (c *Controller) propagate(message *pb.Payload) {
	c.model.Nodes.Range(func(key, value interface{}) bool {
		addr, err := net.ResolveUDPAddr("udp", key.(string))
		if err != nil {
			fmt.Println("Error resolving address:", err)
			return true
		}
		conn, err := net.DialUDP("udp", nil, addr)
		if err != nil {
			fmt.Println("Error connecting to node:", err)
			return true
		}
		defer conn.Close()
		toSend, err := proto.Marshal(message)
		_, err = conn.Write(toSend)
		conn.Write(toSend)
		return true
	})
}
