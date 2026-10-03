package controllers

import (
	"log"
	"path/filepath"
	"time"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/sysinfo"
	"smart-mzcmc/app/ws"
)

// Metrics 返回服务自身的运行指标。
//
// 只给超级管理员：这里有可执行文件路径、工作目录、宿主机文件系统用量——属于
// 「知道的人越少越好」的那类信息，与 /api/system/info 同一档门槛。
//
// 设计上刻意做成**永不失败**：数据库探活失败、拿不到磁盘用量，这些都只体现在返回
// 的状态位里，接口本身仍然返回 200。监控页最需要显示内容的时刻就是出故障的时刻，
// 那时因为一次探活失败就返回 500，操作员看到的只有一个报错。
func (c *SystemController) Metrics(ctx http.Context) http.Response {
	if _, aerr := actorFrom(ctx); aerr != nil {
		return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
	}

	cfg := facades.Config()
	dbPath := cfg.GetString("database.connections.sqlite.database", "database/smart-mzcmc.db")

	// 数据目录取库文件所在目录：数据库、日志、程序通常在同一个分区上，
	// 「磁盘满」是这几个一起出问题的，监控也该一起看。
	dataDir := filepath.Dir(dbPath)

	var online int
	if ws.DefaultHub != nil {
		online = ws.DefaultHub.GetOnlineCount()
	}

	snap := sysinfo.Collect(sysinfo.Params{
		Version:      Version,
		StartedAt:    startedAt,
		OnlineCount:  online,
		Driver:       cfg.GetString("database.default", "sqlite"),
		DatabasePath: dbPath,
		DataDir:      dataDir,
		AppDirs: map[string]string{
			"database": dbPath,
			"storage":  "storage",
			"update":   "update",
		},
		Ping:          probeDatabase,
		UpdateEnabled: cfg.GetBool("update.enabled", false),
	})

	return ctx.Response().Json(200, snap)
}

// probeDatabase 复用 /api/health 的那一次查询，外加耗时与 SQLite 版本。
//
// 连通性直接调 pingDatabase 而不是自己写一条 SELECT 1：那个函数查的是 users 表，
// 「迁移没跑」也会被它发现——那种情况下库文件在、表全空，只查 SELECT 1 是看不出来的。
func probeDatabase(_ string) sysinfo.PingResult {
	start := time.Now()
	dbErr := pingDatabase()
	res := sysinfo.PingResult{
		Connected: dbErr == nil,
		LatencyMS: time.Since(start).Milliseconds(),
	}
	if dbErr != nil {
		log.Printf("[SysInfo] 数据库探活失败: %v", dbErr)
		res.Error = dbErr.Error()
		return res
	}

	// PRAGMA 只存在于 SQLite。将来换驱动时这里会因为语法错误返回空串，
	// 版本显示为空、不影响其他指标——刻意不为一个展示字段加驱动分支。
	var version []struct{ Version string }
	if err := facades.DB().Select(&version, "SELECT sqlite_version() AS version"); err == nil {
		if len(version) > 0 {
			res.Version = version[0].Version
		}
	}
	return res
}
