package main

import (
	"flag"
	"time"

	"github.com/nexusriot/rezoagwe/pkg/bootstrap/controller"
)

func main() {
	port := flag.Int("port", 9999, "port number")
	flag.Parse()
	controller.NewController(true, *port, 15*time.Second).Start()
}
