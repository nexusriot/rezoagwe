package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	b64 "encoding/base64"
	"github.com/cretz/bine/tor"
	"github.com/cretz/bine/torutil/ed25519"
	"github.com/ipsn/go-libtor/libtor"
)

func main() {
	// Start tor with some defaults + elevated verbosity
	fmt.Println("Starting and registering onion service, please wait a bit...")
	t, err := tor.Start(nil, &tor.StartConf{ProcessCreator: libtor.Creator, DebugWriter: os.Stderr})
	if err != nil {
		log.Panicf("Failed to start tor: %v", err)
	}
	defer t.Close()

	// Wait at most a few minutes to publish the service
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	//  dd if=/dev/urandom bs=64 count=1 status=none| base64 -w100
	_b64Ed25519 := "bRkKPcy2xujTnziXN20tkrZGNHztDL6i26ff+Uni/XXJ9C90LGpBYNncMrHn9noq+p0qyiHBE8W5QSfn6hnvhg=="
	_key, _ := b64.StdEncoding.DecodeString(_b64Ed25519)

	// Create an onion service to listen on any port but show as 80
	onion, err := t.Listen(ctx, &tor.ListenConf{RemotePorts: []int{80}, Version3: true, Key: ed25519.PrivateKey(_key)})
	if err != nil {
		log.Panicf("Failed to create onion service: %v", err)
	}
	defer onion.Close()

	fmt.Printf("Please open a Tor capable browser and navigate to http://%v.onion\n", onion.ID)

	// Run a Hello-World HTTP service until terminated
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello, World!")
	})
	http.Serve(onion, nil)
}
