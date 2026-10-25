// Command rezoagwectl drives a running node through its HTTP gateway.
//
// Unlike every other front end in this repository it is not a peer: it holds no
// replica, joins no cluster and appears in nobody's peer list. That is the
// point — looking at a cluster should not mean becoming a member of it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/client"
	"github.com/nexusriot/rezoagwe/pkg/discovery/node"
)

// version is injected at build time with -ldflags "-X main.version=...".
var version = "dev"

// Exit codes. A script branching on what happened should not have to parse
// prose: 2 is "no such key", 3 is "someone got there first", 4 is "the cluster
// does not agree with itself".
const (
	exitOK        = 0
	exitError     = 1
	exitNotFound  = 2
	exitConflict  = 3
	exitDivergent = 4
)

const usage = `rezoagwectl — drive a rezoagwe node through its HTTP gateway

usage: rezoagwectl [flags] <command> [arguments]

store
  get <key>                 print a value
  put <key> [value]         write a value (reads stdin when omitted)
  del <key>                 delete a key
  ls [prefix]               list keys
  watch [prefix]            stream changes as they happen
  history <key>             the recorded versions of a key

cluster
  peers                     who this node can see
  health                    this node's own status
  topology                  the cluster as a graph
  doctor                    what is actually wrong, in prose
  check                     ask every replica whether they agree
  repair <key>...           make THIS node's copy of those keys win everywhere
  repair -divergent         repair every key this node's check calls divergent

data
  export [file]             write the whole store out (stdout when omitted)
  import <file>             merge an export back in
  snapshot                  ask the node to write a snapshot now
  restore -preview <file>   inspect a backup diff without writing
  restore -plan ID <keys…>  restore selected keys from that preview

chat
  chat                      print the conversation
  say <text>                send a line

  metrics                   Prometheus exposition text
  version                   print the version and exit

flags (before or after the command):
  -server URL    gateway address (default http://127.0.0.1:8080, $REZOAGWE_SERVER)
  -token TOKEN   bearer token                                  ($REZOAGWE_TOKEN)
  -durable       require journal-sync fsync acknowledgement for put
  -delete-missing include local-only keys as deletes in restore preview
  -cross-cluster allow a restore preview from another cluster
  -ttl DURATION  expiry for put, e.g. 60s
  -if-match VER  make put/del a compare-and-swap; "*" means "only if absent"
  -seed          make import win over what the cluster holds
  -plan ID       fetch a stable preview page, or select a restore plan to apply
  -preview       with repair or restore: emit a reviewable page, without writes
  -apply-plan F  with repair: apply saved preview JSON, guarding concurrent changes
  -offset N      existing repair/restore preview offset (default 0)
  -limit N       repair/existing restore preview size (default 100, maximum 500)
  -divergent     with repair: use this node's consistency check to pick the keys
  -json          machine-readable output where there is a choice
  -timeout DUR   per-request timeout (default 30s)

exit codes: 0 ok, 1 error, 2 no such key, 3 version mismatch, 4 replicas disagree
`

func main() {
	os.Exit(run())
}

func run() int {
	var (
		server        = flag.String("server", envOr("REZOAGWE_SERVER", "http://127.0.0.1:8080"), "gateway address")
		token         = flag.String("token", os.Getenv("REZOAGWE_TOKEN"), "bearer token")
		durable       = flag.Bool("durable", false, "require durable local put acknowledgement")
		deleteMissing = flag.Bool("delete-missing", false, "include keys absent from backup as restore deletions")
		crossCluster  = flag.Bool("cross-cluster", false, "allow restore across clusters")
		ttl           = flag.Duration("ttl", 0, "expiry for put")
		ifMatch       = flag.String("if-match", "", "compare-and-swap against this version")
		seed          = flag.Bool("seed", false, "make import win over what the cluster holds")
		planID        = flag.String("plan", "", "existing preview ID for paging or selected restore apply")
		preview       = flag.Bool("preview", false, "preview repair or restore without writing")
		applyPlan     = flag.String("apply-plan", "", "apply saved repair preview JSON")
		offset        = flag.Int("offset", 0, "repair preview page offset")
		limit         = flag.Int("limit", 100, "repair preview page size (max 500)")
		divergent     = flag.Bool("divergent", false, "repair every key the consistency check names")
		asJSON        = flag.Bool("json", false, "machine-readable output")
		timeout       = flag.Duration("timeout", 30*time.Second, "per-request timeout")
	)
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	// ContinueOnError so a bad flag returns here instead of exiting 2 from
	// inside the flag package — 2 is "no such key" in this tool's contract,
	// and a typo in a flag name is not that.
	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	verb, positionals, err := splitArgs(flag.CommandLine, os.Args[1:])
	if err != nil {
		flag.Usage()
		return exitError
	}
	if verb == "" {
		flag.Usage()
		return exitError
	}
	args := append([]string{verb}, positionals...)
	if verb == "version" {
		fmt.Println("rezoagwectl", version)
		return exitOK
	}

	c := client.New(client.Config{BaseURL: *server, Token: *token, Timeout: *timeout})
	cmd := &command{
		client: c, args: args[1:], asJSON: *asJSON,
		durable: *durable, deleteMissing: *deleteMissing, crossCluster: *crossCluster,
		ttl: *ttl, ifMatch: *ifMatch, seed: *seed, divergent: *divergent,
		preview: *preview, applyPlan: *applyPlan, offset: *offset, limit: *limit, planID: *planID,
	}
	code, err := cmd.dispatch(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "rezoagwectl:", err)
	}
	return code
}

// splitArgs separates the command and its positionals from flags that may
// appear anywhere among them.
//
// Go's flag package stops at the first positional, so `repair -divergent` and
// `put k v -ttl 60s` — which is how anyone writes them — left the flag sitting
// in the argument list as another key, silently. Parsing resumes after each
// positional: take one, parse what follows, repeat. A positional that really
// begins with "-" goes after "--", which the flag package already honours.
func splitArgs(fs *flag.FlagSet, argv []string) (verb string, positionals []string, err error) {
	if err := fs.Parse(argv); err != nil {
		return "", nil, err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return "", nil, nil
	}
	verb, rest = rest[0], rest[1:]
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return "", nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		rest = rest[1:]
	}
	return verb, positionals, nil
}

type command struct {
	durable, deleteMissing, crossCluster bool
	planID                               string
	preview                              bool
	applyPlan                            string
	offset, limit                        int
	client                               *client.Client
	args                                 []string
	asJSON                               bool
	ttl                                  time.Duration
	ifMatch                              string
	seed                                 bool
	divergent                            bool
}

func (c *command) dispatch(name string) (int, error) {
	switch name {
	case "get":
		return c.get()
	case "put", "set":
		return c.put()
	case "del", "delete", "rm":
		return c.del()
	case "ls", "list", "keys":
		return c.list()
	case "watch":
		return c.watch()
	case "history":
		return c.history()
	case "peers":
		return c.simple(func() (interface{}, error) { return c.client.Peers() })
	case "health", "status":
		return c.simple(func() (interface{}, error) { return c.client.Status() })
	case "topology":
		return c.simple(func() (interface{}, error) { return c.client.Topology() })
	case "activity":
		return c.activity()
	case "doctor":
		return c.doctor()
	case "check", "consistency":
		return c.check()
	case "repair":
		return c.repair()
	case "export":
		return c.export()
	case "restore":
		return c.restore()
	case "import":
		return c.importFile()
	case "snapshot":
		return c.snapshot()
	case "chat":
		return c.chat()
	case "say":
		return c.say()
	case "metrics":
		return c.metrics()
	default:
		flag.Usage()
		return exitError, fmt.Errorf("unknown command %q", name)
	}
}

// arg returns positional argument i, or "".
func (c *command) arg(i int) string {
	if i < len(c.args) {
		return c.args[i]
	}
	return ""
}

func (c *command) get() (int, error) {
	key := c.arg(0)
	if key == "" {
		return exitError, fmt.Errorf("get needs a key")
	}
	value, version, err := c.client.Get(key)
	if err != nil {
		return codeFor(err), err
	}
	if c.asJSON {
		return exitOK, writeJSON(client.Entry{Key: key, Value: value, Version: version})
	}
	// No trailing newline: a value is bytes, and `$(rezoagwectl get k)` should
	// be the value rather than the value plus whatever this chose to add.
	_, err = io.WriteString(os.Stdout, value)
	return exitOK, err
}

func (c *command) put() (int, error) {
	key := c.arg(0)
	if key == "" {
		return exitError, fmt.Errorf("put needs a key")
	}
	value := c.arg(1)
	if len(c.args) < 2 {
		// Reading stdin is what makes `... | rezoagwectl put k` work, and what
		// lets a value contain the characters a shell would otherwise eat.
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return exitError, err
		}
		value = string(data)
	}
	version, err := c.client.Put(key, value, client.PutOptions{TTL: c.ttl, IfMatch: c.ifMatch, Durable: c.durable})
	if err != nil {
		return codeFor(err), err
	}
	fmt.Println(version)
	return exitOK, nil
}

func (c *command) del() (int, error) {
	key := c.arg(0)
	if key == "" {
		return exitError, fmt.Errorf("del needs a key")
	}
	if err := c.client.Delete(key, c.ifMatch); err != nil {
		return codeFor(err), err
	}
	return exitOK, nil
}

func (c *command) list() (int, error) {
	entries, err := c.client.List(c.arg(0))
	if err != nil {
		return codeFor(err), err
	}
	if c.asJSON {
		return exitOK, writeJSON(entries)
	}
	for _, e := range entries {
		line := e.Key + "\t" + e.Value
		if e.ExpiresAt > 0 {
			line += fmt.Sprintf("\t(expires in %s)", time.Until(time.Unix(e.ExpiresAt, 0)).Round(time.Second))
		}
		fmt.Println(line)
	}
	return exitOK, nil
}

func (c *command) watch() (int, error) {
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		close(stop)
	}()

	events, errCh := c.client.Watch(c.arg(0), stop)
	for ev := range events {
		switch ev.Name {
		case "snapshot":
			if c.asJSON {
				writeJSON(map[string]interface{}{"event": "snapshot", "entries": ev.Snapshot})
				continue
			}
			for _, e := range ev.Snapshot {
				fmt.Printf("= %s\t%s\n", e.Key, e.Value)
			}
		case "dropped":
			// Loudly, and on stderr: a watch with a hole in it that says
			// nothing is worse than no watch.
			fmt.Fprintf(os.Stderr, "rezoagwectl: %d event(s) dropped; this stream has holes in it\n", ev.Dropped)
		case "kv":
			if c.asJSON {
				writeJSON(ev.Event)
				continue
			}
			u := ev.Event.Update
			mark := "+"
			if u.Action == "delete" {
				mark = "-"
			}
			fmt.Printf("%s %s\t%s\t(%s)\n", mark, u.Key, u.Value, ev.Event.Source)
		}
	}
	select {
	case err := <-errCh:
		if err != nil {
			return codeFor(err), err
		}
	default:
	}
	return exitOK, nil
}

func (c *command) history() (int, error) {
	key := c.arg(0)
	if key == "" {
		return exitError, fmt.Errorf("history needs a key")
	}
	out, err := c.client.History(key)
	if err != nil {
		return codeFor(err), err
	}
	return exitOK, writeJSON(out)
}

func (c *command) activity() (int, error) {
	lines, err := c.client.Activity()
	if err != nil {
		return codeFor(err), err
	}
	if c.asJSON {
		return exitOK, writeJSON(lines)
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return exitOK, nil
}

func (c *command) simple(get func() (interface{}, error)) (int, error) {
	v, err := get()
	if err != nil {
		return codeFor(err), err
	}
	return exitOK, writeJSON(v)
}

func (c *command) doctor() (int, error) {
	report, err := c.client.Doctor()
	if err != nil {
		return codeFor(err), err
	}
	fmt.Print(report)
	return exitOK, nil
}

// check exits 4 on a divergent cluster, so a cron job can branch on it without
// reading the report.
func (c *command) check() (int, error) {
	report, converged, err := c.client.Consistency()
	if err != nil {
		return codeFor(err), err
	}
	if err := writeJSON(report); err != nil {
		return exitError, err
	}
	if !converged {
		return exitDivergent, nil
	}
	return exitOK, nil
}

// repair is the one command here that can make a cluster worse, so it says
// what it is about to do and names the node it will do it from.
func (c *command) repair() (int, error) {
	if c.applyPlan != "" {
		data, err := os.ReadFile(c.applyPlan)
		if err != nil {
			return exitError, err
		}
		var plan node.RepairPlan
		if err := json.Unmarshal(data, &plan); err != nil {
			return exitError, err
		}
		out, err := c.client.ApplyRepairPlan(plan)
		if err != nil {
			return codeFor(err), err
		}
		if err := writeJSON(out); err != nil {
			return exitError, err
		}
		if !out.Complete {
			return 4, nil
		}
		return exitOK, nil
	}
	if c.preview && c.planID != "" {
		out, err := c.client.RepairPlanPage(c.planID, c.offset, c.limit)
		if err != nil {
			return codeFor(err), err
		}
		return exitOK, writeJSON(out)
	}
	if c.preview {
		out, err := c.client.PreviewRepair(c.args, c.divergent, c.offset, c.limit)
		if err != nil {
			return codeFor(err), err
		}
		return exitOK, writeJSON(out)
	}
	if c.divergent {
		out, err := c.client.RepairDivergent()
		if err != nil {
			return codeFor(err), err
		}
		return exitOK, writeJSON(out)
	}
	if len(c.args) == 0 {
		return exitError, fmt.Errorf(
			"repair needs the keys to repair, or -divergent to use this node's consistency check")
	}
	out, err := c.client.Repair(c.args)
	if err != nil {
		return codeFor(err), err
	}
	return exitOK, writeJSON(out)
}

func (c *command) export() (int, error) {
	data, err := c.client.Export()
	if err != nil {
		return codeFor(err), err
	}
	if path := c.arg(0); path != "" {
		return exitOK, os.WriteFile(path, data, 0o644)
	}
	_, err = os.Stdout.Write(data)
	return exitOK, err
}

func (c *command) importFile() (int, error) {
	path := c.arg(0)
	var data []byte
	var err error
	if path == "" || path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return exitError, err
	}
	applied, err := c.client.Import(data, c.seed)
	if err != nil {
		return codeFor(err), err
	}
	fmt.Printf("%d entr%s applied\n", applied, plural(applied))
	return exitOK, nil
}

func (c *command) snapshot() (int, error) {
	path, err := c.client.Snapshot()
	if err != nil {
		return codeFor(err), err
	}
	fmt.Println(path)
	return exitOK, nil
}

func (c *command) chat() (int, error) {
	log, err := c.client.Chat()
	if err != nil {
		return codeFor(err), err
	}
	if c.asJSON {
		return exitOK, writeJSON(log)
	}
	for _, e := range log {
		nick, _ := e["nick"].(string)
		if nick == "" {
			nick, _ = e["sender"].(string)
		}
		text, _ := e["text"].(string)
		fmt.Printf("<%s> %s\n", nick, text)
	}
	return exitOK, nil
}

func (c *command) say() (int, error) {
	text := strings.Join(c.args, " ")
	if text == "" {
		return exitError, fmt.Errorf("say needs something to say")
	}
	if err := c.client.Say(text); err != nil {
		return codeFor(err), err
	}
	return exitOK, nil
}

func (c *command) metrics() (int, error) {
	out, err := c.client.Metrics()
	if err != nil {
		return codeFor(err), err
	}
	fmt.Print(out)
	return exitOK, nil
}

// codeFor maps an error to the exit code a script should branch on.
func codeFor(err error) int {
	switch {
	case client.NotFound(err):
		return exitNotFound
	case client.Conflict(err):
		return exitConflict
	default:
		return exitError
	}
}

func writeJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func (c *command) restore() (int, error) {
	if c.preview {
		if c.planID != "" {
			p, err := c.client.RepairPlanPage(c.planID, c.offset, c.limit)
			if err != nil {
				return codeFor(err), err
			}
			return exitOK, writeJSON(p)
		}
		if len(c.args) != 1 {
			return exitError, fmt.Errorf("restore -preview requires a backup file")
		}
		data, err := os.ReadFile(c.args[0])
		if err != nil {
			return exitError, err
		}
		p, err := c.client.PreviewRestore(data, c.deleteMissing, c.crossCluster)
		if err != nil {
			return codeFor(err), err
		}
		return exitOK, writeJSON(p)
	}
	if c.planID == "" || len(c.args) == 0 {
		return exitError, fmt.Errorf("restore requires -preview FILE, or -plan ID and explicit keys")
	}
	report, err := c.client.ApplyRestore(c.planID, c.args)
	if err != nil {
		return codeFor(err), err
	}
	if err = writeJSON(report); err != nil {
		return exitError, err
	}
	if !report.Complete {
		return 4, nil
	}
	return exitOK, nil
}
