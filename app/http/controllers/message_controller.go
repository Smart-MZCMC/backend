package controllers

import (
	"log"
	"strconv"
	"time"

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

// maxLogLimit 是 /api/logs 单页返回的上限。
const maxLogLimit = 500

// parseTimeFilter 解析日志筛选用的时间参数。
//
// 必须接受 `YYYY-MM-DDTHH:mm` 这种**没有秒**的写法：浏览器的
// `<input type="datetime-local">` 默认就是分钟精度，管理后台的时间筛选控件
// 直接把它提交上来。少了这一条，界面上选完时间一查询就是 400
// 「from 时间格式不正确」，而这个错误信息完全指不到真正的原因。
func parseTimeFilter(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// isDateOnly 报告一个时间参数是不是「只有日期、没有时间」。
//
// 用它来决定「结束时间要不要补到当天 23:59:59」：用户选到 2 月 1 日，
// 期望的是包含 2 月 1 日整天，而不是 2 月 1 日 00:00 之前。
func isDateOnly(raw string) bool {
	return len(raw) == len("2006-01-02")
}

// ListLogs 按条件分页查询协调日志。
//
// 这里同时修掉一个「界面对使用者撒谎」的问题：total 之前返回的是
// len(messages)，也就是被 limit 截断后这一页的长度。首页的「消息累计」
// StatCard 喂的是 listLogs({limit: 8})，于是那个数字恒定不超过 8——
// 屏幕上摆着一个错的具体数字比没有更糟。现在 total 是同条件下的真实行数。
//
// 分页用游标（cursor）而不是 offset：日志表按 created_at 倒序，翻页期间
// 新消息不断写入，offset 会让同一条记录被重复看到或整段跳过。
func (c *MessageController) ListLogs(ctx http.Context) http.Response {
	projectID := ctx.Request().Input("project_id")
	msgType := ctx.Request().Input("type")
	senderID := ctx.Request().Input("sender_id", "")
	fromRaw := ctx.Request().Input("from", "")
	toRaw := ctx.Request().Input("to", "")
	cursorRaw := ctx.Request().Input("cursor", "")

	limit, _ := strconv.Atoi(ctx.Request().Input("limit", "100"))
	if limit <= 0 || limit > maxLogLimit {
		limit = 100
	}

	query := facades.Orm().Query().Model(&models.Message{})

	if projectID != "" {
		query = query.Where("project_id = ?", projectID)
	}
	if msgType != "" {
		query = query.Where("type = ?", msgType)
	}
	if senderID != "" {
		query = query.Where("sender_id = ?", senderID)
	}

	if from, ok := parseTimeFilter(fromRaw); ok {
		query = query.Where("created_at >= ?", from)
	} else if fromRaw != "" {
		return ctx.Response().Json(400, map[string]any{"error": "from 时间格式不正确"})
	}
	if to, ok := parseTimeFilter(toRaw); ok {
		// 纯日期的 to 要算到当天结束，否则用户选到 2 月 1 日却拿不到当天的
		// 记录，看起来像丢数据。带具体时刻的（含 datetime-local 的分钟精度）
		// 不加，那是用户明确指定的截止点。
		if isDateOnly(toRaw) {
			to = to.Add(24*time.Hour - time.Nanosecond)
		}
		query = query.Where("created_at <= ?", to)
	} else if toRaw != "" {
		return ctx.Response().Json(400, map[string]any{"error": "to 时间格式不正确"})
	}

	// 真实总数。注意它必须与上面的筛选条件完全一致，否则界面会出现
	// 「共 300 条，本页 100 条」但翻页翻不到底的情况。
	total, err := query.Count()
	if err != nil {
		log.Printf("[Logs] 统计日志总数失败: %v", err)
		return ctx.Response().Json(500, map[string]any{"error": "查询日志失败"})
	}

	// 游标：上一页最后一条的 ID。倒序翻页即「取 ID 更小的」。
	pageQuery := query
	if cursorRaw != "" {
		cursor, err := strconv.ParseUint(cursorRaw, 10, 64)
		if err != nil {
			return ctx.Response().Json(400, map[string]any{"error": "cursor 参数无效"})
		}
		pageQuery = pageQuery.Where("id < ?", cursor)
	}

	var messages []models.Message
	if err := pageQuery.OrderByDesc("id").Limit(limit).Find(&messages); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询日志失败"})
	}

	nextCursor := uint64(0)
	if len(messages) == limit && len(messages) > 0 {
		nextCursor = uint64(messages[len(messages)-1].ID)
	}

	return ctx.Response().Json(200, map[string]any{
		"total":       total,
		"messages":    messages,
		"next_cursor": nextCursor,
		"has_more":    nextCursor != 0,
	})
}
