package routes

import (
	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/contracts/route"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/http/controllers"
	"smart-mzcmc/app/http/middleware"
	"smart-mzcmc/app/models"
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

	// 状态检查（公开）。管理后台侧边栏显示版本号用，字段保持不变以兼容旧前端。
	statusController := controllers.NewStatusController()
	facades.Route().Get("/api/status", statusController.ServerStatus)

	// 健康检查（公开）。给监控 / 反向代理探活用，只回答「能不能用」，
	// 不泄露任何部署信息。
	healthController := controllers.NewHealthController()
	facades.Route().Get("/api/health", healthController.Health)

	// === 初始化向导（公开） ===
	//
	// 全新部署时数据库不存在，除这两个接口外的 API 都由 SetupGate 中间件
	// 拦成 503 并把人引导到 /admin/setup。接口本身必须公开：此刻系统里
	// 一个账号都没有，无从登录。
	setupController := controllers.NewSetupController()
	facades.Route().Get("/api/setup/status", setupController.Status)
	facades.Route().Post("/api/setup/apply", setupController.Apply)

	// === 认证路由（公开） ===
	authController := controllers.NewAuthController()
	facades.Route().Post("/api/auth/login", authController.Login)
	facades.Route().Post("/api/auth/register", authController.Register)
	// 管理后台登录页据此在「登录」与「创建首个管理员」之间切换。
	facades.Route().Get("/api/auth/bootstrap", authController.BootstrapStatus)

	// === 需要认证的路由 ===
	facades.Route().Middleware(middleware.Jwt()).Group(func(r route.Router) {
		r.Get("/api/auth/profile", authController.Profile)

		// 个人资料：任何人只能改自己，所以不挂角色门槛。
		// 判定依据是「只操作 ctx 里的那个 id」，不存在越权空间。
		r.Put("/api/auth/profile", authController.UpdateProfile)
		r.Put("/api/auth/password", authController.ChangePassword)

		// 角色清单对任意登录用户开放。管理前端据此渲染角色标签与下拉选项，
		// 而不是在前端再写一份映射——之前 AppShell、用户页、权限分配页各有一处
		// admin ? '管理员' : '导播' 的三元表达式，加超级管理员后全部会漏改，
		// 把超管显示成「导播」。
		roleController := controllers.NewRoleController()
		r.Get("/api/roles", roleController.List)

		adminController := controllers.NewAdminController()
		// 管理接口除 JWT 外还要校验角色：Jwt 中间件只验证令牌有效，
		// 不关心持有者身份。少了这一层，任何登录用户（包括最低权限的导播）
		// 都能列出全部用户与项目、增删项目、分配权限。
		//
		// 守卫是「管理员及以上」而非「角色等于管理员」，所以超级管理员自动获得
		// 这里全部接口的访问权，不需要额外在名字列表里补一遍。
		r.Prefix("/api/admin").Middleware(middleware.RequireRole(models.RoleAdmin)).Group(func(ar route.Router) {
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

		// 系统维护：只有超级管理员。系统更新会替换服务自身的可执行文件并重启进程，
		// 任何一次误操作都会影响全系统所有客户端，所以不与普通管理权限同级。
		systemController := controllers.NewSystemController()
		r.Prefix("/api/system").Middleware(middleware.RequireRole(models.RoleSuperAdmin)).Group(func(sr route.Router) {
			sr.Get("/info", systemController.Info)
			sr.Get("/update", systemController.UpdateStatus)
			sr.Post("/update/apply", systemController.ApplyUpdate)
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

		// 日志读取对所有登录用户开放（导播端与管理后台都要看），
		// 下面把「只读」和「会改动数据」拆成两个等级：
		//
		//   导出 —— 负责人及以上。业务侧要看报表导出，这是导播与后勤用不到的。
		//   清理 —— 管理员及以上。清理会真的删数据，负责人没有理由做这件事。
		//
		// 早期两者都是 RequireRole("admin")，等于把「只能删不能导」绑在一起，
		// 让不需要删除权限的人也被迫持有删除权限。
		r.Prefix("/api/logs").Middleware(middleware.RequireRole(models.RoleLeader)).Group(func(lr route.Router) {
			lr.Post("/export", plugins.ExportLogsHandler)
			lr.Post("/export/csv", plugins.ExportLogsCSVHandler)
		})
		r.Prefix("/api/logs").Middleware(middleware.RequireRole(models.RoleAdmin)).Group(func(lr route.Router) {
			lr.Post("/cleanup", plugins.CleanupLogsHandler)
		})
	})

	// === 采访端路由（无需JWT） ===
	interviewController := controllers.NewInterviewController()
	facades.Route().Get("/api/interview/:projectId", interviewController.ListByProject)
	facades.Route().Post("/api/interview/status", interviewController.UpdateStatus)

	// === WebSocket + 采访端 Web 托管在独立服务器 (端口 3002) ===
}
