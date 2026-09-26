package config

import (
	"smart-mzcmc/app/facades"
)

func init() {
	config := facades.Config()
	config.Add("plugins", map[string]any{
		// ntfy-alert：把导播掉线、控制权超时等事件推到 ntfy。
		//
		// server 与 topic 都配置齐了才会真正启用；只配一个视为配置错误并停用，
		// 以免运行时静默丢告警。配置值会透出到管理后台的「插件与统计」页。
		"ntfy": map[string]any{
			"server": config.Env("NTFY_SERVER", ""),
			"topic":  config.Env("NTFY_TOPIC", ""),
			// 显式关闭开关。留空时按「server+topic 是否齐全」自动判断。
			"enabled": config.Env("PLUGIN_NTFY_ENABLED", ""),
		},

		// log-archive：定期删除超过保留天数的消息日志。
		"log_archive": map[string]any{
			"enabled":        config.Env("PLUGIN_LOG_ARCHIVE_ENABLED", true),
			"retention_days": config.Env("PLUGIN_LOG_RETENTION_DAYS", 30),
			// 清理检查间隔，支持 Go duration 写法（30s / 5m / 1h）。
			"check_interval": config.Env("PLUGIN_LOG_CHECK_INTERVAL", "1h"),
		},

		// csv-export：日志导出接口（JSON / CSV）。
		"csv_export": map[string]any{
			"enabled": config.Env("PLUGIN_CSV_EXPORT_ENABLED", true),
		},
	})
}
