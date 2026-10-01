package controllers

import (
	"log"
	"os"
	"path/filepath"
	"sync"
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
		Enabled:        cfg.GetBool("update.enabled", false),
		Server:         cfg.GetString("update.server", ""),
		Repo:           cfg.GetString("update.repo", "Smart-MZCMC/backend"),
		Asset:          cfg.GetString("update.asset", "backend-linux-amd64.tar.gz"),
		AllowReplace:   cfg.GetBool("update.allow_replace", false),
		Token:          cfg.GetString("update.token", ""),
		Timeout:        time.Duration(timeout) * time.Second,
		DownloadMirror: cfg.GetString("update.download_mirror", ""),
		ChecksumURL:    cfg.GetString("update.checksum_url", ""),
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
			"enabled":         cfg.GetBool("update.enabled", false),
			"allow_replace":   cfg.GetBool("update.allow_replace", false),
			"repo":            cfg.GetString("update.repo", "Smart-MZCMC/backend"),
			"asset":           cfg.GetString("update.asset", "backend-linux-amd64.tar.gz"),
			"server":          cfg.GetString("update.server", ""),
			"download_mirror": cfg.GetString("update.download_mirror", ""),
			"checksum_url":    cfg.GetString("update.checksum_url", ""),
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

// applyMutex 保证同一时刻只有一个更新任务在跑。
//
// 不是可有可无的保护：更新会写自己的可执行文件。两个任务并发时第二个会拿到
// 第一个刚备份的/刚替换的中间态，失败时连回滚的依据都没有。
var applyMutex sync.Mutex

// ApplyUpdate 启动一次更新，并立即返回。
//
// 原来是同步的——下载 26 MB、校验、解压、备份、替换、迁移全在一个 HTTP 请求里
// 跑完，界面上只能显示「更新中…」和一个不动的按钮。用户既不知道卡在哪一步，
// 也不知道是在下载还是在解压；校园网下这个过程可能好几分钟，期间那个请求还
// 随时可能因为反向代理超时而被掐断，一次失败就是从头再来。
//
// 现在改成：立刻返回「已启动」，界面轮询 /api/system/update/progress 看进度。
// 进度由 updater 包的包级状态承载（更新跑在 goroutine 里，进度查询是另一个
// 请求）。
//
// 危险操作，且只有超级管理员能到（路由层已限制）。调用方必须显式传
// confirm=true 与目标版本号 target_version——后者是为了消除竞态：检查时看到
// 的是 v1.2.0，下载时更新源可能已经变成 v1.3.0，界面上确认的还是前者。
//
// 替换成功后本进程会退出，由 systemd 拉起新版本。
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

	// 用 TryLock 而不是 Lock：已经有任务在跑时立刻拒绝，而不是让第二次请求
	// 在这里干等到前一个结束。界面上重复点「更新」时那句提示更有用。
	if !applyMutex.TryLock() {
		return ctx.Response().Json(409, map[string]any{
			"error": "已有一次更新正在执行，请等它结束",
		})
	}

	exe, err := os.Executable()
	if err != nil {
		applyMutex.Unlock()
		return ctx.Response().Json(500, map[string]any{"error": "无法定位当前程序"})
	}
	exe, _ = filepath.EvalSymlinks(exe)

	log.Printf("[Update] %s(#%d, %s) 申请更新到 %s", actor.Username, actor.ID, actor.Role, target)

	updater.BeginProgress()
	go func() {
		defer applyMutex.Unlock()

		res, err := newUpdater().ApplyWithProgress(Version, target, filepath.Base(exe), nil)
		if err != nil {
			log.Printf("[Update] 更新失败: %v", err)
			updater.FailProgress(err.Error())
			return
		}
		updater.FinishProgress(res)

		if res.Replaced {
			// 先把结果写回去再退出：进程一停，界面就再也拿不到任何信息。
			// 这段等待是给前端的最后一次轮询留的时间——进度条上最后那一下
			// 「完成」必须让人看见，否则用户只看到一条连接被掐断的报错，
			// 分不清是更新失败了还是服务正在重启。
			time.Sleep(3 * time.Second)
			log.Printf("[Update] 新版本 %s 已就绪，退出进程交由进程管理器重启", res.Version)
			os.Exit(0)
		}
	}()

	return ctx.Response().Json(200, map[string]any{
		"started": true,
		"message": "更新已启动，请查看进度条",
	})
}

// UpdateProgress 返回当前更新的进度。
//
// 没有任务在跑时返回 stage=idle 的空进度而不是 404——「还没开始更新」是正常
// 状态，前端据此把进度条收起来。
func (c *SystemController) UpdateProgress(ctx http.Context) http.Response {
	if _, aerr := actorFrom(ctx); aerr != nil {
		return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
	}
	return ctx.Response().Json(200, updater.CurrentProgress())
}
