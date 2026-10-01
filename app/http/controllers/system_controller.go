package controllers

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
	"smart-mzcmc/app/updater"
)

type SystemController struct{}

func NewSystemController() *SystemController {
	return &SystemController{}
}

// newUpdater 从配置构造更新器。
func newUpdater() *updater.Updater {
	cfg := facades.Config()
	timeout := cfg.GetInt("update.timeout", 60)
	return updater.New(updater.Config{
		Enabled:      cfg.GetBool("update.enabled", false),
		Server:       cfg.GetString("update.server", ""),
		Repo:         cfg.GetString("update.repo", "Smart-MZCMC/backend"),
		Asset:        cfg.GetString("update.asset", "backend-linux-amd64.tar.gz"),
		AllowReplace: cfg.GetBool("update.allow_replace", false),
		Token:        cfg.GetString("update.token", ""),
		Timeout:      time.Duration(timeout) * time.Second,
	}, func(format string, a ...any) { log.Printf("[Update] "+format, a...) })
}

// Info 返回运行环境与更新配置，供系统设置页展示。
func (c *SystemController) Info(ctx http.Context) http.Response {
	actor, aerr := actorFrom(ctx)
	if aerr != nil {
		return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
	}

	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)

	cfg := facades.Config()
	return ctx.Response().Json(200, map[string]any{
		"version":    Version,
		"runtime":    runtimeInfo(),
		"executable": exe,
		"updater": map[string]any{
			"enabled":       cfg.GetBool("update.enabled", false),
			"allow_replace": cfg.GetBool("update.allow_replace", false),
			"repo":          cfg.GetString("update.repo", "Smart-MZCMC/backend"),
			"asset":         cfg.GetString("update.asset", "backend-linux-amd64.tar.gz"),
			"server":        cfg.GetString("update.server", ""),
		},
		"operated_by": map[string]any{
			"username":   actor.Username,
			"role":       actor.Role,
			"role_label": models.Role(actor.Role).Label(),
		},
	})
}

// UpdateStatus 检查是否有新版本。
//
// 只读操作，无论配置如何都可以调用——「为什么查不到更新」本身就需要能看到。
func (c *SystemController) UpdateStatus(ctx http.Context) http.Response {
	if _, aerr := actorFrom(ctx); aerr != nil {
		return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
	}

	res := newUpdater().Check(Version)
	status := 200
	// 更新源不可达不算「接口出错」，而是把原因放进 error 字段返回 200：
	// 前端要显示「连不上更新源」，而不是一个没有细节的 500。
	if res.Error != "" {
		log.Printf("[Update] 检查更新失败: %s", res.Error)
	}
	return ctx.Response().Json(status, res)
}

// ApplyUpdate 下载并应用新版本。
//
// 危险操作，且只有超级管理员能到（路由层已限制）。调用方必须显式传
// confirm=true 与目标版本号 target_version——后者是为了消除竞态：检查时看到
// 的是 v1.2.0，下载时更新源可能已经变成 v1.3.0，界面上确认的还是前者。
//
// 替换成功后本进程会退出，由 systemd 拉起新版本。这一步放在最后，且只在
// 配置允许替换时才会走到。
func (c *SystemController) ApplyUpdate(ctx http.Context) http.Response {
	actor, aerr := actorFrom(ctx)
	if aerr != nil {
		return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
	}

	if !ctx.Request().InputBool("confirm", false) {
		return ctx.Response().Json(400, map[string]any{
			"error": "该操作会下载并可能替换服务程序，必须显式传 confirm=true 确认",
		})
	}
	target := ctx.Request().Input("target_version", "")
	if target == "" {
		return ctx.Response().Json(400, map[string]any{
			"error": "必须传 target_version（你在界面上确认的那个版本号）",
		})
	}

	exe, err := os.Executable()
	if err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "无法定位当前程序"})
	}
	exe, _ = filepath.EvalSymlinks(exe)

	log.Printf("[Update] %s(#%d, %s) 申请更新到 %s", actor.Username, actor.ID, actor.Role, target)

	res, err := newUpdater().Apply(Version, target, filepath.Base(exe))
	if err != nil {
		log.Printf("[Update] 更新失败: %v", err)
		return ctx.Response().Json(502, map[string]any{"error": err.Error()})
	}

	auditAction(ctx, actor, "执行在线更新到 "+res.Version)

	// 先把结果写回去再退出：进程一停，这个响应就发不出去了，
	// 界面会一直转圈，用户不知道到底成没成。
	if res.Replaced {
		go func() {
			// 留一点时间让响应真正发出去。
			time.Sleep(1500 * time.Millisecond)
			log.Printf("[Update] 新版本 %s 已就绪，退出进程交由进程管理器重启", res.Version)
			os.Exit(0)
		}()
	}

	return ctx.Response().Json(200, res)
}
