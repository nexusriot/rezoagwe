package controller

import (
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/gdamore/tcell/v2"
	"github.com/golang/protobuf/proto"
	"github.com/rivo/tview"
	log "github.com/sirupsen/logrus"

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

func (c *Controller) HandleConnection(conn *net.UDPConn, wg *sync.WaitGroup, dich chan<- struct{}) {

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
		dich <- struct{}{}
	}
}

func (c *Controller) Start() error {

	var wg sync.WaitGroup
	diCh := make(chan struct{})

	// TODO: move to network methods
	c.model.RegisterNode()

	discoveredNodes := c.model.DiscoverNodes()
	for _, node := range discoveredNodes {
		if node != "" && node != c.model.NodeAddr {
			c.model.Nodes.Store(node, true)
			// TODO - > improve discover

		}
	}
	addr, err := net.ResolveUDPAddr("udp", c.model.NodeAddr)
	if err != nil {
		log.Errorf("Error resolving address: %s", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Panicf("Error starting UDP server: %s", err)
	}
	defer conn.Close()
	log.Infof("UDP node is listening on %s", addr)
	wg.Add(1)
	go c.HandleConnection(conn, &wg, diCh)

	c.fillNodes()
	c.fillDetails()
	c.setInput()

	go func() {
		for {
			select {
			case _, ok := <-diCh:
				if ok {
					c.fillStoreQ()
				}
			default:
			}
		}
	}()

	return c.view.App.Run()
}

func (c *Controller) create() *tcell.EventKey {
	createForm := c.view.NewCreateForm(fmt.Sprintf("New key"))
	createForm.AddButton("Save", func() {
		key := createForm.GetFormItem(0).(*tview.InputField).GetText()
		value := createForm.GetFormItem(1).(*tview.InputField).GetText()
		if key != "" {
			log.Debugf("Creating record: key: %s, value: %s", key, value)
			c.store(key, value)
			c.view.Pages.RemovePage("modal")
		}
	})
	createForm.AddButton("Quit", func() {
		c.view.Pages.RemovePage("modal")
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(createForm, 60, 11), true, true)
	return nil
}

func (c *Controller) fillNodes() {
	nodes := c.model.GetNodes()
	c.view.NodeList.Clear()
	c.view.NodeList.SetMainTextColor(tcell.Color31)
	for _, node := range nodes {
		c.view.NodeList.AddItem(node, node, 0, func() {
		})
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
	c.view.List.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyRune:
			switch event.Rune() {
			case 'c':
				return c.create()
			case 'd':
				return c.delete()
			}
		}
		return event
	})
}

func (c *Controller) Stop() {
	log.Debugf("exit...")
	c.view.App.Stop()
}

func (c *Controller) fillDetails() {
	c.view.Details.Clear()
	fmt.Fprintf(c.view.Details, "[blue] Node Uuid -> [gray] %s\n", c.model.NodeUUID)
	fmt.Fprintf(c.view.Details, "[blue] Node Address -> [gray] %s\n\n", c.model.NodeAddr)
	fmt.Fprintf(c.view.Details, "[green] Bootstrap -> [white] %s\n", c.model.BootstrapAddr)
}

func (c *Controller) fillStoreQ() {
	c.view.App.QueueUpdateDraw(func() {
		c.view.List.Clear()
		for key, value := range c.model.GetStore() {
			kv := fmt.Sprintf("%s:%s", key, value)
			c.view.List.AddItem(kv, kv, 0, func() {
			})
		}
	})
}

func (c *Controller) store(key, value string) {
	c.model.Store.Set(key, value)
	msg := pb.Payload{
		Action: pb.DiscoveryAction_SET,
		Key:    key,
		Value:  []byte(value),
	}
	c.propagate(&msg)
	c.view.List.Clear()
	for key, value := range c.model.GetStore() {
		kv := fmt.Sprintf("%s:%s", key, value)
		c.view.List.AddItem(kv, kv, 0, func() {
		})
	}
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

func (c *Controller) del(key string) {
	c.model.Store.Delete(key)
	msg := pb.Payload{
		Action: pb.DiscoveryAction_DELETE,
		Key:    key,
	}
	c.propagate(&msg)
	c.view.List.Clear()
	for key, value := range c.model.GetStore() {
		kv := fmt.Sprintf("%s:%s", key, value)
		c.view.List.AddItem(kv, kv, 0, func() {
		})
	}
}

func (c *Controller) delete() *tcell.EventKey {
	if c.view.List.GetItemCount() == 0 {
		return nil
	}
	var err error
	i := c.view.List.GetCurrentItem()
	_, cur := c.view.List.GetItemText(i)
	cur = strings.TrimSpace(cur)
	parts := strings.Split(cur, ":")
	key := parts[0]
	if _, ok := c.model.GetStore()[key]; ok {
		delQ := c.view.NewDeleteQ(cur)
		delQ.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			if buttonLabel == "ok" {
				c.del(key)
				if err != nil {
					c.view.Pages.RemovePage("modal")
					return
				}
			}
			c.view.Pages.RemovePage("modal")
		})
		c.view.Pages.AddPage("modal", c.view.ModalEdit(delQ, 20, 7), true, true)
	}
	return nil
}
