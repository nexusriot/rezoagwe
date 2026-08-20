package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/nexusriot/rezoagwe/pkg/bootstrap/controller"
	"github.com/nexusriot/rezoagwe/pkg/bootstrap/model"
	"github.com/nexusriot/rezoagwe/pkg/bootstrap/server"
)

// version is injected at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	port := flag.Int("port", 9999, "listen port (UDP and TCP)")
	timeout := flag.Duration("timeout", 30*time.Second, "drop a node that has not registered within this window")
	data := flag.String("data", "", "roster file (default: <config dir>/rezoagwe/bootstrap-<port>.json; \"-\" disables)")
	psk := flag.String("psk", "", "pre-shared key authenticating every packet")
	cluster := flag.String("cluster", "rezoagwe", "cluster name; must match the nodes")
	debug := flag.Bool("debug", false, "enable debug logging (requires -logfile)")
	logFile := flag.String("logfile", "", "write logs to this file instead of discarding them")
	headless := flag.Bool("headless", false, "run without the TUI")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("rezoagwe-bootstrap", version)
		return
	}

	closeLog, err := setupLogging(*debug, *logFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logging:", err)
		os.Exit(1)
	}
	defer closeLog()

	dataPath := *data
	switch dataPath {
	case "":
		dataPath = model.DefaultDataPath(*port)
	case "-":
		dataPath = ""
	}

	cfg := server.Config{
		Port:        *port,
		NodeTimeout: *timeout,
		DataPath:    dataPath,
		PSK:         *psk,
		Cluster:     *cluster,
	}

	if *headless {
		if err := runHeadless(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	ctrl, err := controller.NewController(cfg, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start bootstrap:", err)
		os.Exit(1)
	}
	if err := ctrl.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}
}

// setupLogging points logrus at a file (or discards it) and sets the level.
// The TUI owns the terminal, so stderr logging is never an option.
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

// runHeadless serves the rendezvous role with no terminal UI, for a machine
// that has no terminal to give it.
func runHeadless(cfg server.Config) error {
	srv, err := server.New(cfg)
	if err != nil {
		return fmt.Errorf("start bootstrap: %w", err)
	}
	srv.Start()
	defer srv.Stop()
	fmt.Printf("rezoagwe-bootstrap %s on udp+tcp :%d (cluster %s)\n", version, cfg.Port, cfg.Cluster)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	return nil
}
