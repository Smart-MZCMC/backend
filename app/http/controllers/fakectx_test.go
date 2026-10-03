package controllers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	contractshttp "github.com/goravel/framework/contracts/http"

	"smart-mzcmc/app/models"
)

// 最小可用的 contractshttp.Context，只实现控制器真正用到的那几个方法。
//
// 只写要用的、其余靠内嵌接口在真被调用时 panic，是为了让「端点多用了一个
// 上下文能力」变成一次可见的失败，而不是安静地少断言一样东西。
// （middleware/permission_test.go 里的 fakeCtx 是同一套做法。）
// policyCtx 刻意**不内嵌** contractshttp.Context，而是把它的每个方法都写出来。
// 内嵌会得到一个名为 Context 的字段，与接口里的 Context() 方法同名，
// 于是结构体上的那个方法被字段盖掉，policyCtx 不再满足接口。
// 把方法逐个写出来之后，「控制器多用了哪个上下文能力」会变成一次 panic。
type policyCtx struct {
	values map[string]any
	req    *policyRequest
	resp   *policyResponse
}

// contractshttp.Context 内嵌了 context.Context，所以下面这几个方法也得有。
// 权限编辑这条路径一个都不碰，写成 panic 是为了让「它哪天碰了」可见。
func (c *policyCtx) Context() context.Context { panic("本用例不涉及 gin 的 context") }

func (c *policyCtx) WithContext(context.Context) { panic("本用例不涉及 gin 的 context") }

func (c *policyCtx) Deadline() (time.Time, bool) { panic("本用例不涉及 context.Context") }

func (c *policyCtx) Done() <-chan struct{} { panic("本用例不涉及 context.Context") }

func (c *policyCtx) Err() error { panic("本用例不涉及 context.Context") }

// policyResponse 是控制器返回的那个 AbortableResponse。
//
// 单独给它一个名字而不是复用 ContextResponse：前者是「控制器写出来的响应」
// （被测对象），后者是「上下文提供的响应能力」（被测对象的依赖）。
// 名字混起来会让读的人分不清这份 code 是被测代码设的还是别处来的。
type policyResponse struct {
	contractshttp.ContextResponse
	status int
	body   map[string]any
}

func newPolicyCtx(t *testing.T, actor models.User, routeParams map[string]string, body map[string]any) *policyCtx {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	return &policyCtx{
		values: map[string]any{"user": actor},
		req:    &policyRequest{route: routeParams, all: body},
		resp:   &policyResponse{},
	}
}

type policyRequest struct {
	contractshttp.ContextRequest
	route map[string]string
	all   map[string]any
}

func (r *policyRequest) Route(key string) string { return r.route[key] }

func (r *policyRequest) All() map[string]any { return r.all }

func (r *policyResponse) Render() error { return nil }

// Json 记录状态码与响应体，并返回自己（链式 Abort 的一部分）。
//
// 响应体在这里做一次 **JSON 往返**（编成字节再解回 map），而不是直接把
// 结构体存下来。理由：前端看到的是 JSON 字节，字段名、omitempty 行为、
// nil 编成 null 还是 [] 这些**只有经过序列化才确定**。直接拿结构体断言
// 会漏掉「json tag 拼错了」这类问题——那正是前后端之间唯一的契约。
func (r *policyResponse) Json(code int, obj any) contractshttp.AbortableResponse {
	r.status = code
	if obj == nil {
		return r
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		panic("响应体无法序列化：" + err.Error())
	}
	if err := json.Unmarshal(raw, &r.body); err != nil {
		panic("响应体不是 JSON 对象：" + err.Error())
	}
	return r
}

func (r *policyResponse) Abort() error { return nil }

func (c *policyCtx) Request() contractshttp.ContextRequest { return c.req }

func (c *policyCtx) Response() contractshttp.ContextResponse { return c.resp }

func (c *policyCtx) Value(key any) any {
	name, ok := key.(string)
	if !ok {
		panic("ctx 的键一律是字符串")
	}
	return c.values[name]
}

func (c *policyCtx) WithValue(key any, value any) {
	name, ok := key.(string)
	if !ok {
		panic("ctx 的键一律是字符串")
	}
	c.values[name] = value
}

// replyOf 从上下文里取出控制器写下的响应。
//
// 控制器的返回类型是 contractshttp.Response（链式 Abort 的结果），
// 真正记录状态码与响应体的是本文件里的 *policyResponse。断言走这条路而不是
// 改控制器的返回类型——那是被测代码的形状，不该为了好测而动。
func replyOf(t *testing.T, ctx *policyCtx, _ contractshttp.Response) *policyResponse {
	t.Helper()
	if ctx.resp.status == 0 {
		t.Fatal("控制器没有写出任何响应——它一定在某条分支上没返回东西")
	}
	return ctx.resp
}
