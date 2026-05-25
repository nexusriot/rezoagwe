package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/golang/protobuf/proto"
	"github.com/rivo/tview"
	log "github.com/sirupsen/logrus"

	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	"github.com/nexusriot/rezoagwe/pkg/discovery/view"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

type Controller struct {
	debug     bool
	view      *view.View
	model     *model.Model
	listen    *net.UDPConn // shared sender + receiver
	startedAt time.Time

	filterMu sync.RWMutex
	filter   string
}

func NewController(
	debug bool,
	bootstrapAddr,
	nodeAddr,
	nick string,
) *Controller {
	m := model.NewModel(bootstrapAddr, nodeAddr, nick)
	v := view.NewView()
	v.Frame.AddText("Rezoagwe Discovery Node v0.0.3", true, tview.AlignCenter, tcell.ColorGreen)
	return &Controller{
		debug:     debug,
		view:      v,
		model:     m,
		startedAt: time.Now(),
	}
}

func (c *Controller) sendTo(addr string, packet []byte) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		log.Errorf("resolve %s: %s", addr, err)
		return
	}
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		log.Errorf("dial %s: %s", addr, err)
		return
	}
	defer conn.Close()
	if _, err := conn.Write(packet); err != nil {
		log.Errorf("write %s: %s", addr, err)
	}
}

func (c *Controller) sendKV(addr string, msg *pb.Payload) {
	body, err := proto.Marshal(msg)
	if err != nil {
		log.Errorf("marshal payload: %s", err)
		return
	}
	c.sendTo(addr, pb.EncodeKV(body))
}

func (c *Controller) sendJSON(addr string, kind pb.MessageKind, v interface{}) {
	pkt, err := pb.EncodeJSON(kind, v)
	if err != nil {
		log.Errorf("encode kind=%d: %s", kind, err)
		return
	}
	c.sendTo(addr, pkt)
}

func (c *Controller) broadcastKV(msg *pb.Payload) {
	body, err := proto.Marshal(msg)
	if err != nil {
		log.Errorf("marshal payload: %s", err)
		return
	}
	pkt := pb.EncodeKV(body)
	c.model.Nodes.Range(func(key, _ interface{}) bool {
		c.sendTo(key.(string), pkt)
		return true
	})
}

func (c *Controller) broadcastJSON(kind pb.MessageKind, v interface{}) {
	pkt, err := pb.EncodeJSON(kind, v)
	if err != nil {
		log.Errorf("encode kind=%d: %s", kind, err)
		return
	}
	c.model.Nodes.Range(func(key, _ interface{}) bool {
		c.sendTo(key.(string), pkt)
		return true
	})
}

func (c *Controller) HandleConnection(conn *net.UDPConn, updateCh chan<- struct{}) {
	buf := make([]byte, 65535)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Errorf("read udp: %s", err)
			continue
		}
		kind, body, err := pb.SplitKind(buf[:n])
		if err != nil {
			log.Errorf("split kind: %s", err)
			continue
		}
		switch kind {
		case pb.KindKV:
			c.handleKV(body, updateCh)
		case pb.KindChat:
			c.handleChat(body)
		case pb.KindStateRequest:
			c.handleStateRequest(body)
		case pb.KindStateResponse:
			c.handleStateResponse(body, updateCh)
		case pb.KindPeerGossip:
			c.handlePeerGossip(body)
		case pb.KindHello:
			c.handleHello(body)
		default:
			log.Errorf("unknown kind: %d", kind)
		}
	}
}

func (c *Controller) handleKV(body []byte, updateCh chan<- struct{}) {
	loaded := new(pb.Payload)
	if err := proto.Unmarshal(body, loaded); err != nil {
		log.Errorf("unmarshal payload: %s", err)
		return
	}
	switch loaded.Action {
	case pb.DiscoveryAction_SET:
		c.model.Store.Set(loaded.Key, string(loaded.Value))
	case pb.DiscoveryAction_DELETE:
		c.model.Store.Delete(loaded.Key)
	default:
		log.Errorf("unknown kv action: %s", loaded.Action)
		return
	}
	select {
	case updateCh <- struct{}{}:
	default:
	}
}

func (c *Controller) handleChat(body []byte) {
	var m pb.ChatMessage
	if err := json.Unmarshal(body, &m); err != nil {
		log.Errorf("unmarshal chat: %s", err)
		return
	}
	c.model.TouchPeer(m.Sender)
	if m.Nick != "" {
		c.model.SetNick(m.Sender, m.Nick)
	}
	c.appendChat(c.formatChat(m.Sender, m.Nick, m.Text, m.TS))
	// Learn about chat sender as a peer if we didn't know them.
	if c.model.AddPeer(m.Sender) {
		c.announceJoin(m.Sender, m.Nick)
		c.refreshNodes()
		c.sendJSON(m.Sender, pb.KindHello, c.helloMessage())
	}
}

func (c *Controller) handleStateRequest(body []byte) {
	var req pb.StateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Errorf("unmarshal state req: %s", err)
		return
	}
	c.model.TouchPeer(req.From)
	snap := c.model.Store.Snapshot()
	c.sendJSON(req.From, pb.KindStateResponse, pb.StateResponse{Store: snap})
}

func (c *Controller) handleStateResponse(body []byte, updateCh chan<- struct{}) {
	var resp pb.StateResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		log.Errorf("unmarshal state resp: %s", err)
		return
	}
	c.model.Store.Replace(resp.Store)
	select {
	case updateCh <- struct{}{}:
	default:
	}
}

func (c *Controller) handlePeerGossip(body []byte) {
	var g pb.PeerGossip
	if err := json.Unmarshal(body, &g); err != nil {
		log.Errorf("unmarshal gossip: %s", err)
		return
	}
	c.model.TouchPeer(g.From)
	if g.Nick != "" {
		c.model.SetNick(g.From, g.Nick)
	}
	changed := false
	if c.model.AddPeer(g.From) {
		changed = true
		c.announceJoin(g.From, g.Nick)
		c.sendJSON(g.From, pb.KindHello, c.helloMessage())
	}
	for _, p := range g.Peers {
		if c.model.AddPeer(p) {
			changed = true
			c.announceJoin(p, c.model.NickOf(p))
			c.sendJSON(p, pb.KindHello, c.helloMessage())
		}
	}
	if changed {
		c.refreshNodes()
	}
}

func (c *Controller) handleHello(body []byte) {
	var h pb.Hello
	if err := json.Unmarshal(body, &h); err != nil {
		log.Errorf("unmarshal hello: %s", err)
		return
	}
	c.model.TouchPeer(h.From)
	if h.Nick != "" {
		c.model.SetNick(h.From, h.Nick)
	}
	if c.model.AddPeer(h.From) {
		c.announceJoin(h.From, h.Nick)
		c.refreshNodes()
	}
}

func (c *Controller) Start() error {
	c.model.RegisterNode()

	discovered := c.model.DiscoverNodes()
	for _, node := range discovered {
		c.model.AddPeer(node)
	}

	addr, err := net.ResolveUDPAddr("udp", c.model.NodeAddr)
	if err != nil {
		log.Errorf("resolve %s: %s", c.model.NodeAddr, err)
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Panicf("listen udp: %s", err)
	}
	defer conn.Close()
	c.listen = conn
	log.Infof("UDP node is listening on %s", addr)

	updateCh := make(chan struct{}, 16)
	go c.HandleConnection(conn, updateCh)

	// Announce to existing peers + request state from a random one.
	c.helloAllPeers()
	c.requestStateFromRandomPeer()

	// Periodic gossip: share our known peers with one random peer.
	go c.gossipLoop()
	// Periodic heartbeat: re-register with bootstrap + Hello every peer.
	go c.heartbeatLoop()
	// Periodic peer eviction: drop peers we haven't heard from in a while.
	go c.evictLoop()

	c.fillNodes()
	c.fillDetails()
	c.setInput()
	c.view.HighlightFocus()
	// NOTE: do NOT call refreshStatus() here. It goes through
	// QueueUpdateDraw, which is synchronous in this tview version and
	// blocks on a done channel that only the running event loop signals.
	// Calling it before App.Run() would deadlock the main goroutine and
	// the TUI would never start. statusLoop paints the first frame from
	// its own goroutine, which is safe — it'll block harmlessly until
	// App.Run() begins draining the update queue.
	go c.statusLoop()

	go func() {
		for range updateCh {
			c.fillStoreQ()
		}
	}()

	return c.view.App.Run()
}

func (c *Controller) helloAllPeers() {
	c.model.Nodes.Range(func(key, _ interface{}) bool {
		c.sendJSON(key.(string), pb.KindHello, pb.Hello{From: c.model.NodeAddr})
		return true
	})
}

func (c *Controller) requestStateFromRandomPeer() {
	peers := c.model.GetNodes()
	if len(peers) == 0 {
		return
	}
	peer := peers[rand.Intn(len(peers))]
	c.sendJSON(peer, pb.KindStateRequest, pb.StateRequest{From: c.model.NodeAddr})
}

func (c *Controller) gossipLoop() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for range t.C {
		peers := c.model.GetNodes()
		if len(peers) == 0 {
			continue
		}
		target := peers[rand.Intn(len(peers))]
		c.sendJSON(target, pb.KindPeerGossip, pb.PeerGossip{
			From:  c.model.NodeAddr,
			Nick:  c.model.NodeNick,
			Peers: peers,
		})
	}
}

// heartbeatLoop keeps bootstrap and peers aware that we are still alive.
func (c *Controller) heartbeatLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		c.model.RegisterNode()
		hello := c.helloMessage()
		c.model.Nodes.Range(func(key, _ interface{}) bool {
			c.sendJSON(key.(string), pb.KindHello, hello)
			return true
		})
	}
}

func (c *Controller) helloMessage() pb.Hello {
	return pb.Hello{From: c.model.NodeAddr, Nick: c.model.NodeNick}
}

// evictLoop drops peers from which no packet has arrived for a while.
func (c *Controller) evictLoop() {
	const threshold = 15 * time.Second
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		if evicted := c.model.EvictStalePeers(threshold); len(evicted) > 0 {
			for _, ep := range evicted {
				log.Debugf("evicted stale peer %s", ep.Addr)
				c.announceLeave(ep.Addr, ep.Nick)
			}
			c.refreshNodes()
		}
	}
}

func (c *Controller) Stop() {
	log.Debugf("exit...")
	c.view.App.Stop()
}

func (c *Controller) setInput() {
	c.view.App.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		// Never steal keys while a modal dialog is on top: the form/modal
		// needs the raw events (including Tab to move between fields).
		if c.view.Pages.HasPage("modal") {
			return event
		}
		switch event.Key() {
		case tcell.KeyCtrlQ:
			c.Stop()
			return nil
		case tcell.KeyTab:
			c.cycleFocus()
			return nil
		}
		return event
	})
	c.view.List.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEnter:
			return c.edit()
		case tcell.KeyRune:
			switch event.Rune() {
			case 'c':
				return c.create()
			case 'e':
				return c.edit()
			case 'd':
				return c.delete()
			case '/':
				c.setFocus(c.view.Filter)
				return nil
			}
		}
		return event
	})

	// Filter field: type to filter, Enter focuses the keys list, Esc clears.
	c.view.Filter.SetChangedFunc(func(text string) {
		c.setFilter(text)
	})
	c.view.Filter.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			c.setFocus(c.view.List)
		case tcell.KeyEsc:
			c.view.Filter.SetText("")
			c.setFocus(c.view.List)
		}
	})
	c.view.ChatInput.SetDoneFunc(func(key tcell.Key) {
		if key != tcell.KeyEnter {
			return
		}
		text := strings.TrimSpace(c.view.ChatInput.GetText())
		if text == "" {
			return
		}
		c.view.ChatInput.SetText("")
		// Send off the tview event-loop goroutine: UDP dials should never
		// be able to stall the UI, even if a peer address is unreachable.
		go c.sendChat(text)
	})
}

func (c *Controller) sendChat(text string) {
	m := pb.ChatMessage{
		Sender: c.model.NodeAddr,
		Nick:   c.model.NodeNick,
		Text:   text,
		TS:     time.Now().Unix(),
	}
	c.appendChat(c.formatChat(m.Sender, m.Nick, m.Text, m.TS))
	c.broadcastJSON(pb.KindChat, m)
}

func (c *Controller) cycleFocus() {
	order := c.view.Focusables()
	cur := c.view.App.GetFocus()
	idx := -1
	for i, p := range order {
		if p == cur {
			idx = i
			break
		}
	}
	next := order[(idx+1)%len(order)]
	c.setFocus(next)
}

// setFocus changes focus and refreshes border colors so the active pane
// is visually distinct.
func (c *Controller) setFocus(p tview.Primitive) {
	c.view.App.SetFocus(p)
	c.view.HighlightFocus()
}

func (c *Controller) openKeyForm(title, initKey, initValue string, keyReadOnly bool) {
	form, keyField, valueArea := c.view.NewKeyForm(title, initKey, initValue, keyReadOnly)
	form.AddButton("Save", func() {
		key := strings.TrimSpace(keyField.GetText())
		value := valueArea.GetText()
		c.view.Pages.RemovePage("modal")
		c.setFocus(c.view.List)
		if key == "" {
			return
		}
		log.Debugf("save record: key=%q value=%q", key, value)
		go c.store(key, value)
	})
	form.AddButton("Cancel", func() {
		c.view.Pages.RemovePage("modal")
		c.setFocus(c.view.List)
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(form, 70, 16), true, true)
	if keyReadOnly {
		c.view.App.SetFocus(valueArea)
	} else {
		c.view.App.SetFocus(form)
	}
}

func (c *Controller) create() *tcell.EventKey {
	c.openKeyForm("New key", "", "", false)
	return nil
}

func (c *Controller) edit() *tcell.EventKey {
	key := c.currentSelectedKey()
	if key == "" {
		// No selection: fall through to a create flow so Enter is never a no-op.
		c.openKeyForm("New key", "", "", false)
		return nil
	}
	val, _ := c.model.Store.Get(key)
	c.openKeyForm("Edit "+key, key, val, true)
	return nil
}

func (c *Controller) fillNodes() {
	c.view.NodeList.Clear()
	c.view.NodeList.SetMainTextColor(tcell.Color31)
	for _, node := range c.model.GetNodes() {
		n := node
		nick := c.model.NickOf(n)
		label := n
		if nick != "" {
			label = fmt.Sprintf("%s — %s", n, nick)
		}
		c.view.NodeList.AddItem(label, n, 0, func() {})
	}
}

func (c *Controller) refreshNodes() {
	c.view.App.QueueUpdateDraw(func() {
		c.fillNodes()
	})
}

func (c *Controller) refreshChat() {
	lines := c.model.ChatLog()
	c.view.App.QueueUpdateDraw(func() {
		c.view.Chat.Clear()
		fmt.Fprint(c.view.Chat, strings.Join(lines, "\n"))
		c.view.Chat.ScrollToEnd()
	})
}

// chatPalette is a small set of distinguishable tview color names used for
// per-sender coloring. The "own" color is handled separately so this set
// stays neutral.
var chatPalette = []string{
	"red", "green", "blue", "fuchsia", "aqua",
	"orange", "lime", "yellow", "deepskyblue", "violet",
	"lightcoral", "mediumseagreen",
}

func colorFor(s string) string {
	if s == "" {
		return "white"
	}
	var h uint32
	for _, r := range s {
		h = h*131 + uint32(r)
	}
	return chatPalette[h%uint32(len(chatPalette))]
}

func (c *Controller) formatChat(sender, nick, text string, ts int64) string {
	stamp := time.Unix(ts, 0).Format("15:04:05")
	if sender == c.model.NodeAddr {
		// Own message: distinct cyan + a "you" tag, no addr clutter.
		return fmt.Sprintf("[gray]%s[-] [::b][aqua]you[-][::-]: [white]%s[-]",
			stamp, text)
	}
	if nick == "" {
		nick = c.model.NickOf(sender)
	}
	if nick == "" {
		nick = "?"
	}
	color := colorFor(sender)
	return fmt.Sprintf("[gray]%s[-] [::b][%s]%s[-][::-][gray]@%s[-]: %s",
		stamp, color, nick, sender, text)
}

func (c *Controller) formatSystem(text string) string {
	stamp := time.Now().Format("15:04:05")
	return fmt.Sprintf("[gray]%s[-] [darkgray]» %s[-]", stamp, text)
}

func (c *Controller) appendChat(line string) {
	c.model.AppendChat(line)
	c.refreshChat()
}

func (c *Controller) announceJoin(addr, nick string) {
	if nick == "" {
		c.appendChat(c.formatSystem(fmt.Sprintf("%s joined", addr)))
	} else {
		color := colorFor(addr)
		c.appendChat(c.formatSystem(
			fmt.Sprintf("[%s]%s[-][darkgray] (%s) joined", color, nick, addr)))
	}
}

func (c *Controller) announceLeave(addr, nick string) {
	if nick == "" {
		c.appendChat(c.formatSystem(fmt.Sprintf("%s left", addr)))
	} else {
		color := colorFor(addr)
		c.appendChat(c.formatSystem(
			fmt.Sprintf("[%s]%s[-][darkgray] (%s) left", color, nick, addr)))
	}
}

// formatUptime renders an uptime as "1h02m03s" / "42s" etc.
func formatUptime(d time.Duration) string {
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

func (c *Controller) refreshStatus() {
	peers := c.model.GetNodes()
	peerCount := len(peers)
	kvCount := len(c.model.Store.Snapshot())
	uptime := formatUptime(time.Since(c.startedAt))

	var stateTag string
	if peerCount == 0 {
		stateTag = "[red]DEGRADED[-]"
	} else {
		stateTag = "[green]CONNECTED[-]"
	}

	line := fmt.Sprintf(
		" %s  [white]%s[-] [gray]([-][yellow]%s[-][gray])[-]  [white]peers:[-][cyan]%d[-]  [white]keys:[-][cyan]%d[-]  [white]up:[-][cyan]%s[-]",
		stateTag,
		c.model.NodeAddr,
		c.model.NodeNick,
		peerCount,
		kvCount,
		uptime,
	)

	c.view.App.QueueUpdateDraw(func() {
		c.view.Status.Clear()
		fmt.Fprint(c.view.Status, line)
	})
}

func (c *Controller) statusLoop() {
	// First paint runs from a goroutine on purpose: refreshStatus uses
	// QueueUpdateDraw, which is synchronous and would deadlock if it ran
	// on the main goroutine before App.Run() starts the event loop.
	c.refreshStatus()
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for range t.C {
		c.refreshStatus()
	}
}

func (c *Controller) fillDetails() {
	c.view.Details.Clear()
	fmt.Fprintf(c.view.Details, "[blue]Nick         ->[gray] %s\n", c.model.NodeNick)
	fmt.Fprintf(c.view.Details, "[blue]Node UUID    ->[gray] %s\n", c.model.NodeUUID)
	fmt.Fprintf(c.view.Details, "[blue]Node Address ->[gray] %s\n", c.model.NodeAddr)
	fmt.Fprintf(c.view.Details, "[green]Bootstrap    ->[white] %s\n", c.model.BootstrapAddr)
}

// previewValue returns a one-line, length-capped preview of value for the
// secondary list text.
func previewValue(v string) string {
	v = strings.ReplaceAll(v, "\n", " ⏎ ")
	v = strings.ReplaceAll(v, "\r", "")
	v = strings.ReplaceAll(v, "\t", " ")
	if len(v) > 80 {
		v = v[:77] + "..."
	}
	if v == "" {
		return "(empty)"
	}
	return v
}

// currentSelectedKey returns the key under the cursor in the keys list, or "".
func (c *Controller) currentSelectedKey() string {
	if c.view.List.GetItemCount() == 0 {
		return ""
	}
	main, _ := c.view.List.GetItemText(c.view.List.GetCurrentItem())
	return main
}

func (c *Controller) getFilter() string {
	c.filterMu.RLock()
	defer c.filterMu.RUnlock()
	return c.filter
}

func (c *Controller) setFilter(s string) {
	c.filterMu.Lock()
	c.filter = s
	c.filterMu.Unlock()
	c.fillStoreQ()
}

func (c *Controller) fillStoreQ() {
	snap := c.model.Store.Snapshot()
	filter := strings.ToLower(c.getFilter())

	// Stable order so cursor restoration is meaningful.
	keys := make([]string, 0, len(snap))
	for k := range snap {
		if filter == "" || strings.Contains(strings.ToLower(k), filter) ||
			strings.Contains(strings.ToLower(snap[k]), filter) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	c.view.App.QueueUpdateDraw(func() {
		prev := ""
		if c.view.List.GetItemCount() > 0 {
			prev, _ = c.view.List.GetItemText(c.view.List.GetCurrentItem())
		}
		c.view.List.Clear()
		newIdx := -1
		for i, key := range keys {
			k := key
			c.view.List.AddItem(k, previewValue(snap[k]), 0, nil)
			if k == prev {
				newIdx = i
			}
		}
		if newIdx >= 0 {
			c.view.List.SetCurrentItem(newIdx)
		}
	})
}

func (c *Controller) store(key, value string) {
	c.model.Store.Set(key, value)
	c.broadcastKV(&pb.Payload{
		Action: pb.DiscoveryAction_SET,
		Key:    key,
		Value:  []byte(value),
	})
	c.fillStoreQ()
}

func (c *Controller) del(key string) {
	c.model.Store.Delete(key)
	c.broadcastKV(&pb.Payload{
		Action: pb.DiscoveryAction_DELETE,
		Key:    key,
	})
	c.fillStoreQ()
}

func (c *Controller) delete() *tcell.EventKey {
	key := c.currentSelectedKey()
	if key == "" {
		return nil
	}
	if _, ok := c.model.Store.Get(key); ok {
		delQ := c.view.NewDeleteQ(key)
		delQ.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			c.view.Pages.RemovePage("modal")
			c.setFocus(c.view.List)
			if buttonLabel == "ok" {
				go c.del(key)
			}
		})
		c.view.Pages.AddPage("modal", c.view.ModalEdit(delQ, 20, 7), true, true)
		c.view.App.SetFocus(delQ)
	}
	return nil
}
