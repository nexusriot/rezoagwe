package main

import (
	"flag"

	"github.com/nexusriot/rezoagwe/pkg/discovery/controller"
	"github.com/nexusriot/rezoagwe/pkg/discovery/model"
)

func main() {
	bootstrapAddr := flag.String("bootstrap", ":9999", "bootstrap address")
	nodeAddr := flag.String("node", ":3137", "node address")
	nick := flag.String("nick", "anon", "chat nickname")
	data := flag.String("data", "", "KV persistence file (default: <config dir>/rezoagwe/<node>.json; \"-\" disables)")
	flag.Parse()

	dataPath := *data
	switch dataPath {
	case "":
		dataPath = model.DefaultDataPath(*nodeAddr)
	case "-":
		dataPath = "" // persistence disabled
	}

	controller.NewController(true, *bootstrapAddr, *nodeAddr, *nick, dataPath).Start()
}
