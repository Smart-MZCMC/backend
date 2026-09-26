package routes

import (
	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/contracts/route"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/http/controllers"
	"smart-mzcmc/app/http/middleware"
	"smart-mzcmc/app/plugins"
)

func Web() {
	// 静态资源
	facades.Route().Static("public", "./public")

	// 根路由 - 系统首页
	//
	// 这里必须显式写 Cache-Control：Response().File() 走 gin 的静态文件分支，
	// 只带 Last-Modified 不带缓存指令，浏览器会套用启发式缓存，导致改完首页
	// 刷新仍显示旧内容（见 staticSite.go 里 cacheControlFor 的说明）。
	facades.Route().Get("/", func(ctx http.Context) http.Response {
		ctx.Response().Header("Cache-Control", "no-cache, must-revalidate")
		return ctx.Response().File("./public/index.html")
	})

	// 管理后台（/admin，SvelteKit 静态构建，SPA 路由）
	// 与文档站（/docs，VitePress 静态构建，cleanUrls）的托管，
	// 统一由 routes.StaticSites() 全局中间件处理，
	// 在 bootstrap/app.go 的 WithMiddleware 中挂载。

	// 状态检查
	statusController := controllers.NewStatusController()
	facades.Route().Get("/api/status", statusController.ServerStatus)

	// === 认证路由（公开） ===
	authController := controllers.NewAuthController()
	facades.Route().Post("/api/auth/login", authController.Login)
	facades.Route().Post("/api/auth/register", authController.Register)

	// === 需要认证的路由 ===
	facades.Route().Middleware(middleware.Jwt()).Group(func(r route.Router) {
		r.Get("/api/auth/profile", authController.Profile)

		adminController := controllers.NewAdminController()
		// 管理接口除 JWT 外还要校验角色：Jwt 中间件只验证令牌有效，
		// 不关心持有者身份。少了这一层，任何登录用户（包括最低权限的导播）
		// 都能列出全部用户与项目、增删项目、分配权限。
		r.Prefix("/api/admin").Middleware(middleware.RequireRole("admin")).Group(func(ar route.Router) {
			ar.Get("/users", adminController.ListUsers)
			ar.Delete("/users/:id", adminController.DeleteUser)
			ar.Put("/users/:id/role", adminController.UpdateUserRole)
			ar.Get("/projects", adminController.ListProjects)
			ar.Post("/projects", adminController.CreateProject)
			ar.Put("/projects/:id", adminController.UpdateProject)
			ar.Delete("/projects/:id", adminController.DeleteProject)
			ar.Post("/assign", adminController.AssignProject)
			ar.Post("/revoke", adminController.RevokeProject)
			ar.Get("/users/:id/projects", adminController.ListUserProjects)
		})

		lockController := controllers.NewLockController()
		r.Prefix("/api/locks").Group(func(lr route.Router) {
			lr.Post("/:projectId/acquire", lockController.Acquire)
			lr.Post("/:projectId/release", lockController.Release)
			lr.Post("/:projectId/heartbeat", lockController.Heartbeat)
			lr.Get("/:projectId/status", lockController.Status)
		})

		messageController := controllers.NewMessageController()
		r.Get("/api/messages/:projectId", messageController.ListByProject)
		r.Get("/api/logs", messageController.ListLogs)

		// 插件系统 API
		r.Get("/api/plugins", plugins.ListPluginsHandler)
		r.Get("/api/projects/:projectId/stats", plugins.ProjectStatsHandler)

		// 日志导出与清理属于管理操作，仅管理员可用。
		// 导播端与管理后台都会读 /api/logs 和 /api/plugins，所以那两个保持
		// 「已登录即可」，只有会改动数据的导出/清理收紧。
		r.Prefix("/api/logs").Middleware(middleware.RequireRole("admin")).Group(func(lr route.Router) {
			lr.Post("/export", plugins.ExportLogsHandler)
			lr.Post("/export/csv", plugins.ExportLogsCSVHandler)
			lr.Post("/cleanup", plugins.CleanupLogsHandler)
		})
	})

	// === 采访端路由（无需JWT） ===
	interviewController := controllers.NewInterviewController()
	facades.Route().Get("/api/interview/:projectId", interviewController.ListByProject)
	facades.Route().Post("/api/interview/status", interviewController.UpdateStatus)

	// === WebSocket + 采访端 Web 托管在独立服务器 (端口 3002) ===
}
