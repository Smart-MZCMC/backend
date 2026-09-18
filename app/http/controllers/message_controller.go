package controllers

import (
	"strconv"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
)

type MessageController struct{}

func NewMessageController() *MessageController {
	return &MessageController{}
}

func (c *MessageController) ListByProject(ctx http.Context) http.Response {
	projectID := ctx.Request().Route("projectId")
	limit, _ := strconv.Atoi(ctx.Request().Input("limit", "50"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	var messages []models.Message
	if err := facades.Orm().Query().
		Where("project_id = ?", projectID).
		OrderByDesc("created_at").
		Limit(limit).
		Find(&messages); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询消息失败"})
	}

	return ctx.Response().Json(200, messages)
}

func (c *MessageController) ListLogs(ctx http.Context) http.Response {
	projectID := ctx.Request().Input("project_id")
	msgType := ctx.Request().Input("type")
	limit, _ := strconv.Atoi(ctx.Request().Input("limit", "100"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	query := facades.Orm().Query().Model(&models.Message{})

	if projectID != "" {
		query = query.Where("project_id = ?", projectID)
	}
	if msgType != "" {
		query = query.Where("type = ?", msgType)
	}

	var messages []models.Message
	if err := query.OrderByDesc("created_at").Limit(limit).Find(&messages); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询日志失败"})
	}

	return ctx.Response().Json(200, map[string]any{
		"total":    len(messages),
		"messages": messages,
	})
}
