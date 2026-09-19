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
	facades.Route().Get("/", func(ctx http.Context) http.Response {
		return ctx.Response().File("./public/index.html")
	})

	// 管理后台页面
	facades.Route().Get("/admin", func(ctx http.Context) http.Response {
		return ctx.Response().File("./public/admin/index.html")
	})
	facades.Route().Get("/admin/", func(ctx http.Context) http.Response {
		return ctx.Response().File("./public/admin/index.html")
	})

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
		r.Prefix("/api/admin").Group(func(ar route.Router) {
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
		r.Post("/api/logs/export", plugins.ExportLogsHandler)
		r.Post("/api/logs/export/csv", plugins.ExportLogsCSVHandler)
		r.Post("/api/logs/cleanup", plugins.CleanupLogsHandler)
	})

	// === 采访端路由（无需JWT） ===
	interviewController := controllers.NewInterviewController()
	facades.Route().Get("/api/interview/:projectId", interviewController.ListByProject)
	facades.Route().Post("/api/interview/status", interviewController.UpdateStatus)

	// === WebSocket + 采访端 Web 托管在独立服务器 (端口 3002) ===
}
