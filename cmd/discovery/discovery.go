package main

import (
	"github.com/nexusriot/rezoagwe/pkg/discovery/controller"
)

func main() {
	controller.NewController(true, 9999).Start()
}
