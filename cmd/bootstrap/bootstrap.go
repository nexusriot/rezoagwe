package main

import (
	"flag"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/bootstrap/controller"
)

func main() {
	port := flag.Int("port", 9999, "port number")
	flag.Parse()

	// 1000s timeout is fine for the PoC.
	_ = controller.NewController(true, *port, 1000*time.Second).Start()
}
