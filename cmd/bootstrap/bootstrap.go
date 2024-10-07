package main

import (
	"time"

	"github.com/nexusriot/rezoagwe/pkg/bootstrap/controller"
)

func main() {
	controller.NewController(true, 9999, 1000*time.Second).Start()
}
