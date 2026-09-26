package plugins

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
)

// LogArchive 日志归档插件：定期清理超过保留天数的消息日志
type LogArchive struct {
	retentionDays int
	checkInterval time.Duration
	stopCh        chan struct{}
	stopOnce      sync.Once
	enabled       bool
	reason        string
}

// LogArchiveConfig 日志归档插件配置，来自 config/plugins.go。
type LogArchiveConfig struct {
	Enabled       bool
	RetentionDays int
	// CheckInterval 支持 Go duration 写法（30s / 5m / 1h）。无法解析时回退 1h。
	CheckInterval string
}

func NewLogArchive(cfg LogArchiveConfig) *LogArchive {
	l := &LogArchive{
		retentionDays: cfg.RetentionDays,
		checkInterval: time.Hour,
		stopCh:        make(chan struct{}),
		enabled:       cfg.Enabled,
	}

	if l.retentionDays <= 0 {
		l.retentionDays = 30
	}
	if d, err := time.ParseDuration(cfg.CheckInterval); err == nil && d > 0 {
		l.checkInterval = d
	} else if cfg.CheckInterval != "" {
		log.Printf("[LogArchive] 无法解析的检查间隔 %q，回退为 1h", cfg.CheckInterval)
	}

	if !cfg.Enabled {
		l.reason = "已通过 PLUGIN_LOG_ARCHIVE_ENABLED=false 关闭"
	}
	return l
}

func (l *LogArchive) Name() string    { return "log-archive" }
func (l *LogArchive) Version() string { return "1.0.0" }

func (l *LogArchive) OnEvent(event Event) {}

func (l *LogArchive) Describe() Descriptor {
	return Descriptor{
		Name:        l.Name(),
		Version:     l.Version(),
		Description: "定期删除超过保留天数的消息日志",
		Enabled:     l.enabled,
		Reason:      l.reason,
		Config: map[string]string{
			"retention_days": fmt.Sprintf("%d 天", l.retentionDays),
			"check_interval": l.checkInterval.String(),
		},
	}
}

// Start 启动定时清理。插件被停用时不启动，事件也不会被处理。
func (l *LogArchive) Start() {
	if !l.enabled {
		log.Printf("[LogArchive] 已停用: %s", l.reason)
		return
	}
	go l.run()
}

func (l *LogArchive) run() {
	log.Printf("[LogArchive] 启动，保留 %d 天日志，每 %v 检查一次", l.retentionDays, l.checkInterval)
	ticker := time.NewTicker(l.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			l.cleanup()
		case <-l.stopCh:
			return
		}
	}
}

func (l *LogArchive) cleanup() {
	cutoff := time.Now().AddDate(0, 0, -l.retentionDays)
	result, err := facades.Orm().Query().Where("created_at < ?", cutoff).Delete(&models.Message{})
	if err != nil {
		log.Printf("[LogArchive] 清理失败: %v", err)
		return
	}
	if result.RowsAffected > 0 {
		log.Printf("[LogArchive] 已清理 %d 条过期日志 (>%d天)", result.RowsAffected, l.retentionDays)
	}
}

func (l *LogArchive) Stop() {
	// 用 sync.Once 保证幂等：未 Start 过也能安全 Stop，重复调用不会 panic。
	l.stopOnce.Do(func() { close(l.stopCh) })
}

// ExportLogsHandler 导出项目日志为 JSON 文件
func ExportLogsHandler(ctx http.Context) http.Response {
	projectIDStr := ctx.Request().Input("project_id", "0")
	projectID, _ := strconv.ParseUint(projectIDStr, 10, 64)
	if projectID == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "需要 project_id 参数"})
	}

	outputDir := filepath.Join("storage", "exports")

	var messages []models.Message
	if err := facades.Orm().Query().
		Where("project_id = ?", projectID).
		OrderByDesc("created_at").
		Find(&messages); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询日志失败"})
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "创建目录失败"})
	}

	filename := fmt.Sprintf("logs_project_%d_%s.json", projectID, time.Now().Format("20060102_150405"))
	filePath := filepath.Join(outputDir, filename)

	file, err := os.Create(filePath)
	if err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "创建文件失败"})
	}
	defer file.Close()

	file.WriteString("[\n")
	for i, m := range messages {
		line := fmt.Sprintf(`  {"id":%d,"project_id":%d,"sender_id":%d,"type":"%s","content":%s,"created_at":"%s"}`,
			m.ID, m.ProjectID, m.SenderID, m.Type,
			strconv.Quote(m.Content), m.CreatedAt.Format("2006-01-02T15:04:05Z"))
		if i < len(messages)-1 {
			line += ","
		}
		file.WriteString(line + "\n")
	}
	file.WriteString("]\n")

	log.Printf("[LogArchive] 导出 %d 条日志到 %s", len(messages), filePath)

	return ctx.Response().Json(200, map[string]any{
		"message": "导出成功",
		"file":    filePath,
		"count":   len(messages),
	})
}

// ExportLogsCSVHandler 导出项目日志为 CSV 文件
func ExportLogsCSVHandler(ctx http.Context) http.Response {
	projectIDStr := ctx.Request().Input("project_id", "0")
	projectID, _ := strconv.ParseUint(projectIDStr, 10, 64)
	if projectID == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "需要 project_id 参数"})
	}

	outputDir := filepath.Join("storage", "exports")

	var messages []models.Message
	if err := facades.Orm().Query().
		Where("project_id = ?", projectID).
		OrderByDesc("created_at").
		Find(&messages); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询日志失败"})
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "创建目录失败"})
	}

	filename := fmt.Sprintf("logs_project_%d_%s.csv", projectID, time.Now().Format("20060102_150405"))
	filePath := filepath.Join(outputDir, filename)

	file, err := os.Create(filePath)
	if err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "创建文件失败"})
	}
	defer file.Close()

	file.WriteString("\xEF\xBB\xBF") // UTF-8 BOM
	file.WriteString("ID,ProjectID,SenderID,Type,Content,CreatedAt\n")

	for _, m := range messages {
		content := m.Content
		if len(content) > 0 && (content[0] == '"' || content[0] == ',' || content[0] == '\n') {
			content = `"` + content + `"`
		}
		line := fmt.Sprintf("%d,%d,%d,%s,%s,%s\n",
			m.ID, m.ProjectID, m.SenderID, m.Type,
			content, m.CreatedAt.Format("2006-01-02 15:04:05"))
		file.WriteString(line)
	}

	log.Printf("[CSVExport] 导出 %d 条日志到 %s", len(messages), filePath)

	return ctx.Response().Json(200, map[string]any{
		"message": "导出成功",
		"file":    filePath,
		"count":   len(messages),
	})
}
