package middleware

import (
	"context"
	"strings"
	"testing"
	"time"

	contractshttp "github.com/goravel/framework/contracts/http"

	"smart-mzcmc/app/rbac"
)

// 这份文件只测**不需要连数据库**就能验证的那一半：中间件在拿到身份之前的
// 几道守卫，以及它对外声明的名字。
//
// 为什么只测这一半：RequirePermission 的主干是「查 users 表拿到 role，
// 再问 rbac.Can」。为了让这一段可测而去建库建表不划算（本仓库也没有这样的
// 测试脚手架：tests/test_case.go 那套要 Boot 整个应用，会在仓库里留下一个
// 真实的 database/smart-mzcmc.db）。「查库拿角色」那一段与 jwt.go 里
// RoleMiddleware 的做法逐字相同，风险不在那里。
//
// 真正需要重点覆盖的两件事都在这里：
//  1. **没有身份就一律拒绝**（fail-closed）。
//  2. **中间件对外的名字**——routes 包的挂载测试靠 Signature() 字符串来断言
//     「这条路由要哪项权限」，名字一旦变了而路由没跟着变，测试会安静地失配。

// fakeCtx 是最小可用的 contractshttp.Context。
//
// 只实现中间件真正用到的那几个方法，其余一律 panic——框架的 Context 有 60 多个
// 方法，全写出来只会变成噪音；写成 panic 反而更好：中间件一旦多用了别的上下文
// 能力，测试会立刻炸在这儿，而不是安静地少断言一样东西。
type fakeCtx struct {
	values map[string]any
	req    *fakeRequest
	resp   *fakeResponse
}

func newFakeCtx(values map[string]any) *fakeCtx {
	if values == nil {
		values = map[string]any{}
	}
	return &fakeCtx{
		values: values,
		req:    &fakeRequest{},
		resp:   &fakeResponse{},
	}
}

func (c *fakeCtx) Context() context.Context { panic("本用例不涉及 gin 的 context") }

func (c *fakeCtx) WithContext(context.Context) { panic("本用例不涉及 gin 的 context") }

// contractshttp.Context 内嵌了 context.Context，所以这三个方法也得有。
// 中间件一个都不碰，写成 panic 是为了让「它哪天碰了」变成一次可见的失败。
func (c *fakeCtx) Deadline() (time.Time, bool) { panic("本用例不涉及 context.Context") }

func (c *fakeCtx) Done() <-chan struct{} { panic("本用例不涉及 context.Context") }

func (c *fakeCtx) Err() error { panic("本用例不涉及 context.Context") }

// WithValue 照单全收：中间件会往 ctx 里写 role 与 user，
// 写完之后这里的 values 就是断言「控制器能不能拿到操作者」的地方。
func (c *fakeCtx) WithValue(key any, value any) {
	name, ok := key.(string)
	if !ok {
		panic("ctx 的键一律是字符串")
	}
	c.values[name] = value
}

func (c *fakeCtx) Value(key any) any {
	name, ok := key.(string)
	if !ok {
		panic("ctx 的键一律是字符串")
	}
	return c.values[name]
}

func (c *fakeCtx) Request() contractshttp.ContextRequest { return c.req }

func (c *fakeCtx) Response() contractshttp.ContextResponse { return c.resp }

type fakeRequest struct {
	contractshttp.ContextRequest
	nextCall int
}

func (r *fakeRequest) Next() { r.nextCall++ }

type fakeResponse struct {
	contractshttp.ContextResponse
	code     int
	body     map[string]any
	aborted  bool
	jsonCall int
}

func (r *fakeResponse) Render() error { return nil }

func (r *fakeResponse) Json(code int, obj any) contractshttp.AbortableResponse {
	r.jsonCall++
	r.code = code
	if m, ok := obj.(map[string]any); ok {
		r.body = m
	}
	return r
}

func (r *fakeResponse) Abort() error {
	r.aborted = true
	return nil
}

// TestPermission_未登录一律拒绝 是 fail-closed 的第一道。
//
// 没有 user_id 的情况下**绝不能**走到权限判断——哪怕 Casbin 那边配置有问题、
// 哪怕权限名是空的。漏了这一道的话，任何一个没带 Authorization 头的请求都
// 会带着空角色去问策略；而空角色在 Enforce 里匹配不到任何一行，本来会被拒，
// 但那是「碰巧被拒」，不是「被明确拒」。差别在于：将来有人给策略加了
// `p, "", log.view`（或者把 matcher 改成通配），碰巧被拒就变成了真放行。
func TestPermission_未登录一律拒绝(t *testing.T) {
	cases := []struct {
		name   string
		values map[string]any
	}{
		{"ctx 里什么都没有", nil},
		{"user_id 类型不对（字符串）", map[string]any{"user_id": "1"}},
		{"user_id 为 0", map[string]any{"user_id": uint(0)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := newFakeCtx(c.values)
			RequirePermission(rbac.PermUserView).Handle(ctx)

			if ctx.resp.code != 401 {
				t.Errorf("未登录应返回 401，实际 %d", ctx.resp.code)
			}
			if !ctx.resp.aborted {
				t.Error("未登录的响应必须 Abort，否则后面的处理器照样会跑")
			}
			if ctx.req.nextCall != 0 {
				t.Error("未登录时不应调用 Next()")
			}
		})
	}
}

// TestPermission_Signature声明了权限名 是 routes 包挂载断言的地基。
//
// 那边靠 "perm=user.view" 这个字符串来判断「这条路由挂了哪项权限」。
// 签名与实际要校验的权限一旦对不上（例如复制粘贴时 perm 字段忘了改），
// 路由测试会全绿，而线上挂的是另一道门——所以这里把两者绑在一起断言。
func TestPermission_Signature声明了权限名(t *testing.T) {
	for _, perm := range rbac.AllPermissions() {
		m := RequirePermission(perm)
		if got := m.Signature(); got != "perm="+perm {
			t.Errorf("中间件签名 %q 与它实际校验的权限 %q 不一致", got, perm)
		}
	}
}

// TestPermission_守卫名不会与等级门槛混淆 挡住命名上的混淆。
//
// 迁移前路由上挂的是 "role>=admin"，签名长得完全不同。这里确认新的签名
// 不含任何能让人一眼看成等级门槛的片段——将来有人 grep "role" 排查问题时，
// 不该在权限守卫里翻出东西。
func TestPermission_守卫名不会与等级门槛混淆(t *testing.T) {
	sig := RequirePermission(rbac.PermSwitchOperate).Signature()
	if strings.Contains(sig, "role") {
		t.Errorf("权限守卫的签名不该含 role，实际 %q", sig)
	}
	if strings.Contains(sig, ">=") {
		t.Errorf("权限守卫的签名不该含等级比较，实际 %q", sig)
	}
}

// TestPermission_与已删除的切台守卫不同名 防的是「换了实现忘了改路由」。
//
// RequireSwitchingOperator 已经被 switch.operate 取代（等价性由
// app/rbac 里的 TestSwitchOperate_与被删掉的白名单逐字等价 证明）。
// 如果将来某个路由上还挂着 switching-operator，说明有一处守卫**没有**
// 随迁移换掉——那套接口要么没人能进（权限名对不上，一律拒绝），要么还是
// 老的那套白名单（等于权限迁移对它无效）。
func TestPermission_与已删除的切台守卫不同名(t *testing.T) {
	m, ok := RequirePermission(rbac.PermSwitchOperate).(*PermissionMiddleware)
	if !ok {
		t.Fatal("RequirePermission 应返回 *PermissionMiddleware")
	}
	if m.Signature() == "switching-operator" {
		t.Error("切台守卫的签名仍是已删除的 switching-operator")
	}
}
