package controllers

import (
	"log"
	"strconv"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
)

// AuditController 提供操作审计的读取接口。
//
// 之前管理后台那个名叫「日志审计」的页面显示的其实是 messages 表里的
// 协调日志（谁来切了台、谁发了内部消息），跟「谁改了别人的角色、
// 谁清掉了日志」完全是两回事。B4 之后两者分开：协调日志留在 /api/logs，
// 操作审计走这里。
type AuditController struct{}

func NewAuditController() *AuditController {
	return &AuditController{}
}

// maxAuditLimit 是审计列表单页上限。
const maxAuditLimit = 500

// List 按条件分页查询审计记录。
//
// 与 /api/logs 用同一套游标分页：审计表也是只追加、按时间倒序翻，
// offset 分页在写入持续进行时会重复或漏行。
func (c *AuditController) List(ctx http.Context) http.Response {
	action := ctx.Request().Input("action", "")
	actorID := ctx.Request().Input("actor_id", "")
	fromRaw := ctx.Request().Input("from", "")
	toRaw := ctx.Request().Input("to", "")
	cursorRaw := ctx.Request().Input("cursor", "")

	limit, _ := strconv.Atoi(ctx.Request().Input("limit", "100"))
	if limit <= 0 || limit > maxAuditLimit {
		limit = 100
	}

	query := facades.Orm().Query().Model(&models.AuditLog{})

	if action != "" {
		query = query.Where("action = ?", action)
	}
	if actorID != "" {
		query = query.Where("actor_id = ?", actorID)
	}
	if from, ok := parseTimeFilter(fromRaw); ok {
		query = query.Where("created_at >= ?", from)
	} else if fromRaw != "" {
		return ctx.Response().Json(400, map[string]any{"error": "from 时间格式不正确"})
	}
	if to, ok := parseTimeFilter(toRaw); ok {
		query = query.Where("created_at <= ?", to)
	} else if toRaw != "" {
		return ctx.Response().Json(400, map[string]any{"error": "to 时间格式不正确"})
	}

	total, err := query.Count()
	if err != nil {
		log.Printf("[Audit] 统计审计记录失败: %v", err)
		return ctx.Response().Json(500, map[string]any{"error": "查询审计记录失败"})
	}

	pageQuery := query
	if cursorRaw != "" {
		cursor, err := strconv.ParseUint(cursorRaw, 10, 64)
		if err != nil {
			return ctx.Response().Json(400, map[string]any{"error": "cursor 参数无效"})
		}
		pageQuery = pageQuery.Where("id < ?", cursor)
	}

	var logs []models.AuditLog
	if err := pageQuery.OrderByDesc("id").Limit(limit).Find(&logs); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询审计记录失败"})
	}

	nextCursor := uint64(0)
	if len(logs) == limit && len(logs) > 0 {
		nextCursor = uint64(logs[len(logs)-1].ID)
	}

	// 一并给出出现过的动作类型，供界面渲染筛选下拉——硬编码一份动作清单
	// 在前后端两边，加一个审计点就会漏改（这个项目已经在角色名上踩过一次）。
	actions := make([]string, 0)
	if err := facades.Orm().Query().Model(&models.AuditLog{}).
		Distinct("action").OrderBy("action").Pluck("action", &actions); err != nil {
		log.Printf("[Audit] 查询动作类型失败: %v", err)
	}

	return ctx.Response().Json(200, map[string]any{
		"total":       total,
		"logs":        logs,
		"next_cursor": nextCursor,
		"has_more":    nextCursor != 0,
		"actions":     actions,
	})
}
