package controllers

import (
	"os"
	"runtime"
	"time"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
	"smart-mzcmc/app/ws"
)

// 进程启动时刻，接口里换算成秒。
//
// 之前系统只有 /api/status，它不碰数据库也不看时间，所以数据库整个不见了它
// 照样返回 200 {"status":"running"}。真出故障时最难处理的正是「进程活着但
// 不可用」——监控看不出来，人也看不出来。
var startedAt = time.Now()

type HealthController struct{}

func NewHealthController() *HealthController {
	return &HealthController{}
}

// Health 报告后端是否真的可用。
//
// 判定是「数据库能否完成一次最小查询」。只 ping 连接不够：连接池在文件被挪走
// 或磁盘出问题时可能还握着 fd，要真的发一条查询才会暴露。
//
// 公开路由：反向代理与监控探针没有登录态。但只回答布尔值与版本号，
// 不返回路径、主机名等部署细节，所以不构成信息泄露。数据库报错详情只给
// 超级管理员（见 healthDetail）。
func (c *HealthController) Health(ctx http.Context) http.Response {
	dbErr := pingDatabase()

	// 数据库不可用时返回 503，让负载均衡与监控能真正把它摘下去。
	// 返回 200 等于告诉探针「一切正常」，那这个探针就白配了。
	status := 200
	body := "ok"
	if dbErr != nil {
		status = 503
		body = "unhealthy"
	}

	return ctx.Response().Json(status, map[string]any{
		"status":         body,
		"version":        Version,
		"uptime_seconds": int64(time.Since(startedAt).Seconds()),
		"checks":         map[string]any{"database": dbErr == nil},
		"detail":         healthDetail(ctx, dbErr),
	})
}

// healthDetail 只对超级管理员返回数据库错误详情。
//
// 探针会把整个响应体记进日志，无条件带上驱动名与文件路径等于把内部信息
// 散到各处。
func healthDetail(ctx http.Context, dbErr error) any {
	if dbErr == nil {
		return nil
	}
	actor, aerr := actorFrom(ctx)
	if aerr != nil || !models.Role(actor.Role).AtLeast(models.RoleSuperAdmin) {
		return nil
	}
	return dbErr.Error()
}

// pingDatabase 用一次最小查询确认数据库可用。
//
// 用 models.User 而不是匿名 struct：Goravel 的 ORM 依赖模型元数据解析字段
// 映射，查进匿名 struct 会直接失败（表现为「记录不存在」）。users 表不存在
// 也确实应该算不健康——那意味着迁移没跑。
func pingDatabase() error {
	_, err := facades.Orm().Query().Model(&models.User{}).Count()
	return err
}

// runtimeInfo 汇总运行环境，供系统设置页展示。
func runtimeInfo() map[string]any {
	exe, _ := os.Executable()
	wd, _ := os.Getwd()

	var online int
	if ws.DefaultHub != nil {
		online = ws.DefaultHub.GetOnlineCount()
	}

	return map[string]any{
		"pid":            os.Getpid(),
		"go_version":     runtime.Version(),
		"platform":       runtime.GOOS + "/" + runtime.GOARCH,
		"num_cpu":        runtime.NumCPU(),
		"goroutines":     runtime.NumGoroutine(),
		"executable":     exe,
		"working_dir":    wd,
		"uptime_seconds": int64(time.Since(startedAt).Seconds()),
		"online_count":   online,
	}
}
