package main

import (
	"flag"

	"github.com/nexusriot/rezoagwe/pkg/discovery/controller"
)

func main() {

	bootstrapAddr := flag.String("bootstrap", ":9999", "bootstrap address")
	nodeAddr := flag.String("node", ":3137", "node address")
	flag.Parse()

	controller.NewController(true, *bootstrapAddr, *nodeAddr).Start()
}
