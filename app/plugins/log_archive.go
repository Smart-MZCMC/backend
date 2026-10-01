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

// maxExportRows 是单次导出允许落盘的最大行数。
//
// 30 天保留期下，一个活跃项目可能有几十万条消息。此前两处导出都没有
// Limit，等于把整表读进内存再拼成一个几百 MB 的 JSON 同步写盘——期间
// 这个进程的内存峰值和磁盘占用都是不可控的。截断比 OOM 好。
const maxExportRows = 20000

// exportFilePrefix 是导出文件名的前缀，清理时据此只动本插件自己产生的文件，
// 不会误删别人放进 storage/exports 的东西。
const exportFilePrefix = "logs_project_"

// LogArchive 日志归档插件：定期清理超过保留天数的消息日志
type LogArchive struct {
	retentionDays int
	checkInterval time.Duration
	stopCh        chan struct{}
	stopOnce      sync.Once
	enabled       bool
	reason        string

	// presenceScan 是注入进来的「采访端掉线扫描」函数。
	//
	// 用回调而不是直接 import app/ws：ws 已经 import 了本包（hub.go 要发
	// 插件事件），这里再反向 import 就成环了。
	// 顺带复用了本插件那个本来只用来清日志的 goroutine，不必再起一个
	// 只为扫状态的后台服务。
	presenceScan     func(time.Duration)
	presenceInterval time.Duration
	presenceTimeout  time.Duration
}

// LogArchiveConfig 日志归档插件配置，来自 config/plugins.go。
type LogArchiveConfig struct {
	Enabled       bool
	RetentionDays int
	// CheckInterval 支持 Go duration 写法（30s / 5m / 1h）。无法解析时回退 1h。
	CheckInterval string
	// PresenceInterval / PresenceTimeout 同理，用于采访端掉线扫描。
	// 留空时分别回退 60s / 90s。
	PresenceInterval string
	PresenceTimeout  string
}

func NewLogArchive(cfg LogArchiveConfig) *LogArchive {
	l := &LogArchive{
		retentionDays:    cfg.RetentionDays,
		checkInterval:    time.Hour,
		stopCh:           make(chan struct{}),
		enabled:          cfg.Enabled,
		presenceInterval: 60 * time.Second,
		presenceTimeout:  90 * time.Second,
	}

	if l.retentionDays <= 0 {
		l.retentionDays = 30
	}
	if d, err := time.ParseDuration(cfg.CheckInterval); err == nil && d > 0 {
		l.checkInterval = d
	} else if cfg.CheckInterval != "" {
		log.Printf("[LogArchive] 无法解析的检查间隔 %q，回退为 1h", cfg.CheckInterval)
	}
	if d, err := time.ParseDuration(cfg.PresenceInterval); err == nil && d > 0 {
		l.presenceInterval = d
	} else if cfg.PresenceInterval != "" {
		log.Printf("[LogArchive] 无法解析的掉线扫描间隔 %q，回退为 60s", cfg.PresenceInterval)
	}
	if d, err := time.ParseDuration(cfg.PresenceTimeout); err == nil && d > 0 {
		l.presenceTimeout = d
	} else if cfg.PresenceTimeout != "" {
		log.Printf("[LogArchive] 无法解析的掉线判定阈值 %q，回退为 90s", cfg.PresenceTimeout)
	}

	if !cfg.Enabled {
		l.reason = "已通过 PLUGIN_LOG_ARCHIVE_ENABLED=false 关闭"
	}
	return l
}

func (l *LogArchive) Name() string    { return "log-archive" }
func (l *LogArchive) Version() string { return "1.0.0" }

func (l *LogArchive) OnEvent(event Event) {}

// SetPresenceScanner 注入采访端掉线扫描函数。
//
// 必须在 Start 之前调用：Start 会立刻把参数读出来交给 goroutine。
func (l *LogArchive) SetPresenceScanner(fn func(time.Duration)) {
	l.presenceScan = fn
}

func (l *LogArchive) Describe() Descriptor {
	return Descriptor{
		Name:        l.Name(),
		Version:     l.Version(),
		Description: "定期删除超过保留天数的消息日志，并扫描失联的采访端",
		Enabled:     l.enabled,
		Reason:      l.reason,
		Config: map[string]string{
			"retention_days":    fmt.Sprintf("%d 天", l.retentionDays),
			"check_interval":    l.checkInterval.String(),
			"presence_interval": l.presenceInterval.String(),
			"presence_timeout":  l.presenceTimeout.String(),
		},
	}
}

// Start 启动定时清理。插件被停用时不启动清理，事件也不会被处理。
//
// 但掉线扫描照常启动：插件开关的本意是「别删日志」，不该顺带把
// 「采访端掉线能不能被发现」一起关掉——那是两件事。
func (l *LogArchive) Start() {
	if !l.enabled {
		log.Printf("[LogArchive] 已停用: %s", l.reason)
		if l.presenceScan != nil {
			go l.run(false)
		}
		return
	}
	go l.run(true)
}

func (l *LogArchive) run(includeCleanup bool) {
	if includeCleanup {
		log.Printf("[LogArchive] 启动，保留 %d 天日志，每 %v 检查一次；采访端掉线每 %v 扫描一次（阈值 %v）",
			l.retentionDays, l.checkInterval, l.presenceInterval, l.presenceTimeout)
	} else {
		log.Printf("[LogArchive] 仅运行采访端掉线扫描，每 %v 一次（阈值 %v）",
			l.presenceInterval, l.presenceTimeout)
	}

	ticker := time.NewTicker(l.checkInterval)
	defer ticker.Stop()

	// 掉线扫描要快得多，所以是独立的 ticker，只是共用这个 goroutine。
	// 没有它的话，采访端走出 WiFi 后最多要等一小时才会变红。
	presenceTicker := time.NewTicker(l.presenceInterval)
	defer presenceTicker.Stop()

	scan := l.presenceScan
	timeout := l.presenceTimeout

	for {
		select {
		case <-ticker.C:
			if includeCleanup {
				l.cleanup()
			}
		case <-presenceTicker.C:
			if scan != nil {
				scan(timeout)
			}
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
	} else if result.RowsAffected > 0 {
		log.Printf("[LogArchive] 已清理 %d 条过期日志 (>%d天)", result.RowsAffected, l.retentionDays)
	}

	// 导出文件一并清理。
	//
	// storage/exports 里的文件是导出接口写下来的，但前端下载走的是浏览器端
	// 自己拼 blob 的路径，**服务端这份没有任何人读**。不在这里删，它就会
	// 一直涨到把磁盘塞满为止。
	l.cleanupExports(cutoff)
}

// cleanupExports 删除 storage/exports 下超过保留期的导出文件。
func (l *LogArchive) cleanupExports(cutoff time.Time) {
	dir := ExportDir()

	entries, err := os.ReadDir(dir)
	if err != nil {
		// 目录不存在是正常情况（还没导出过），不值得报警。
		if !os.IsNotExist(err) {
			log.Printf("[LogArchive] 读取导出目录失败: %v", err)
		}
		return
	}

	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// 只动本插件自己产生的文件，避免误删别人放进这个目录的东西。
		if len(entry.Name()) < len(exportFilePrefix) ||
			entry.Name()[:len(exportFilePrefix)] != exportFilePrefix {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			log.Printf("[LogArchive] 删除导出文件 %s 失败: %v", entry.Name(), err)
			continue
		}
		removed++
	}
	if removed > 0 {
		log.Printf("[LogArchive] 已清理 %d 个过期导出文件 (>%d天)", removed, l.retentionDays)
	}
}

func (l *LogArchive) Stop() {
	// 用 sync.Once 保证幂等：未 Start 过也能安全 Stop，重复调用不会 panic。
	l.stopOnce.Do(func() { close(l.stopCh) })
}

// ExportDir 返回导出文件的落盘目录。
func ExportDir() string {
	return filepath.Join("storage", "exports")
}

// ExportRange 是一次导出请求的时间窗。
type ExportRange struct {
	From time.Time
	To   time.Time
}

// parseTimeParam 解析导出接口的时间参数。
//
// 同时接受 RFC3339 与「2026-01-02」两种写法：前者是接口文档里的形式，
// 后者是人在浏览器里手敲或从管理后台日期选择器上直接拿到的形式。
func parseTimeParam(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析的时间格式 %q", raw)
}

// parseExportRange 读取并校验导出接口的 from/to 参数。
//
// 时间范围是**必填**的。此前两处导出都是对整个项目历史做无条件 Find，
// 既没有时间边界也没有行数上限，一次误点就可能把几百 MB 灌进内存。
func parseExportRange(ctx http.Context) (ExportRange, error) {
	from, err := parseTimeParam(ctx.Request().Input("from", ""))
	if err != nil {
		return ExportRange{}, err
	}
	to, err := parseTimeParam(ctx.Request().Input("to", ""))
	if err != nil {
		return ExportRange{}, err
	}
	if from.IsZero() || to.IsZero() {
		return ExportRange{}, fmt.Errorf("必须提供 from 与 to 时间范围（示例：2026-01-01 与 2026-02-01）")
	}
	// 「只有日期」的 to 要算到当天结束，否则用户选到 2 月 1 日却拿不到
	// 2 月 1 日当天的数据，看起来像丢数据。
	if to.Hour() == 0 && to.Minute() == 0 && to.Second() == 0 {
		to = to.Add(24*time.Hour - time.Nanosecond)
	}
	if !to.After(from) {
		return ExportRange{}, fmt.Errorf("to 必须晚于 from")
	}
	return ExportRange{From: from, To: to}, nil
}

// queryMessagesForExport 按时间窗取消息，并强制行数上限。
func queryMessagesForExport(projectID uint, rng ExportRange) ([]models.Message, error) {
	var messages []models.Message
	if err := facades.Orm().Query().
		Where("project_id = ?", projectID).
		Where("created_at >= ?", rng.From).
		Where("created_at <= ?", rng.To).
		OrderBy("created_at").
		Limit(maxExportRows).
		Find(&messages); err != nil {
		return nil, err
	}
	return messages, nil
}

// ExportLogsHandler 导出项目日志为 JSON 文件
func ExportLogsHandler(ctx http.Context) http.Response {
	projectIDStr := ctx.Request().Input("project_id", "0")
	projectID, _ := strconv.ParseUint(projectIDStr, 10, 64)
	if projectID == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "需要 project_id 参数"})
	}

	rng, err := parseExportRange(ctx)
	if err != nil {
		return ctx.Response().Json(400, map[string]any{"error": err.Error()})
	}

	messages, err := queryMessagesForExport(uint(projectID), rng)
	if err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询日志失败"})
	}

	outputDir := ExportDir()
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "创建目录失败"})
	}

	filename := fmt.Sprintf("%s%d_%s.json", exportFilePrefix, projectID, time.Now().Format("20060102_150405"))
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

	log.Printf("[LogArchive] 导出 %d 条日志到 %s（%s ~ %s）",
		len(messages), filePath, rng.From.Format(time.RFC3339), rng.To.Format(time.RFC3339))

	return ctx.Response().Json(200, map[string]any{
		"message": "导出成功",
		"file":    filePath,
		"count":   len(messages),
		"from":    rng.From,
		"to":      rng.To,
		// 触顶说明还有数据没导出，界面要提示用户缩小时间范围再导一次。
		"truncated": len(messages) >= maxExportRows,
		"limit":     maxExportRows,
	})
}

// ExportLogsCSVHandler 导出项目日志为 CSV 文件
func ExportLogsCSVHandler(ctx http.Context) http.Response {
	projectIDStr := ctx.Request().Input("project_id", "0")
	projectID, _ := strconv.ParseUint(projectIDStr, 10, 64)
	if projectID == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "需要 project_id 参数"})
	}

	rng, err := parseExportRange(ctx)
	if err != nil {
		return ctx.Response().Json(400, map[string]any{"error": err.Error()})
	}

	messages, err := queryMessagesForExport(uint(projectID), rng)
	if err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "查询日志失败"})
	}

	outputDir := ExportDir()
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "创建目录失败"})
	}

	filename := fmt.Sprintf("%s%d_%s.csv", exportFilePrefix, projectID, time.Now().Format("20060102_150405"))
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

	log.Printf("[CSVExport] 导出 %d 条日志到 %s（%s ~ %s）",
		len(messages), filePath, rng.From.Format(time.RFC3339), rng.To.Format(time.RFC3339))

	return ctx.Response().Json(200, map[string]any{
		"message":   "导出成功",
		"file":      filePath,
		"count":     len(messages),
		"from":      rng.From,
		"to":        rng.To,
		"truncated": len(messages) >= maxExportRows,
		"limit":     maxExportRows,
	})
}
