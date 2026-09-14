package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	log "github.com/sirupsen/logrus"

	"github.com/nexusriot/rezoagwe/pkg/discovery/diagnostics"
	"github.com/nexusriot/rezoagwe/pkg/discovery/node"
	"github.com/nexusriot/rezoagwe/pkg/discovery/topology"
	"github.com/nexusriot/rezoagwe/pkg/discovery/view"
	pb "github.com/nexusriot/rezoagwe/pkg/proto"
)

// Controller wires the node engine to the tview front end. It implements
// node.Events.
type Controller struct {
	view    *view.View
	node    *node.Node
	version string
	httpAdr string

	filterMu sync.RWMutex
	filter   string

	feedMu       sync.RWMutex
	showActivity bool

	// Refresh requests are coalesced through capacity-1 channels: engine
	// goroutines must never block on the UI. QueueUpdateDraw waits for the
	// tview event loop, and Ctrl+Q runs the engine shutdown *on* that loop —
	// a direct call from a node goroutine would deadlock the two against each
	// other.
	kvCh    chan struct{}
	peersCh chan struct{}
	feedCh  chan struct{}
	done    chan struct{}

	stopOnce sync.Once
}

func NewController(cfg node.Config, version string) (*Controller, error) {
	v := view.NewView()
	v.Frame.AddText("Rezoagwe Discovery Node "+version, true, tview.AlignCenter, tcell.ColorGreen)
	c := &Controller{
		view:    v,
		version: version,
		kvCh:    make(chan struct{}, 1),
		peersCh: make(chan struct{}, 1),
		feedCh:  make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	n, err := node.New(cfg, c)
	if err != nil {
		return nil, err
	}
	c.node = n
	return c, nil
}

// Node exposes the engine so a caller can attach the HTTP gateway.
func (c *Controller) Node() *node.Node { return c.node }

// SetHTTPAddr records the gateway address for the details pane.
func (c *Controller) SetHTTPAddr(addr string) { c.httpAdr = addr }

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (c *Controller) KVChanged()       { signal(c.kvCh) }
func (c *Controller) PeersChanged()    { signal(c.peersCh) }
func (c *Controller) ChatChanged()     { signal(c.feedCh) }
func (c *Controller) ActivityChanged() { signal(c.feedCh) }

func (c *Controller) Start() error {
	c.node.Start()
	// Joining dials bootstrap and a peer, either of which can block on an
	// unreachable host; the first frame must not wait on that.
	go c.node.Join()

	// Only direct widget writes are safe here. Everything that repaints goes
	// through QueueUpdateDraw, which is synchronous in this tview version: it
	// blocks on a done channel that only the running event loop signals, so
	// calling one before App.Run() deadlocks the main goroutine and the TUI
	// never appears. The first paint of every pane therefore happens in
	// refreshLoop and statusLoop, on their own goroutines, where blocking until
	// Run() starts draining the queue is harmless.
	c.fillDetails()
	c.setInput()
	c.view.HighlightFocus()

	go c.refreshLoop()
	go c.statusLoop()

	return c.view.App.Run()
}

func (c *Controller) Stop() {
	c.stopOnce.Do(func() {
		log.Debugf("exit...")
		close(c.done)
		c.node.Stop()
		c.view.App.Stop()
	})
}

// refreshLoop paints the first frame and then repaints in response to engine
// events. The initial paint lives here rather than in Start for the reason
// documented there.
func (c *Controller) refreshLoop() {
	c.fillStore()
	c.fillNodes()
	c.fillFeed()
	for {
		select {
		case <-c.done:
			return
		case <-c.kvCh:
			c.fillStore()
		case <-c.peersCh:
			c.fillNodes()
		case <-c.feedCh:
			c.fillFeed()
		}
	}
}

func (c *Controller) statusLoop() {
	c.refreshStatus()
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			c.refreshStatus()
		}
	}
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
			case 'h':
				return c.history()
			case 'm':
				return c.metrics()
			case 'g':
				return c.topology()
			case 'D':
				return c.diagnostics()
			case 'v':
				return c.consistency()
			case 'a':
				return c.toggleFeed()
			case 'x':
				return c.exportDialog()
			case 'i':
				return c.importDialog()
			case '?':
				return c.help()
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
		// Off the event-loop goroutine: sending dials peers, and an unreachable
		// peer must never stall the UI.
		go c.node.Submit(text)
	})
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

func (c *Controller) closeModal() {
	c.view.Pages.RemovePage("modal")
	c.setFocus(c.view.List)
}

func (c *Controller) showModal(p tview.Primitive, width, height int, focus tview.Primitive) {
	c.view.Pages.AddPage("modal", c.view.ModalEdit(p, width, height), true, true)
	c.view.App.SetFocus(focus)
}

func (c *Controller) openKeyForm(title, initKey, initValue string, ttl int64, keyReadOnly bool, guard *pb.Version) {
	f := c.view.NewKeyForm(title, initKey, initValue, ttl, keyReadOnly)
	f.Form.AddButton("Save", func() {
		key := strings.TrimSpace(f.Key.GetText())
		value := f.Value.GetText()
		ttlSecs, _ := strconv.Atoi(strings.TrimSpace(f.TTL.GetText()))
		cas := f.CAS.IsChecked()
		c.closeModal()
		if key == "" {
			return
		}
		log.Debugf("save record: key=%q ttl=%d cas=%v", key, ttlSecs, cas)
		go c.store(key, value, time.Duration(ttlSecs)*time.Second, cas, guard)
	})
	f.Form.AddButton("Cancel", func() {
		c.closeModal()
	})
	height := 18
	if keyReadOnly {
		c.showModal(f.Form, 70, height, f.Value)
	} else {
		c.showModal(f.Form, 70, height, f.Form)
	}
}

func (c *Controller) create() *tcell.EventKey {
	c.openKeyForm("New key", "", "", 0, false, nil)
	return nil
}

func (c *Controller) edit() *tcell.EventKey {
	key := c.currentSelectedKey()
	if key == "" {
		// No selection: fall through to a create flow so Enter is never a no-op.
		c.openKeyForm("New key", "", "", 0, false, nil)
		return nil
	}
	e, ok := c.node.Entry(key)
	if !ok {
		c.openKeyForm("New key", key, "", 0, false, nil)
		return nil
	}
	var ttl int64
	if e.ExpiresAt > 0 {
		ttl = e.ExpiresAt - time.Now().Unix()
		if ttl < 0 {
			ttl = 0
		}
	}
	// The version on screen is the guard: if a peer writes the same key while
	// this dialog is open, a guarded save is refused instead of silently
	// overwriting the newer value.
	guard := e.Version
	c.openKeyForm("Edit "+key, key, e.Value, ttl, true, &guard)
	return nil
}

func (c *Controller) delete() *tcell.EventKey {
	key := c.currentSelectedKey()
	if key == "" {
		return nil
	}
	if _, ok := c.node.Get(key); !ok {
		return nil
	}
	delQ := c.view.NewDeleteQ(key)
	delQ.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
		c.closeModal()
		if buttonLabel == "ok" {
			go c.node.Delete(key)
		}
	})
	c.showModal(delQ, 20, 7, delQ)
	return nil
}

func (c *Controller) store(key, value string, ttl time.Duration, cas bool, guard *pb.Version) {
	if !cas {
		c.node.Set(key, value, ttl)
		return
	}
	expect := pb.Version{}
	if guard != nil {
		expect = *guard
	}
	if _, ok := c.node.CompareAndSet(key, value, ttl, expect); !ok {
		c.notify("guarded write to %s refused: the key changed since the form opened", key)
	}
}

// notify puts a local-only line in the feed.
func (c *Controller) notify(format string, args ...interface{}) {
	c.node.Model.AppendChat(pb.ChatEntry{
		TS:   time.Now().Unix(),
		Text: fmt.Sprintf(format, args...),
		Kind: pb.ChatSystem,
	})
	c.ChatChanged()
}

func (c *Controller) history() *tcell.EventKey {
	key := c.currentSelectedKey()
	if key == "" {
		return nil
	}
	entries := c.node.History(key)
	var b strings.Builder
	if len(entries) == 0 {
		b.WriteString("[gray]no recorded versions[-]\n")
	}
	// Newest first: the last thing that happened is what you are looking for.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		origin := "[aqua]remote[-]"
		if e.Local {
			origin = "[yellow]local[-]"
		}
		what := "set"
		if e.Deleted {
			what = "delete"
		}
		fmt.Fprintf(&b, "[white]v%d[-] [gray]%s[-] %s by [::b]%s[::-] %s\n",
			e.Version.Counter, e.At.Format("15:04:05"), what,
			c.node.Model.WriterName(e.Version.Node), origin)
		if !e.Deleted {
			fmt.Fprintf(&b, "    %s\n", tview.Escape(previewValue(e.Value)))
		}
	}
	tv := c.view.NewTextModal("History of "+key, b.String())
	c.showModal(tv, 80, 20, tv)
	return nil
}

func (c *Controller) metrics() *tcell.EventKey {
	s := c.node.Metrics.Snapshot()
	var b strings.Builder
	row := func(label string, v uint64) {
		fmt.Fprintf(&b, "  [white]%-24s[-] [cyan]%d[-]\n", label, v)
	}
	b.WriteString("[::b]Traffic[::-]\n")
	row("packets sent", s.PacketsSent)
	row("packets received", s.PacketsRecv)
	row("bytes sent", s.BytesSent)
	row("bytes received", s.BytesRecv)
	row("send errors", s.SendErrors)
	b.WriteString("\n[::b]Rejected packets[::-]\n")
	row("failed authentication", s.AuthFailures)
	row("replays", s.ReplayDrops)
	row("clock skew", s.SkewDrops)
	row("malformed", s.MalformedDrops)
	b.WriteString("\n[::b]Replication[::-]\n")
	row("local writes", s.KVLocalWrites)
	row("remote applied", s.KVApplied)
	row("stale rejected", s.KVRejectedStale)
	row("guarded writes refused", s.KVCASFailures)
	row("expired", s.KVExpired)
	row("tombstones reclaimed", s.KVGCed)
	b.WriteString("\n[::b]Anti-entropy[::-]\n")
	row("digests sent", s.AERounds)
	row("entries pushed", s.AEPushed)
	row("entries pulled", s.AEPulled)
	row("snapshots served", s.StateSyncOut)
	row("snapshots received", s.StateSyncIn)
	row("stream errors", s.StreamErrors)
	if len(s.Kinds) > 0 {
		b.WriteString("\n[::b]Messages by kind (sent/received)[::-]\n")
		for _, k := range s.Kinds {
			fmt.Fprintf(&b, "  [white]%-24s[-] [cyan]%d[-]/[cyan]%d[-]\n", k.Kind, k.Sent, k.Recv)
		}
	}
	tv := c.view.NewTextModal("Metrics", b.String())
	c.showModal(tv, 70, 26, tv)
	return nil
}

// topology draws the cluster as this node understands it.
//
// The Android and desktop apps have had this since they were written; the Go
// node, which is the one most likely to be running headless on a server, could
// only ever list its peers. A list cannot show a partition.
func (c *Controller) topology() *tcell.EventKey {
	g := c.node.Topology()
	groups := topology.Components(g)

	var b strings.Builder
	fmt.Fprintf(&b, "[::b]%d node(s), %d link(s), %d component(s)[::-]\n\n",
		len(g.Nodes), len(g.Links), len(groups))
	if len(groups) > 1 {
		sizes := make([]string, 0, len(groups))
		for _, grp := range groups {
			sizes = append(sizes, fmt.Sprint(len(grp)))
		}
		fmt.Fprintf(&b, "  [red]partitioned: %s[-]\n\n", strings.Join(sizes, " | "))
	}

	b.WriteString("[::b]Nodes[::-]\n")
	for _, n := range g.Nodes {
		colour := "white"
		switch n.Role {
		case topology.RoleSelf:
			colour = "yellow"
		case topology.RoleIndirect:
			colour = "darkgray"
		}
		advertised := "-"
		if n.Advertised >= 0 {
			advertised = fmt.Sprintf("%d", n.Advertised)
		}
		fmt.Fprintf(&b, "  [%s]%-24s[-] [darkgray]%-8s[-] links %-3d advertises %s\n",
			colour, truncate(n.Label, 24), n.Role, n.Degree, advertised)
	}

	if len(g.Links) > 0 {
		b.WriteString("\n[::b]Links[::-]\n")
		for _, l := range g.Links {
			colour := "white"
			if l.Kind == topology.LinkObserved {
				colour = "darkgray"
			}
			fmt.Fprintf(&b, "  [%s]%s — %s[-] [darkgray](%s)[-]\n", colour, l.A, l.B, l.Kind)
		}
		b.WriteString("\n[darkgray]observed = claimed by one end only; " +
			"mutual = both ends agree[-]\n")
	}

	tv := c.view.NewTextModal("Cluster", b.String())
	c.showModal(tv, 78, 28, tv)
	return nil
}

// diagnostics turns the counters into the short list of things actually wrong.
func (c *Controller) diagnostics() *tcell.EventKey {
	snap := c.node.Diagnostics()
	checks := diagnostics.Checks(snap, time.Now())

	var b strings.Builder
	fmt.Fprintf(&b, "[::b]%s[::-]  [darkgray]cluster %s, key %s[-]\n\n",
		snap.Addr, snap.Cluster, snap.KeyFingerprint)
	for _, ch := range checks {
		fmt.Fprintf(&b, "[%s]%-5s[-] [::b]%s[::-]\n", severityColour(ch.Severity),
			strings.ToUpper(string(ch.Severity)), ch.Title)
		fmt.Fprintf(&b, "      [darkgray]%s[-]\n\n", ch.Detail)
	}
	tv := c.view.NewTextModal("Diagnostics", b.String())
	c.showModal(tv, 80, 30, tv)
	return nil
}

// severityColour maps a finding to a tview colour tag.
func severityColour(s diagnostics.Severity) string {
	switch s {
	case diagnostics.SeverityError:
		return "red"
	case diagnostics.SeverityWarn:
		return "yellow"
	case diagnostics.SeverityOK:
		return "green"
	default:
		return "blue"
	}
}

// consistency asks every peer what it holds and reports who disagrees.
//
// It dials every peer, so it runs off the event loop: an unreachable peer must
// never freeze the terminal.
func (c *Controller) consistency() *tcell.EventKey {
	tv := c.view.NewTextModal("Consistency", "  checking every peer…")
	c.showModal(tv, 76, 24, tv)
	go func() {
		body := renderConsistency(c.node.CheckConsistency())
		c.view.App.QueueUpdateDraw(func() { tv.SetText(body) })
	}()
	return nil
}

func renderConsistency(r node.ConsistencyReport) string {
	var b strings.Builder
	verdict := "[green]every peer agrees[-]"
	if !r.Converged {
		verdict = "[red]replicas disagree[-]"
	}
	fmt.Fprintf(&b, "[::b]%s[::-]\n\n", verdict)
	fmt.Fprintf(&b, "  [white]this node[-]  %d keys, %d tombstones, clock %d\n\n",
		r.Keys, r.Tombstones, r.Clock)

	if len(r.Peers) == 0 {
		b.WriteString("  [darkgray]no peers to compare against[-]\n")
		return b.String()
	}
	for _, p := range r.Peers {
		name := p.Addr
		if p.Nick != "" {
			name = fmt.Sprintf("%s (%s)", p.Nick, p.Addr)
		}
		switch {
		case !p.Reachable:
			fmt.Fprintf(&b, "  [yellow]?[-] %s\n      [darkgray]did not answer: %s[-]\n", name, p.Error)
		case p.Agrees:
			fmt.Fprintf(&b, "  [green]=[-] %s  [darkgray]%d keys, clock %d[-]\n", name, p.Keys, p.Clock)
		default:
			fmt.Fprintf(&b, "  [red]≠[-] %s  [darkgray]%d keys, clock %d[-]\n", name, p.Keys, p.Clock)
			fmt.Fprintf(&b, "      [darkgray]differs in %d of %d key ranges[-]\n",
				len(p.DifferingBuckets), pb.FingerprintBuckets)
		}
	}
	if r.Unreachable > 0 {
		fmt.Fprintf(&b, "\n[darkgray]%d peer(s) did not answer; a silent peer says nothing "+
			"about whether it agrees.[-]\n", r.Unreachable)
	}
	if !r.Converged {
		b.WriteString("\n[darkgray]Anti-entropy repairs this on its own. A disagreement that " +
			"persists across several checks is the one worth chasing.[-]\n")
	}
	return b.String()
}

// truncate shortens a label to fit a fixed column.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func (c *Controller) help() *tcell.EventKey {
	body := `[::b]Keys pane[::-]
  [yellow]c[-]        create a key
  [yellow]e[-] / [yellow]Enter[-]  edit the selected key
  [yellow]d[-]        delete the selected key
  [yellow]h[-]        version history of the selected key
  [yellow]/[-]        filter keys and values
  [yellow]x[-] / [yellow]i[-]    export / import the store
  [yellow]m[-]        replication metrics
  [yellow]g[-]        cluster graph (peers, links, partitions)
  [yellow]D[-]        diagnostics: what is actually wrong
  [yellow]v[-]        verify every replica holds the same store
  [yellow]a[-]        switch the feed between chat and activity
  [yellow]?[-]        this help
  [yellow]Tab[-]      cycle panes
  [yellow]Ctrl+Q[-]   quit (announces departure to peers)

[::b]Chat commands[::-]
  [yellow]/nick[-] <name>              rename this node
  [yellow]/me[-] <text>                emote
  [yellow]/msg[-] <peer> <text>        direct message
  [yellow]/peers[-]                    list peers
  [yellow]/keys[-]                     list keys
  [yellow]/get[-] <key>                read a key
  [yellow]/set[-] <key> <value>        write a key
  [yellow]/setttl[-] <key> <s> <value> write a key with a TTL
  [yellow]/del[-] <key>                delete a key

[::b]Writing safely[::-]
  Tick [white]Guard[-] in the edit form to make the save a compare-and-swap
  against the version that was on screen. If a peer wrote the key in the
  meantime the save is refused instead of overwriting it.`
	tv := c.view.NewTextModal("Help", body)
	c.showModal(tv, 76, 30, tv)
	return nil
}

func defaultExportPath() string {
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "rezoagwe-export.json")
}

func (c *Controller) exportDialog() *tcell.EventKey {
	f := c.view.NewPathForm("Export store", defaultExportPath(), "")
	f.Form.AddButton("Export", func() {
		path := strings.TrimSpace(f.Path.GetText())
		c.closeModal()
		if path == "" {
			return
		}
		go func() {
			data, err := c.node.Export()
			if err != nil {
				c.notify("export failed: %s", err)
				return
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				c.notify("export failed: %s", err)
				return
			}
			c.notify("exported %d keys to %s", c.node.Model.Store.Len(), path)
		}()
	})
	f.Form.AddButton("Cancel", func() { c.closeModal() })
	c.showModal(f.Form, 70, 9, f.Form)
	return nil
}

func (c *Controller) importDialog() *tcell.EventKey {
	f := c.view.NewPathForm("Import store", defaultExportPath(),
		"Seed (re-stamp as local writes so they win)")
	f.Form.AddButton("Import", func() {
		path := strings.TrimSpace(f.Path.GetText())
		seed := f.Seed != nil && f.Seed.IsChecked()
		c.closeModal()
		if path == "" {
			return
		}
		go func() {
			data, err := os.ReadFile(path)
			if err != nil {
				c.notify("import failed: %s", err)
				return
			}
			n, err := c.node.Import(data, seed)
			if err != nil {
				c.notify("import failed: %s", err)
				return
			}
			c.notify("imported %d entries from %s", n, path)
		}()
	})
	f.Form.AddButton("Cancel", func() { c.closeModal() })
	c.showModal(f.Form, 70, 11, f.Form)
	return nil
}

func (c *Controller) toggleFeed() *tcell.EventKey {
	c.feedMu.Lock()
	c.showActivity = !c.showActivity
	c.feedMu.Unlock()
	c.fillFeed()
	return nil
}

func (c *Controller) activityShown() bool {
	c.feedMu.RLock()
	defer c.feedMu.RUnlock()
	return c.showActivity
}

func (c *Controller) fillNodes() {
	peers := c.node.Peers()
	c.view.App.QueueUpdateDraw(func() {
		c.view.NodeList.Clear()
		c.view.NodeList.SetMainTextColor(tcell.Color31)
		for _, p := range peers {
			label := p.Addr
			if p.Nick != "" {
				label = fmt.Sprintf("%s — %s", p.Addr, p.Nick)
			}
			c.view.NodeList.AddItem(label, p.Addr, 0, nil)
		}
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

// renderChat turns a structured entry into a tview line.
func (c *Controller) renderChat(e pb.ChatEntry) string {
	stamp := time.Unix(e.TS, 0).Format("15:04:05")
	text := tview.Escape(e.Text)
	own := e.Sender == c.node.Addr()

	switch e.Kind {
	case pb.ChatSystem:
		return fmt.Sprintf("[gray]%s[-] [darkgray]» %s[-]", stamp, text)
	case pb.ChatAction:
		name := c.senderName(e, own)
		return fmt.Sprintf("[gray]%s[-] [darkgray]*[-] [::b][%s]%s[-][::-] %s",
			stamp, colorFor(e.Sender), name, text)
	case pb.ChatDirect:
		if own {
			return fmt.Sprintf("[gray]%s[-] [::b][magenta]dm → %s[-][::-]: %s",
				stamp, c.peerLabel(e.To), text)
		}
		return fmt.Sprintf("[gray]%s[-] [::b][magenta]dm ← %s[-][::-]: %s",
			stamp, c.senderName(e, false), text)
	}

	if own {
		return fmt.Sprintf("[gray]%s[-] [::b][aqua]you[-][::-]: [white]%s[-]", stamp, text)
	}
	return fmt.Sprintf("[gray]%s[-] [::b][%s]%s[-][::-][gray]@%s[-]: %s",
		stamp, colorFor(e.Sender), c.senderName(e, false), e.Sender, text)
}

func (c *Controller) senderName(e pb.ChatEntry, own bool) string {
	if own {
		return "you"
	}
	if e.Nick != "" {
		return e.Nick
	}
	if nick := c.node.Model.NickOf(e.Sender); nick != "" {
		return nick
	}
	if e.Sender == "" {
		return "?"
	}
	return e.Sender
}

func (c *Controller) peerLabel(addr string) string {
	if nick := c.node.Model.NickOf(addr); nick != "" {
		return nick
	}
	return addr
}

func (c *Controller) fillFeed() {
	activity := c.activityShown()
	var lines []string
	title := "Chat"
	if activity {
		title = "Activity (replication)"
		for _, l := range c.node.Activity() {
			lines = append(lines, "[darkgray]"+tview.Escape(l)+"[-]")
		}
		if len(lines) == 0 {
			lines = append(lines, "[darkgray]nothing replicated yet[-]")
		}
	} else {
		for _, e := range c.node.Chat() {
			lines = append(lines, c.renderChat(e))
		}
	}
	body := strings.Join(lines, "\n")
	c.view.App.QueueUpdateDraw(func() {
		c.view.SetFeedTitle(title)
		c.view.Feed.Clear()
		fmt.Fprint(c.view.Feed, body)
		c.view.Feed.ScrollToEnd()
	})
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
	st := c.node.Status()

	stateTag := "[red]DEGRADED[-]"
	if st.Connected {
		stateTag = "[green]CONNECTED[-]"
	}
	tombs := ""
	if st.Tombstones > 0 {
		tombs = fmt.Sprintf("  [white]tombs:[-][gray]%d[-]", st.Tombstones)
	}

	line := fmt.Sprintf(
		" %s  [white]%s[-] [gray]([-][yellow]%s[-][gray])[-]  [white]peers:[-][cyan]%d[-]  [white]keys:[-][cyan]%d[-]%s  [white]up:[-][cyan]%s[-]",
		stateTag,
		st.Addr,
		st.Nick,
		st.Peers,
		st.Keys,
		tombs,
		formatUptime(time.Duration(st.UptimeSec)*time.Second),
	)

	c.view.App.QueueUpdateDraw(func() {
		c.view.Status.Clear()
		fmt.Fprint(c.view.Status, line)
	})
}

func (c *Controller) fillDetails() {
	st := c.node.Status()
	c.view.Details.Clear()
	fmt.Fprintf(c.view.Details, "[blue]Nick         ->[gray] %s\n", st.Nick)
	fmt.Fprintf(c.view.Details, "[blue]Node ID      ->[gray] %s\n", st.NodeID)
	fmt.Fprintf(c.view.Details, "[blue]Node Address ->[gray] %s\n", st.Addr)
	fmt.Fprintf(c.view.Details, "[blue]Cluster      ->[gray] %s\n", st.Cluster)
	seeds := strings.Join(c.node.Model.BootstrapAddrs, ", ")
	fmt.Fprintf(c.view.Details, "[green]Bootstrap    ->[white] %s\n", seeds)
	if c.node.Model.DataPath != "" {
		fmt.Fprintf(c.view.Details, "[green]Data         ->[white] %s\n", c.node.Model.DataPath)
	}
	if c.httpAdr != "" {
		fmt.Fprintf(c.view.Details, "[green]HTTP         ->[white] http://%s\n", c.httpAdr)
	}
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
	return strings.TrimSuffix(main, ttlMarker)
}

// ttlMarker flags a key that expires, so the list shows at a glance which
// values are temporary.
const ttlMarker = " ⏳"

func (c *Controller) getFilter() string {
	c.filterMu.RLock()
	defer c.filterMu.RUnlock()
	return c.filter
}

func (c *Controller) setFilter(s string) {
	c.filterMu.Lock()
	c.filter = s
	c.filterMu.Unlock()
	c.fillStore()
}

func (c *Controller) fillStore() {
	entries := c.node.Entries()
	filter := strings.ToLower(c.getFilter())

	type row struct {
		label     string
		secondary string
	}
	rows := make([]row, 0, len(entries))
	for _, e := range entries {
		if filter != "" &&
			!strings.Contains(strings.ToLower(e.Key), filter) &&
			!strings.Contains(strings.ToLower(e.Value), filter) {
			continue
		}
		label := e.Key
		secondary := previewValue(e.Value)
		if e.ExpiresAt > 0 {
			label += ttlMarker
			remaining := time.Until(time.Unix(e.ExpiresAt, 0)).Round(time.Second)
			if remaining < 0 {
				remaining = 0
			}
			secondary = fmt.Sprintf("%s  [expires in %s]", secondary, remaining)
		}
		rows = append(rows, row{label: label, secondary: secondary})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].label < rows[j].label })

	c.view.App.QueueUpdateDraw(func() {
		prev := ""
		if c.view.List.GetItemCount() > 0 {
			prev, _ = c.view.List.GetItemText(c.view.List.GetCurrentItem())
		}
		c.view.List.Clear()
		newIdx := -1
		for i, r := range rows {
			c.view.List.AddItem(r.label, r.secondary, 0, nil)
			if r.label == prev {
				newIdx = i
			}
		}
		if newIdx >= 0 {
			c.view.List.SetCurrentItem(newIdx)
		}
	})
}
