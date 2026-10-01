package plugins

import (
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/audit"
	"smart-mzcmc/app/models"
)

// CSVExport CSV 导出插件
type CSVExport struct {
	enabled bool
}

func NewCSVExport() *CSVExport {
	return &CSVExport{enabled: true}
}

// NewCSVExportWith 按配置构造，便于停用后仍能在后台看到「已停用」。
func NewCSVExportWith(enabled bool) *CSVExport {
	return &CSVExport{enabled: enabled}
}

func (c *CSVExport) Name() string        { return "csv-export" }
func (c *CSVExport) Version() string     { return "1.0.0" }
func (c *CSVExport) OnEvent(event Event) {}
func (c *CSVExport) Stop()               {}

func (c *CSVExport) Describe() Descriptor {
	d := Descriptor{
		Name:        c.Name(),
		Version:     c.Version(),
		Description: "日志导出接口：JSON 全量导出与 CSV 导出",
		Enabled:     c.enabled,
		Config:      map[string]string{},
	}
	if !c.enabled {
		d.Reason = "已通过 PLUGIN_CSV_EXPORT_ENABLED=false 关闭"
	}
	return d
}

// ListPluginsHandler 列出所有已注册插件，含生效配置与真实开关状态
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
	// ID == 0 的判断不能省：First 查不到记录时不返回错误（只把结构体留成零值），
	// 只判 error 的话 lockExists 恒为 true，项目统计里的「锁定中」永远是开的。
	// 说明见 app/http/middleware/jwt.go。
	lockExists := facades.Orm().Query().Where("project_id = ?", projectID).
		First(&lock) == nil && lock.ID != 0
	lockActive := lockExists && time.Now().Before(lock.ExpireAt)

	interviewCount, _ := facades.Orm().Query().Model(&models.InterviewStatus{}).Where("project_id = ?", projectID).Count()

	// 切台统计。shot_cuts 是这一轮新增的表，有了它，「这个项目切了多少次台」
	// 才是一个能查出来的事实，而不是靠翻 messages 里的 JSON 猜。
	cutCount, _ := facades.Orm().Query().Model(&models.ShotCut{}).
		Where("project_id = ?", projectID).Count()
	avgDwell := averageShotDwell(uint(projectID))

	return ctx.Response().Json(200, map[string]any{
		"project_id":             projectID,
		"message_count":          msgCount,
		"lock_active":            lockActive,
		"lock_holder":            lock.UserID,
		"interview_points":       interviewCount,
		"shot_cut_count":         cutCount,
		"avg_shot_dwell_seconds": avgDwell,
		"timestamp":              time.Now().Format(time.RFC3339),
	})
}

// averageShotDwell 返回平均停留时长（秒）。
//
// 最后一段没有后续切台，停留多久无从得知，所以不参与平均——按「到现在为止」
// 算会让这个数字随着你盯着屏幕的时间不断变大。
func averageShotDwell(projectID uint) float64 {
	var cuts []models.ShotCut
	if err := facades.Orm().Query().Select("to_shot", "cut_at").
		Where("project_id = ?", projectID).
		OrderBy("cut_at").Find(&cuts); err != nil || len(cuts) < 2 {
		return 0
	}

	var total float64
	segments := 0
	for i := 0; i+1 < len(cuts); i++ {
		dwell := cuts[i+1].CutAt.Sub(cuts[i].CutAt).Seconds()
		if dwell < 0 {
			continue
		}
		total += dwell
		segments++
	}
	if segments == 0 {
		return 0
	}
	return total / float64(segments)
}

// CleanupLogsHandler 手动清理过期日志
//
// 这个接口会真的删数据，而删除的历史记录本身就是审计对象——所以清理动作
// 必须自己留下痕迹，否则「谁把证据清了」永远查不出来。
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

	if actor, ok := audit.ActorFrom(ctx); ok {
		audit.Write(ctx, actor, audit.Record{
			Action:     "logs.cleanup",
			Summary:    fmt.Sprintf("清理 %d 天前的协调日志，删除 %d 条", days, result.RowsAffected),
			TargetType: "message",
			Detail: map[string]any{
				"days":          days,
				"rows_affected": result.RowsAffected,
				"cutoff":        cutoff.Format(time.RFC3339),
			},
		})
	} else {
		// 走到这里说明路由上的角色守卫没生效，属于配置错误而不是正常路径。
		// 记下来比静默删掉要好。
		log.Printf("[Logs] 清理了 %d 条日志，但上下文中没有操作者，未能写入审计", result.RowsAffected)
	}

	return ctx.Response().Json(200, map[string]any{
		"message": fmt.Sprintf("已清理 %d 条日志 (>%d天)", result.RowsAffected, days),
		"count":   result.RowsAffected,
	})
}
