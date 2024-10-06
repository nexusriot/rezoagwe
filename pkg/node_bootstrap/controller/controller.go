package controller

import (
	"github.com/nexusriot/rezoagwe/pkg/node_bootstrap/model"
	"github.com/nexusriot/rezoagwe/pkg/node_bootstrap/view"
)

type Controller struct {
	debug bool
	view  *view.View
	model *model.Model
}
