package controllers

import (
	"github.com/goravel/framework/contracts/http"

	"smart-mzcmc/app/ws"
)

type StatusController struct{}

func NewStatusController() *StatusController {
	return &StatusController{}
}

func (c *StatusController) ServerStatus(ctx http.Context) http.Response {
	return ctx.Response().Json(200, map[string]any{
		"status":       "running",
		"online_count": ws.DefaultHub.GetOnlineCount(),
		"version":      "1.0.0",
	})
}
