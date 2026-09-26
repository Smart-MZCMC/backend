package controllers

import (
	"github.com/goravel/framework/contracts/http"

	"smart-mzcmc/app/ws"
)

type StatusController struct{}

// Version 当前后端版本号。会随发布手动同步，改动见 CHANGELOG。
const Version = "1.1.0"

func NewStatusController() *StatusController {
	return &StatusController{}
}

func (c *StatusController) ServerStatus(ctx http.Context) http.Response {
	return ctx.Response().Json(200, map[string]any{
		"status":       "running",
		"online_count": ws.DefaultHub.GetOnlineCount(),
		"version":      Version,
	})
}
