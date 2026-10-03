package routes

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	contractshttp "github.com/goravel/framework/contracts/http"
	contractsroute "github.com/goravel/framework/contracts/route"

	"smart-mzcmc/app/http/controllers"
	"smart-mzcmc/app/rbac"
)

// 切台守卫挂没挂、挂在哪条路由上，是这套改动里唯一「错了也不会报错」的部分：
// 中间件漏挂不会 panic，只会让现场表现为「点了没反应」；错挂则表现为
// 「负责人把切台锁抢走了」。所以这里断言的是路由注册结果本身，而不是某个
// 纯函数的返回值——纯函数测得再全，中间件没挂上去也是白搭。
//
// permission_routes_test.go 里还有一个更完整的版本：它把**所有**已注册
// 路由的权限声明都钉住，而这里只管切台这一组。

// registeredRoute 是假路由器记录下来的一条路由。
type registeredRoute struct {
	method      string
	path        string
	middlewares []string
}

// fakeRouter 手工实现 contractsroute.Router，只记录、不派发。
//
// 为什么不用框架自带的 mock：那套 mock 依赖 testify，而本仓库的测试一律
// 用标准库（AGENTS.md 的约定）。这里用不到的动词直接 panic，
// 一旦 RegisterLockRoutes 将来用上了别的动词，测试会立刻暴露出来。
type fakeRouter struct {
	prefix      string
	middlewares []string
	routes      *[]registeredRoute
}

func newFakeRouter() *fakeRouter {
	return &fakeRouter{routes: &[]registeredRoute{}}
}

func (f *fakeRouter) child(prefix string, middlewares []string) *fakeRouter {
	return &fakeRouter{prefix: prefix, middlewares: middlewares, routes: f.routes}
}

func (f *fakeRouter) Group(handler contractsroute.GroupFunc) {
	handler(f.child(f.prefix, f.middlewares))
}

func (f *fakeRouter) Prefix(path string) contractsroute.Router {
	return f.child(f.prefix+path, f.middlewares)
}

func (f *fakeRouter) Middleware(middlewares ...contractshttp.Middleware) contractsroute.Router {
	names := make([]string, 0, len(f.middlewares)+len(middlewares))
	names = append(names, f.middlewares...)
	for _, m := range middlewares {
		names = append(names, m.Signature())
	}
	return f.child(f.prefix, names)
}

func (f *fakeRouter) WithoutMiddleware(middlewares ...contractshttp.Middleware) contractsroute.Router {
	return f.child(f.prefix, f.middlewares)
}

func (f *fakeRouter) record(method, path string) {
	*f.routes = append(*f.routes, registeredRoute{
		method:      method,
		path:        f.prefix + path,
		middlewares: append([]string(nil), f.middlewares...),
	})
}

func (f *fakeRouter) Get(path string, _ contractshttp.HandlerFunc) contractsroute.Action {
	f.record("GET", path)
	return nil
}

func (f *fakeRouter) Post(path string, _ contractshttp.HandlerFunc) contractsroute.Action {
	f.record("POST", path)
	return nil
}

func (f *fakeRouter) Any(path string, _ contractshttp.HandlerFunc) contractsroute.Action {
	f.record("ANY", path)
	return nil
}

func (f *fakeRouter) Delete(path string, _ contractshttp.HandlerFunc) contractsroute.Action {
	f.record("DELETE", path)
	return nil
}

func (f *fakeRouter) Patch(path string, _ contractshttp.HandlerFunc) contractsroute.Action {
	f.record("PATCH", path)
	return nil
}

func (f *fakeRouter) Put(path string, _ contractshttp.HandlerFunc) contractsroute.Action {
	f.record("PUT", path)
	return nil
}

func (f *fakeRouter) Options(path string, _ contractshttp.HandlerFunc) contractsroute.Action {
	f.record("OPTIONS", path)
	return nil
}

func (f *fakeRouter) Resource(_ string, _ contractshttp.ResourceController) contractsroute.Action {
	panic("切台路由不应注册资源路由")
}

func (f *fakeRouter) Static(_, _ string) contractsroute.Action {
	panic("切台路由不应有静态资源")
}
func (f *fakeRouter) StaticFile(_, _ string) contractsroute.Action {
	panic("切台路由不应有静态文件")
}
func (f *fakeRouter) StaticFS(_ string, _ http.FileSystem) contractsroute.Action {
	panic("切台路由不应有静态目录")
}

func findRoute(routes []registeredRoute, method, path string) (registeredRoute, bool) {
	for _, r := range routes {
		if r.method == method && r.path == path {
			return r, true
		}
	}
	return registeredRoute{}, false
}

// 声明 fakeRouter 满足接口：一旦框架升级改了 Router 的方法集，
// 这里会编译失败，而不是等到运行时的类型断言失败。
var _ contractsroute.Router = (*fakeRouter)(nil)

// collectLockRoutes 跑一遍真实注册逻辑，返回记录下来的路由。
func collectLockRoutes(t *testing.T) []registeredRoute {
	t.Helper()
	r := newFakeRouter()
	RegisterLockRoutes(r, controllers.NewLockController())
	got := *r.routes
	sort.Slice(got, func(i, j int) bool { return got[i].path < got[j].path })
	return got
}

// TestLockRoutes_改动切台状态的接口必须挂切台权限 守住正向的三条。
//
// 漏挂的效果是静默的：中间件不存在时请求照常通过，只是负责人也能抢锁。
//
// 守卫名从 switching-operator 变成了 perm=switch.operate（具名权限）。
// 名字变了不是小事：中间件换了而路由没跟着换的话，这里会直接红——
// 那是本次迁移里唯一一处「换了实现、必须同步改挂载点」的地方。
func TestLockRoutes_改动切台状态的接口必须挂切台权限(t *testing.T) {
	routes := collectLockRoutes(t)

	for _, path := range []string{
		"/api/locks/:projectId/acquire",
		"/api/locks/:projectId/release",
		"/api/locks/:projectId/heartbeat",
	} {
		r, ok := findRoute(routes, "POST", path)
		if !ok {
			t.Fatalf("路由 %s 没有注册", path)
		}
		assertHasMiddleware(t, r, "project-member")
		assertHasMiddleware(t, r, "perm="+rbac.PermSwitchOperate)
	}
}

// TestLockRoutes_只读状态接口不挂切台权限 防的是「顺手把 status 也挂上了」。
//
// 挂错了不会有人来抱怨——现场只是看不到现在谁在控制，而那是所有人都关心
// 的事（解说端、包装端、采访端都要显示控制状态）。
func TestLockRoutes_只读状态接口不挂切台权限(t *testing.T) {
	routes := collectLockRoutes(t)

	r, ok := findRoute(routes, "GET", "/api/locks/:projectId/status")
	if !ok {
		t.Fatal("GET /api/locks/:projectId/status 没有注册")
	}
	assertHasMiddleware(t, r, "project-member")
	if hasMiddleware(r, "perm="+rbac.PermSwitchOperate) {
		t.Error("只读状态接口不应挂切台权限：项目成员都要能看到当前控制状态")
	}
}

// TestLockRoutes_成员校验必须排在切台权限之前 防的是顺序颠倒。
//
// 顺序错了会把「不是项目成员」报成「角色不对」——两回事，排查方向完全不同：
// 前者要去配项目授权，后者只能换账号。
func TestLockRoutes_成员校验必须排在切台权限之前(t *testing.T) {
	routes := collectLockRoutes(t)

	r, ok := findRoute(routes, "POST", "/api/locks/:projectId/acquire")
	if !ok {
		t.Fatal("POST /api/locks/:projectId/acquire 没有注册")
	}
	memberAt, operatorAt := -1, -1
	for i, name := range r.middlewares {
		switch name {
		case "project-member":
			memberAt = i
		case "perm=" + rbac.PermSwitchOperate:
			operatorAt = i
		}
	}
	if memberAt < 0 || operatorAt < 0 {
		t.Fatalf("acquire 应同时经过成员校验与切台权限，实际 %v", r.middlewares)
	}
	if memberAt > operatorAt {
		t.Errorf("成员校验应排在切台权限之前，实际 %v", r.middlewares)
	}
}

// TestLockRoutes_没有意料之外的切台接口 防的是「顺手又加了一条会改状态的
// 接口，却忘了挂守卫」。新增路由时这条会先失败，逼着人想清楚它该不该挂。
func TestLockRoutes_没有意料之外的切台接口(t *testing.T) {
	routes := collectLockRoutes(t)

	want := []string{
		"/api/locks/:projectId/acquire",
		"/api/locks/:projectId/heartbeat",
		"/api/locks/:projectId/release",
		"/api/locks/:projectId/status",
	}
	got := make([]string, 0, len(routes))
	for _, r := range routes {
		got = append(got, r.path)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("切台路由集合变了：\n  实际 %v\n  期望 %v", got, want)
	}
}

func assertHasMiddleware(t *testing.T, r registeredRoute, signature string) {
	t.Helper()
	if !hasMiddleware(r, signature) {
		t.Errorf("%s %s 缺少中间件 %s（实际 %v）", r.method, r.path, signature, r.middlewares)
	}
}

func hasMiddleware(r registeredRoute, signature string) bool {
	for _, name := range r.middlewares {
		if name == signature {
			return true
		}
	}
	return false
}
