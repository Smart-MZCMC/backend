package plugins

import (
	"fmt"
	"strconv"
	"time"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
)

// CSVExport CSV 导出插件
type CSVExport struct{}

func NewCSVExport() *CSVExport {
	return &CSVExport{}
}

func (c *CSVExport) Name() string    { return "csv-export" }
func (c *CSVExport) Version() string { return "1.0.0" }
func (c *CSVExport) OnEvent(event Event) {}
func (c *CSVExport) Stop() {}

// ListPluginsHandler 列出所有已注册插件
func ListPluginsHandler(ctx http.Context) http.Response {
	plugins := List()
	return ctx.Response().Json(200, plugins)
}

// ProjectStatsHandler 项目统计
func ProjectStatsHandler(ctx http.Context) http.Response {
	projectIDStr := ctx.Request().Route("projectId")
	projectID, _ := strconv.ParseUint(projectIDStr, 10, 64)

	msgCount, _ := facades.Orm().Query().Model(&models.Message{}).Where("project_id = ?", projectID).Count()

	var lock models.ProjectLock
	lockExists := facades.Orm().Query().Where("project_id = ?", projectID).First(&lock) == nil
	lockActive := lockExists && time.Now().Before(lock.ExpireAt)

	interviewCount, _ := facades.Orm().Query().Model(&models.InterviewStatus{}).Where("project_id = ?", projectID).Count()

	return ctx.Response().Json(200, map[string]any{
		"project_id":       projectID,
		"message_count":    msgCount,
		"lock_active":      lockActive,
		"lock_holder":      lock.UserID,
		"interview_points": interviewCount,
		"timestamp":        time.Now().Format(time.RFC3339),
	})
}

// ExportProjectLogsHandler 导出项目完整日志
func ExportProjectLogsHandler(ctx http.Context) http.Response {
	projectIDStr := ctx.Request().Input("project_id", "0")
	projectID, _ := strconv.ParseUint(projectIDStr, 10, 64)
	if projectID == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "需要 project_id 参数"})
	}

	var messages []models.Message
	if err := facades.Orm().Query().
		Where("project_id = ?", projectID).
		With("Sender").
		OrderByDesc("created_at").
		Limit(1000).
		Find(&messages); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询日志失败"})
	}

	return ctx.Response().Json(200, map[string]any{
		"project_id": projectID,
		"count":      len(messages),
		"messages":   messages,
	})
}

// CleanupLogsHandler 手动清理过期日志
func CleanupLogsHandler(ctx http.Context) http.Response {
	days, _ := strconv.Atoi(ctx.Request().Input("days", "30"))
	if days <= 0 {
		days = 30
	}

	cutoff := time.Now().AddDate(0, 0, -days)
	result, err := facades.Orm().Query().Where("created_at < ?", cutoff).Delete(&models.Message{})
	if err != nil {
		return ctx.Response().Json(500, map[string]any{"error": fmt.Sprintf("清理失败: %v", err)})
	}

	return ctx.Response().Json(200, map[string]any{
		"message": fmt.Sprintf("已清理 %d 条日志 (>%d天)", result.RowsAffected, days),
		"count":   result.RowsAffected,
	})
}
