package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/nexusriot/rezoagwe/pkg/discovery/controller"
	"github.com/nexusriot/rezoagwe/pkg/discovery/httpapi"
	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
	"github.com/nexusriot/rezoagwe/pkg/discovery/node"
)

// version is injected at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	bootstrapAddr := flag.String("bootstrap", ":9999", "bootstrap address, or a comma-separated list of seeds")
	nodeAddr := flag.String("node", ":3137", "address to bind")
	advertise := flag.String("advertise", "", "address peers should use to reach this node (default: -node)")
	nick := flag.String("nick", "anon", "chat nickname")
	data := flag.String("data", "", "state file (default: <config dir>/rezoagwe/<node>.json; \"-\" disables)")
	psk := flag.String("psk", "", "pre-shared key authenticating every packet")
	cluster := flag.String("cluster", "rezoagwe", "cluster name; nodes only talk to their own cluster")
	httpAddr := flag.String("http", "", "serve the HTTP gateway on this address, e.g. :8080")
	httpToken := flag.String("http-token", "", "require this bearer token on every gateway request")
	httpCert := flag.String("http-tls-cert", "", "serve the gateway over TLS with this certificate")
	httpKey := flag.String("http-tls-key", "", "private key for -http-tls-cert")
	httpReadOnly := flag.Bool("http-readonly", false, "refuse every mutating gateway request")
	maxValue := flag.Int("max-value-bytes", 0, "refuse values longer than this (0 disables)")
	maxKeys := flag.Int("max-keys", 0, "refuse writes that would exceed this many live keys (0 disables)")
	doctor := flag.Bool("doctor", false, "start a node, join, print a diagnostics report, then exit")
	tombstoneTTL := flag.Duration("tombstone-ttl", 0, "reclaim tombstones older than this (0 disables GC)")
	headless := flag.Bool("headless", false, "run without the TUI (pair with -http to drive the node)")
	debug := flag.Bool("debug", false, "enable debug logging (requires -logfile)")
	logFile := flag.String("logfile", "", "write logs to this file instead of discarding them")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("rezoagwe-discovery", version)
		return
	}

	// The TUI owns the terminal: anything written to stderr scribbles over it.
	// Logs go to a file or nowhere at all.
	closeLog, err := setupLogging(*debug, *logFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logging:", err)
		os.Exit(1)
	}
	defer closeLog()

	dataPath := *data
	switch dataPath {
	case "":
		dataPath = model.DefaultDataPath(*nodeAddr)
	case "-":
		dataPath = "" // persistence disabled
	}

	cfg := node.Config{
		BootstrapAddrs: splitSeeds(*bootstrapAddr),
		NodeAddr:       *nodeAddr,
		AdvertiseAddr:  *advertise,
		Nick:           *nick,
		DataPath:       dataPath,
		PSK:            *psk,
		Cluster:        *cluster,
		Version:        version,
		TombstoneTTL:   *tombstoneTTL,
		Limits:         model.Limits{MaxValueBytes: *maxValue, MaxKeys: *maxKeys},
	}
	httpCfg := httpapi.Config{
		Addr:     *httpAddr,
		Token:    *httpToken,
		TLSCert:  *httpCert,
		TLSKey:   *httpKey,
		ReadOnly: *httpReadOnly,
	}

	if *doctor {
		if err := runDoctor(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if *headless {
		if err := runHeadless(cfg, httpCfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	ctrl, err := controller.NewController(cfg, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start node:", err)
		os.Exit(1)
	}

	if httpCfg.Addr != "" {
		srv, err := httpapi.Serve(httpCfg, ctrl.Node())
		if err != nil {
			fmt.Fprintln(os.Stderr, "start http gateway:", err)
			os.Exit(1)
		}
		defer srv.Close()
		ctrl.SetHTTPAddr(srv.Addr())
	}

	if err := ctrl.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}
}

// gatewayNote says out loud what the gateway will and will not refuse, since
// an unauthenticated write surface is not something to discover later.
func gatewayNote(cfg httpapi.Config) string {
	var notes []string
	if cfg.Token == "" {
		notes = append(notes, "no token: anyone who can reach it can write")
	}
	if cfg.ReadOnly {
		notes = append(notes, "read-only")
	}
	if len(notes) == 0 {
		return ""
	}
	return " (" + strings.Join(notes, ", ") + ")"
}

// splitSeeds parses a comma-separated seed list. Several seeds mean the
// rendezvous service is no longer a single point of failure for joiners.
func splitSeeds(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// setupLogging points logrus at a file (or discards it) and sets the level.
//
// -debug used to be inert: the level was never raised, and even if it had been,
// stderr output would have corrupted the tview screen.
func setupLogging(debug bool, path string) (func(), error) {
	level := log.InfoLevel
	if debug {
		level = log.DebugLevel
	}
	log.SetLevel(level)
	if path == "" {
		log.SetOutput(io.Discard)
		return func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	log.SetOutput(f)
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true, TimestampFormat: time.RFC3339})
	return func() { f.Close() }, nil
}

// runDoctor starts a node, gives it long enough to hear from the cluster, and
// prints what is wrong with it.
//
// The most common rezoagwe problem is a node that starts cleanly and joins
// nothing, and the evidence for why has always been spread across a counter, a
// log line and a peer list. This puts it on one screen without a TUI.
func runDoctor(cfg node.Config) error {
	n, err := node.New(cfg, nil)
	if err != nil {
		return fmt.Errorf("start node: %w", err)
	}
	defer n.Stop()
	n.Start()
	n.Join()

	// Long enough for a heartbeat and a gossip reply; a report taken before
	// then says "no peers" about every healthy node there is.
	settle := cfg.HeartbeatInterval
	if settle <= 0 {
		settle = 5 * time.Second
	}
	time.Sleep(settle * 2)

	fmt.Print(n.DiagnosticsReport())
	return nil
}

// runHeadless serves a node with no terminal UI, which is what makes a node
// scriptable: pair it with -http and the store is a REST service.
func runHeadless(cfg node.Config, httpCfg httpapi.Config) error {
	n, err := node.New(cfg, nil)
	if err != nil {
		return fmt.Errorf("start node: %w", err)
	}
	n.Start()
	go n.Join()
	defer n.Stop()

	if httpCfg.Addr != "" {
		srv, err := httpapi.Serve(httpCfg, n)
		if err != nil {
			return fmt.Errorf("start http gateway: %w", err)
		}
		defer srv.Close()
		fmt.Printf("rezoagwe-discovery %s on %s (cluster %s), gateway on %s%s\n",
			version, cfg.NodeAddr, cfg.Cluster, srv.URL(), gatewayNote(httpCfg))
	} else {
		fmt.Printf("rezoagwe-discovery %s on %s (cluster %s)\n", version, cfg.NodeAddr, cfg.Cluster)
	}

	// Ctrl+C has to reach Stop, or peers wait out the eviction timeout instead
	// of being told this node left.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	return nil
}
