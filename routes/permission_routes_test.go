package routes

import (
	"os"
	"sort"
	"strings"
	"testing"

	"smart-mzcmc/app/http/controllers"
	"smart-mzcmc/app/rbac"
)

// 「哪条路由要哪项权限」是这套改动里最静默的部分：中间件漏挂不会 panic、
// 不会编译错、不会打日志，请求照常通过。现场表现只有两种——
//   - 该拒的没拒：负责人把项目删了、把账号改了，没人拦；
//   - 该放的没放：按钮点了没反应，前端只显示一个 403。
// 所以断言的必须是**路由注册结果本身**，而不是某个纯函数的返回值。
// app/rbac 那边测「策略对不对」，这里测「路由声明对不对」——两者缺一个，
// 权限系统就有一个静默的洞。

// wantPermission 声明一条路由**必须**挂哪些中间件。
//
// 写成全量声明（而不是「必须含有」）是有意的：多挂一道不该有的守卫同样
// 是 bug，而且比少挂更难发现（现场只表现为某个功能点不动，没人会想到
// 「这里其实多了一道门」）。
type wantPermission struct {
	method string
	path   string
	// perms 这条路由要的具名权限。多个时按实际注册顺序排列。
	perms []string
	// projectMember 表示这条路由还要挂项目成员校验。
	//
	// 它与 perms 是**正交**的两件事：perms 回答「这个角色能做什么」，
	// 成员校验回答「能不能看这个项目」。权限迁移没有动后者，也绝不能动：
	// authz.require_project_membership 默认 false（现场要先配齐授权再打开），
	// 一旦顺手改成 true，存量部署会当场连不上。
	projectMember bool
}

// 所有已注册守卫路由的完整清单。任何一条路由的新增、删除、改名、改权限，
// 都要在这里改一行——忘了改，这条用例会先失败，逼着人想清楚它到底该要什么。
var wantPermissions = []wantPermission{
	// ── /api/admin：用户 ────────────────────────────────────────────────
	//
	// 注意 GET /users 要的是 user.view 而不是 user.manage。这是迁移带来的
	// 新增能力：负责人能授权成员、能管采访点，却连被授权的人是谁都看不到。
	{method: "GET", path: "/api/admin/users", perms: []string{rbac.PermUserView}},
	{method: "DELETE", path: "/api/admin/users/:id", perms: []string{rbac.PermUserManage}},
	{method: "PUT", path: "/api/admin/users/:id/role", perms: []string{rbac.PermUserManage}},

	// ── /api/admin：项目与机位 ──────────────────────────────────────────
	//
	// 读列表（GET /projects）与增删改分属两项权限。导播端等项目下拉框就是
	// 靠 GET /projects——它此前挂在 RequireRole(RoleAdmin) 后面，导播的令牌
	// 根本拿不到数据，项目下拉恒定是空的。
	// GET /api/admin/projects 要 project.manage 而不是 project.view：它不过滤，
	// 返回的是全量项目详情。「按成员过滤的那份项目列表」是非管理端的
	// GET /api/projects，两者不是一回事。
	{method: "GET", path: "/api/admin/projects", perms: []string{rbac.PermProjectManage}},
	{method: "POST", path: "/api/admin/projects", perms: []string{rbac.PermProjectManage}},
	{method: "PUT", path: "/api/admin/projects/:id", perms: []string{rbac.PermProjectManage}},
	{method: "DELETE", path: "/api/admin/projects/:id", perms: []string{rbac.PermProjectManage}},
	{method: "POST", path: "/api/admin/projects/:id/cameras", perms: []string{rbac.PermProjectManage}},
	{method: "PUT", path: "/api/admin/projects/:id/cameras/:cameraId", perms: []string{rbac.PermProjectManage}},
	{method: "DELETE", path: "/api/admin/projects/:id/cameras/:cameraId", perms: []string{rbac.PermProjectManage}},

	// ── /api/admin：成员授权 ────────────────────────────────────────────
	//
	// 迁移前这一组是管理员及以上，现在降到负责人——这是本次唯一的**行为降级**，
	// 依据是业务要求（负责人管排期必然要调整谁能上哪个项目）。
	{method: "POST", path: "/api/admin/assign", perms: []string{rbac.PermProjectMember}},
	{method: "POST", path: "/api/admin/revoke", perms: []string{rbac.PermProjectMember}},
	{method: "GET", path: "/api/admin/users/:id/projects", perms: []string{rbac.PermProjectMember}},

	// ── /api/admin：审计 ────────────────────────────────────────────────
	//
	// 审计是「谁能操作这套系统」的记录，与协调日志不是一回事，所以不与
	// /api/logs 共用一项权限。
	{method: "GET", path: "/api/admin/audit-logs", perms: []string{rbac.PermAuditView}},

	// ── /api/system ────────────────────────────────────────────────────
	//
	// 五条接口一项权限，因为它们的能力太整齐（系统信息、运行指标、在线更新），
	// 拆开挂没有意义——真要拆，也是整套一起降级。
	{method: "GET", path: "/api/system/info", perms: []string{rbac.PermSystemMaintain}},
	{method: "GET", path: "/api/system/metrics", perms: []string{rbac.PermSystemMaintain}},
	{method: "GET", path: "/api/system/update", perms: []string{rbac.PermSystemMaintain}},
	{method: "GET", path: "/api/system/update/progress", perms: []string{rbac.PermSystemMaintain}},
	{method: "POST", path: "/api/system/update/apply", perms: []string{rbac.PermSystemMaintain}},

	// ── /api/logs ──────────────────────────────────────────────────────
	//
	// 三项权限拆成三条，而不是「等级及以上」：早期导出与清理都是
	// RequireRole("admin")，等于把「只能删不能导」绑在一起。
	{method: "GET", path: "/api/logs", perms: []string{rbac.PermLogView}},
	{method: "POST", path: "/api/logs/export", perms: []string{rbac.PermLogExport}},
	{method: "POST", path: "/api/logs/export/csv", perms: []string{rbac.PermLogExport}},
	{method: "POST", path: "/api/logs/cleanup", perms: []string{rbac.PermLogCleanup}},

	// ── /api/locks ─────────────────────────────────────────────────────
	//
	// 四条路由里只有三条要 switch.operate。只读的 status 谁都要看——
	// 解说端、包装端、采访端都要显示「现在谁在控制」。
	{method: "GET", path: "/api/locks/:projectId/status", projectMember: true},
	{method: "POST", path: "/api/locks/:projectId/acquire",
		perms: []string{rbac.PermSwitchOperate}, projectMember: true},
	{method: "POST", path: "/api/locks/:projectId/release",
		perms: []string{rbac.PermSwitchOperate}, projectMember: true},
	{method: "POST", path: "/api/locks/:projectId/heartbeat",
		perms: []string{rbac.PermSwitchOperate}, projectMember: true},
}

// collectGuardedRoutes 跑一遍真实注册逻辑，返回全部受守卫路由。
//
// 四个注册函数都在这里调，与 Web() 里调的是同一批。Web() 本身测不了——
// 它直接用 facades.Route()，需要整个应用启动；所以另有
// TestWeb_受守卫的路由必须走注册函数 一条源码级断言兜住「有人绕过注册函数
// 在 Web() 里直接挂路由」这个洞。
func collectGuardedRoutes(t *testing.T) []registeredRoute {
	t.Helper()
	r := newFakeRouter()

	RegisterAdminRoutes(r)
	RegisterSystemRoutes(r)
	RegisterLogRoutes(r, controllers.NewMessageController())
	RegisterLockRoutes(r, controllers.NewLockController())

	got := *r.routes
	sort.Slice(got, func(i, j int) bool {
		if got[i].path != got[j].path {
			return got[i].path < got[j].path
		}
		return got[i].method < got[j].method
	})
	return got
}

// routeKey 是路由的唯一标识。
func routeKey(method, path string) string {
	return method + " " + path
}

// TestPermissionRoutes_每条路由声明的权限与清单一致 是这份测试的主体。
//
// 两个方向都要断：
//   - 清单里有、实际没有 → 守卫漏挂，该拒的没拒。
//   - 实际有、清单里没有 → 有人加了接口忘了想它该要什么权限。少了这一步，
//     新加一条裸路由（不挂任何守卫）会安静地通过所有断言。
func TestPermissionRoutes_每条路由声明的权限与清单一致(t *testing.T) {
	routes := collectGuardedRoutes(t)

	actual := make(map[string]registeredRoute, len(routes))
	for _, r := range routes {
		key := routeKey(r.method, r.path)
		if _, dup := actual[key]; dup {
			t.Errorf("路由 %s 注册了两次", key)
		}
		actual[key] = r
	}

	// 清单 → 实际：少挂守卫会在这里暴露。
	for _, want := range wantPermissions {
		key := routeKey(want.method, want.path)
		got, ok := actual[key]
		if !ok {
			t.Errorf("路由 %s 没有注册（它本该要权限 %v）", key, want.perms)
			continue
		}

		expect := make([]string, 0, len(want.perms)+1)
		if want.projectMember {
			expect = append(expect, "project-member")
		}
		for _, p := range want.perms {
			expect = append(expect, "perm="+p)
		}

		if strings.Join(got.middlewares, ",") != strings.Join(expect, ",") {
			t.Errorf("%s 的守卫：\n  实际 %v\n  期望 %v", key, got.middlewares, expect)
		}
	}

	// 实际 → 清单：多了接口忘了声明权限会在这里暴露。
	if len(actual) != len(wantPermissions) {
		t.Errorf("受守卫的路由共 %d 条，清单里有 %d 条——请在 wantPermissions 里补上",
			len(actual), len(wantPermissions))
	}
	for _, got := range routes {
		key := routeKey(got.method, got.path)
		found := false
		for _, want := range wantPermissions {
			if routeKey(want.method, want.path) == key {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("路由 %s 没写进 wantPermissions——它现在挂着 %v，"+
				"新增路由时必须想清楚它该要哪项权限（或者确实不需要）", key, got.middlewares)
		}
	}
}

// TestPermissionRoutes_权限名都是已声明的 防的是「路由上打错了一个字」。
//
// 拼错的权限名在运行时会被 Can 拒绝（fail-closed），但那个表现是
// 「所有人都进不来，包括超管」——现场表现成整套系统坏了，而不是
// 「某条路由的守卫挂错了」。所以要在这里挡住。
func TestPermissionRoutes_权限名都是已声明的(t *testing.T) {
	for _, r := range collectGuardedRoutes(t) {
		for _, name := range r.middlewares {
			if !strings.HasPrefix(name, "perm=") {
				continue
			}
			perm := strings.TrimPrefix(name, "perm=")
			if !rbac.Known(perm) {
				t.Errorf("%s %s 挂了一个不存在的权限 %q", r.method, r.path, perm)
			}
		}
	}
}

// TestPermissionRoutes_不再有等级门槛 防的是有人又挂回 RequireRole。
//
// 等级门槛表达不了「负责人能看、不能改」，也表达不了「导播能抢锁、
// 负责人不能」。只要还有一条路由挂着它，「权限迁移已完成」就是假的——
// 而且这两种机制混用时，出现的是「后勤等级高于导播于是能删掉导播账号」
// 那类漏洞（等级高低与能否操作他人本就是两件事）。
func TestPermissionRoutes_不再有等级门槛(t *testing.T) {
	for _, r := range collectGuardedRoutes(t) {
		for _, name := range r.middlewares {
			if strings.HasPrefix(name, "role>=") {
				t.Errorf("%s %s 仍挂着等级门槛 %q——接口准入一律用 app/rbac 的具名权限",
					r.method, r.path, name)
			}
		}
	}
}

// TestPermissionRoutes_成员校验没有被权限守卫顶替 是 REQUIRE_PROJECT_MEMBERSHIP
// 的回归防线。
//
// 权限迁移最顺手的一个坏动作是「既然有了权限系统，把成员校验换成一项权限
// 得了」——那会让 authz.require_project_membership 这个开关彻底失效。
// 这里断言的是：需要成员校验的那几条路由仍然挂着 project-member。
//
// ⚠️ 开关本身仍默认 false（见 config/authz.go），而中间件在关闭时只记
// 日志不拦截，那是刻意的现场过渡设计，本次迁移没有动它。
func TestPermissionRoutes_成员校验没有被权限守卫顶替(t *testing.T) {
	routes := collectGuardedRoutes(t)

	// 这四条是带 projectId 的只读接口，它们要的是「能不能看这个项目」。
	// 它们的准入完全由成员校验决定，不要给它们补一项权限——
	// 补了等于让「角色」参与项目级授权，那是两件事。
	memberOnly := []string{
		"/api/locks/:projectId/status",
		"/api/locks/:projectId/acquire",
		"/api/locks/:projectId/release",
		"/api/locks/:projectId/heartbeat",
	}
	for _, path := range memberOnly {
		r, ok := findRoute(routes, methodOf(wantPermissions, path), path)
		if !ok {
			t.Errorf("路由 %s 没有注册", path)
			continue
		}
		assertHasMiddleware(t, r, "project-member")
	}
}

// methodOf 从清单里查一条路由的方法，省得测试里把 GET/POST 写错。
func methodOf(list []wantPermission, path string) string {
	for _, w := range list {
		if w.path == path {
			return w.method
		}
	}
	return ""
}

// TestWeb_受守卫的路由必须走注册函数 堵住「绕过注册函数直接挂路由」。
//
// 上面那几条用例断言的是注册函数的结果。如果有人图省事，在 Web() 里
// 直接写 `facades.Route().Get("/api/admin/...")`，那条路由既不进
// fakeRouter 的记录，也不进 wantPermissions——所有断言照样全绿，而线上
// 少了一道门。
//
// Web() 需要 facades.Route()，起不来，所以只能做源码级检查：Web() 函数体里
// 不允许出现这四个前缀的路径字面量。
func TestWeb_受守卫的路由必须走注册函数(t *testing.T) {
	src, err := os.ReadFile("web.go")
	if err != nil {
		t.Fatalf("读 web.go 失败：%v", err)
	}
	text := string(src)

	const startMarker = "func Web() {"
	start := strings.Index(text, startMarker)
	if start < 0 {
		t.Fatalf("web.go 里找不到 %s——这个用例靠它在 Web() 与注册函数之间切一刀，"+
			"文件结构变了就得跟着改", startMarker)
	}
	// 注册函数定义在 Web() 之后，取到第一个注册函数的注释为止。
	const endMarker = "// RegisterAdminRoutes"
	end := strings.Index(text, endMarker)
	if end < 0 || end < start {
		t.Fatalf("web.go 里找不到 %s（或它排在 Web() 之前）——"+
			"这个用例依赖「Web() 在前、注册函数在后」的布局", endMarker)
	}
	webBody := text[start:end]

	for _, prefix := range []string{"/api/admin", "/api/system", "/api/logs", "/api/locks"} {
		if strings.Contains(webBody, `"`+prefix) {
			t.Errorf("Web() 里出现了受守卫的路径 %q。"+
				"这些路由必须走 RegisterAdminRoutes / RegisterSystemRoutes / "+
				"RegisterLogRoutes / RegisterLockRoutes，否则既进不了 wantPermissions 的断言，"+
				"也没有任何人会想起来给它挂权限", prefix)
		}
	}
}
