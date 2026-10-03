package routes

import (
	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/contracts/route"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/http/controllers"
	"smart-mzcmc/app/http/middleware"
	"smart-mzcmc/app/plugins"
	"smart-mzcmc/app/rbac"
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
	// 管理后台网页专用的登录入口：与上面同一个校验流程，但额外要求角色达到
	// authz.admin_min_role。原生客户端继续用 /api/auth/login，不受此限制。
	facades.Route().Post("/api/auth/admin-login", authController.AdminLogin)
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

		// 当前账号的生效权限清单。给管理后台按**权限名**渲染按钮用——
		// 前端此前是按角色名猜的，与 app/rbac 的权限名没有任何映射关系，
		// 权限迁移后必然不同步（负责人要么看不到入口，要么看到点了 403）。
		// 门槛是「登录即可」：它只回答「我自己能干什么」。
		r.Get("/api/auth/permissions", authController.Permissions)

		// 管理接口除 JWT 外还要校验权限：Jwt 中间件只验证令牌有效，
		// 不关心持有者身份。少了这一层，任何登录用户（包括最低权限的导播）
		// 都能列出全部用户与项目、增删项目、分配权限。
		//
		// 守卫是「具名权限」而不是「等级门槛」：Jwt + RequirePermission。
		// 改用等级（RequireRole）表达不了「负责人能看、不能改」——他 40 级
		// 比导播 30 还高，任何 <= 40 的门槛都放他进来，而业务上负责人只读。
		RegisterAdminRoutes(r)

		// 系统维护。系统更新会替换服务自身的可执行文件并重启进程，
		// 任何一次误操作都会影响全系统所有客户端，所以 system.maintain
		// 在策略里只有超管一行，不与任何普通管理权限同级。
		RegisterSystemRoutes(r)

		// 在线权限编辑。用 system.maintain 当门槛的理由见 RegisterRbacRoutes。
		RegisterRbacRoutes(r)

		lockController := controllers.NewLockController()
		RegisterLockRoutes(r, lockController)

		messageController := controllers.NewMessageController()
		RegisterLogRoutes(r, messageController)

		RegisterProjectRoutes(r, controllers.NewProjectController(), messageController)

		// 插件系统 API
		r.Get("/api/plugins", plugins.ListPluginsHandler)
	})

	// === 采访端路由（无需JWT） ===
	interviewController := controllers.NewInterviewController()
	facades.Route().Get("/api/interview/:projectId", interviewController.ListByProject)
	facades.Route().Post("/api/interview/status", interviewController.UpdateStatus)

	// === WebSocket + 采访端 Web 托管在独立服务器 (端口 3002) ===
}

// RegisterAdminRoutes 挂载 /api/admin 下的全部路由。
//
// 单独抽出来有两个原因：
//  1. 「哪条路由要哪项权限」这件事能被直接断言（permission_routes_test.go）。
//     把守卫漏挂或错挂到一个接口上是**静默失效**：中间件不存在时请求照常
//     通过，现场表现为「点了没反应」或「负责人把项目删了」。
//  2. 权限与路由的对应关系一屏看得完。迁移前这里是一整组
//     RequireRole(RoleAdmin)，把 13 条权限面完全不同的接口捆在一起。
//
// 每个 Prefix 对应一项具名权限。分开的唯一理由是「同一组前缀下不同路由
// 需要不同权限」，中间件必须挂在各自的 Group 上。
//
// 调用方（Web）已经把整组挂在 Jwt() 之后。
func RegisterAdminRoutes(r route.Router) {
	adminController := controllers.NewAdminController()

	// 查用户列表。**这是权限迁移带来的新增能力**：迁移前它和增删改角色挤在
	// 同一个 RequireRole(RoleAdmin) 组里，于是负责人能授权成员、能管采访点，
	// 却连被授权的人是谁都看不到（点开是 403）。
	r.Prefix("/api/admin").Middleware(middleware.RequirePermission(rbac.PermUserView)).
		Group(func(ar route.Router) {
			ar.Get("/users", adminController.ListUsers)
		})

	// 增删账号、改角色。
	//
	// 注意这道门只回答「能不能进这个接口」。「能不能操作**这个人**」由控制器
	// 里的 decideRoleChange / decideDeleteUser 判断（不能操作同级或更高、
	// 不能自降权、不能动最后一个超管）——那是两件事。这里换成具名权限
	// 之后那套判断**原样保留**：把它一起换成「权限够就行」的话，
	// 第一个管理员登录就能给自己升成超管。
	r.Prefix("/api/admin").Middleware(middleware.RequirePermission(rbac.PermUserManage)).
		Group(func(ar route.Router) {
			ar.Delete("/users/:id", adminController.DeleteUser)
			ar.Put("/users/:id/role", adminController.UpdateUserRole)
		})

	// 查项目列表。
	//
	// 这里要的是 project.manage 而不是 project.view，尽管它只读。原因是它
	// **完全不过滤**：AdminController.ListProjects 直接 Find 全部项目，把描述、
	// 场馆、排期、负责人、状态一并返回。而非管理端那条 GET /api/projects
	// （见 Web() 里那条注释）是**按 user_projects 过滤**的——非管理员只拿得到
	// 授权给他的那些。
	//
	// 所以给全员 project.view 等于开了一个绕过过滤的后门：后勤、导播、包装、
	// 解说全都能拉到全量项目详情。导播的项目下拉靠的是非管理端那条，这个
	// 接口只有管理后台在用，降到 project.manage 不损失任何人。
	r.Prefix("/api/admin").Middleware(middleware.RequirePermission(rbac.PermProjectManage)).
		Group(func(ar route.Router) {
			ar.Get("/projects", adminController.ListProjects)
		})

	// 建/改/删项目与机位预设。
	//
	// 机位预设的**读**接口在 /api/projects/:projectId/cameras（非管理端也要
	// 读），不在这一组里。
	r.Prefix("/api/admin").Middleware(middleware.RequirePermission(rbac.PermProjectManage)).
		Group(func(ar route.Router) {
			ar.Post("/projects", adminController.CreateProject)
			ar.Put("/projects/:id", adminController.UpdateProject)
			ar.Delete("/projects/:id", adminController.DeleteProject)
			ar.Post("/projects/:id/cameras", adminController.CreateCamera)
			ar.Put("/projects/:id/cameras/:cameraId", adminController.UpdateCamera)
			ar.Delete("/projects/:id/cameras/:cameraId", adminController.DeleteCamera)
		})

	// 授权/回收项目成员。**这是权限迁移带来的降级**：迁移前是管理员及以上，
	// 现在负责人也能。依据是业务要求——负责人管排期必然要调整谁能上哪个
	// 项目；而 authz.require_project_membership 一旦打开，user_projects 就
	// 真的决定谁能读哪个项目，只让管理员改的话负责人得天天找人代劳。
	r.Prefix("/api/admin").Middleware(middleware.RequirePermission(rbac.PermProjectMember)).
		Group(func(ar route.Router) {
			ar.Post("/assign", adminController.AssignProject)
			ar.Post("/revoke", adminController.RevokeProject)
			ar.Get("/users/:id/projects", adminController.ListUserProjects)
		})

	// 操作审计。它是「谁能操作这套系统」的记录，与协调日志不是一回事，
	// 所以干脆放进 /api/admin 组，而不是和 /api/logs 共用一组守卫。
	auditController := controllers.NewAuditController()
	r.Prefix("/api/admin").Middleware(middleware.RequirePermission(rbac.PermAuditView)).
		Group(func(ar route.Router) {
			ar.Get("/audit-logs", auditController.List)
		})
}

// RegisterProjectRoutes 挂载 /api/projects 与 /api/messages 下的项目视图接口。
//
// **两道守卫的职责严格分开，不要合并**：
//
//   - project.view（RequirePermission）回答「这个角色能不能看项目」。
//   - ProjectMemberMiddleware 回答「这个人能不能看**这一个**项目」。
//
// 它们是正交的两件事。合成一件会同时坏掉两侧：只留 project.view 等于让
// 任何登录用户拉到全部项目（成员过滤形同虚设）；只留成员校验则等于
// project.view 这项权限没有真正的门——它声明了 8 个角色全持有，却没有任何
// 路由要求它，于是「某项权限没有任何路由使用」这件事永远不会被发现。
//
// 调用方（Web）已经把整组挂在 Jwt() 之后。
func RegisterProjectRoutes(r route.Router, projectController *controllers.ProjectController, messageController *controllers.MessageController) {
	// 项目列表（按 user_projects 过滤）。挂 project.view：它过去是条
	// 「声明了但没有任何路由使用」的死条目——policy.csv 里 8 个角色全持有，
	// 而后端从没问过它，于是「这项权限可不可以放开」这个问题无处验证。
	// 现在它是真的门：将来若有人想让某个角色看不到项目列表，改策略即可，
	// 不必去改路由。
	r.Middleware(middleware.RequirePermission(rbac.PermProjectView)).
		Get("/api/projects", projectController.List)

	// 下面这几条都带 projectId，准入**只**由项目成员校验决定，不挂 project.view。
	// 之前它们从 URL 取 projectId 就直接查库，任何登录用户猜到编号
	// 就能读任意项目的消息、统计与切台记录。
	//
	// 为什么不给它们补 project.view：这几条回答的是「能不能看这个项目」，
	// 而 project.view 回答「这个角色能不能看项目」。给它们补上等于让角色
	// 参与项目级授权，是两件事被混在一起——而成员校验才是更严的那一道，
	// 补 project.view 不会让它更严，只会让「成员校验生效了吗」这个问题
	// 从路由声明上再也看不出来。
	//
	// 开关 authz.require_project_membership 仍然默认 false（现场要先配齐
	// 授权再打开），这一点没有因为权限迁移而改变。
	r.Prefix("/api/messages").Middleware(middleware.RequireProjectMember()).
		Group(func(mr route.Router) {
			mr.Get("/:projectId", messageController.ListByProject)
		})

	r.Prefix("/api/projects").Middleware(middleware.RequireProjectMember()).
		Group(func(pr route.Router) {
			pr.Get("/:projectId/cameras", projectController.Cameras)
			pr.Get("/:projectId/shot-cuts", projectController.ShotCuts)
			pr.Get("/:projectId/stats", plugins.ProjectStatsHandler)
		})
}

// RegisterSystemRoutes 挂载 /api/system 下的全部路由。
//
// 一组只有一项权限 system.maintain，因为这一组接口的能力太整齐（系统信息、
// 运行指标、在线更新），拆开挂没有意义——真要拆，也是整套一起降级。
func RegisterSystemRoutes(r route.Router) {
	systemController := controllers.NewSystemController()
	r.Prefix("/api/system").Middleware(middleware.RequirePermission(rbac.PermSystemMaintain)).
		Group(func(sr route.Router) {
			sr.Get("/info", systemController.Info)
			// 运行指标（内存 / 磁盘 / 组件健康），供管理后台的监控页轮询。
			// 与 /info 分开是因为轮询频率高得多：内存曲线要连续看，而运行环境
			// 一次进来看一眼就够，混在一个接口里会把不必要的字段每 10 秒传一次。
			sr.Get("/metrics", systemController.Metrics)
			sr.Get("/update", systemController.UpdateStatus)
			// 进度查询。更新跑在后台 goroutine 里，这个接口是它唯一的观察窗口。
			sr.Get("/update/progress", systemController.UpdateProgress)
			sr.Post("/update/apply", systemController.ApplyUpdate)
		})
}

// RegisterRbacRoutes 挂载 /api/rbac 下的在线权限编辑接口。
//
// ## 为什么门槛是 system.maintain
//
// 它恰好是「不可撤销、只能授予受保护角色」的那一项（app/rbac/protect.go 的
// protectedPermissions）。用一条受保护权限去守权限编辑入口，本身就构成一个
// 闭环：将来即使策略被改坏，也不会出现「谁能改权限」这个问题无解——
// 因为「能改权限的人」那一项永远撤不掉。
//
// 它已经是超管独占（policy.csv 里只有一行），所以不需要新增权限项。
// 换成一项普通权限（比如 user.manage）的话，管理员能给自己开这项权限，
// 于是「谁能改权限」就成了一个可自举的东西：先给自己授权，再改别人的。
//
// ⚠️ 这里的守卫只回答「能不能进这个接口」。改动本身还要过
// app/rbac.ApplyRolePermissions 里的三条 Validate*（受保护权限不可撤销、
// 不可授予非受保护角色、受保护角色的任何改动都拒）。中间件与写入校验是
// 叠加的两层，少掉后者就等于「进得来就改得动」。
//
// 调用方（Web）已经把整组挂在 Jwt() 之后。
func RegisterRbacRoutes(r route.Router) {
	rbacController := controllers.NewRbacController()
	r.Prefix("/api/rbac").Middleware(middleware.RequirePermission(rbac.PermSystemMaintain)).
		Group(func(rr route.Router) {
			// 读：当前完整矩阵 + 保护状态 + 告警。
			// 写之前界面必须先拿它，否则无从知道哪些格子是不可点的。
			rr.Get("/policy", rbacController.ShowPolicy)
			// 写：把某个角色的权限集合整体改成请求里给的那一组。
			//
			// 语义是「替换」而不是「增删」——界面渲染的是一整张勾选表，
			// 提交的就是全量。用追加语义的话取消勾选永远传不上去。
			rr.Put("/roles/:role/permissions", rbacController.UpdateRolePermissions)
		})
}

// RegisterLogRoutes 挂载协调日志的读取、导出与清理。
//
// 三项权限而不是一个等级，理由和迁移前那条注释一样，只是现在用名字说了：
//
//	读 —— log.view，全员。导播端与管理后台都要看。
//	导出 —— log.export，负责人及以上。业务侧要看报表导出，导播与后勤用不到。
//	清理 —— log.cleanup，管理员及以上。清理会真的删数据，负责人没理由做。
//
// 早期导出与清理都是 RequireRole("admin")，等于把「只能删不能导」绑在一起，
// 让不需要删除权限的人也被迫持有删除权限。
//
// 调用方（Web）已经把整组挂在 Jwt() 之后。
func RegisterLogRoutes(r route.Router, messageController *controllers.MessageController) {
	r.Middleware(middleware.RequirePermission(rbac.PermLogView)).
		Get("/api/logs", messageController.ListLogs)

	r.Prefix("/api/logs").Middleware(middleware.RequirePermission(rbac.PermLogExport)).
		Group(func(lr route.Router) {
			lr.Post("/export", plugins.ExportLogsHandler)
			lr.Post("/export/csv", plugins.ExportLogsCSVHandler)
		})

	r.Prefix("/api/logs").Middleware(middleware.RequirePermission(rbac.PermLogCleanup)).
		Group(func(lr route.Router) {
			lr.Post("/cleanup", plugins.CleanupLogsHandler)
		})
}

// RegisterLockRoutes 挂载切台相关路由。
//
// 单独抽出来是为了让「哪些接口带切台守卫」这件事能被直接断言。见
// lock_routes_test.go：把守卫漏挂或错挂到一个接口上是静默失效，
// 单测里看不出来，现场表现为「点了没反应」或「负责人把锁抢走了」。
//
// 调用方（Web）已经把整组挂在 Jwt() 之后。
func RegisterLockRoutes(r route.Router, lockController *controllers.LockController) {
	// 项目成员校验挂在这一组上。之前这里的 projectId 直接取自 URL，
	// 任何登录用户只要猜到编号就能抢任意项目的控制权。
	//
	// 开关 REQUIRE_PROJECT_MEMBERSHIP 默认 false，中间件此时只记日志不拦截，
	// 存量部署不受影响（见 app/http/middleware/project.go）。
	//
	// 这一组内部还要再分：会改变切台状态的三个接口额外要求 switch.operate。
	// 成员校验挡不住负责人——他在业务上常常是项目成员，但按业务要求不参与
	// 导播工作。只读的状态查询保持所有成员可看。
	r.Prefix("/api/locks").Middleware(middleware.RequireProjectMember()).Group(func(lr route.Router) {
		// 只读：谁在控制是所有人都关心的事，不设角色门槛。
		lr.Get("/:projectId/status", lockController.Status)

		// 中间件按注册顺序执行，所以顺序是：Jwt（外层）→ 成员 → switch.operate。
		switching := lr.Middleware(middleware.RequirePermission(rbac.PermSwitchOperate))
		switching.Post("/:projectId/acquire", lockController.Acquire)
		switching.Post("/:projectId/release", lockController.Release)
		switching.Post("/:projectId/heartbeat", lockController.Heartbeat)
	})
}
