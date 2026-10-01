package routes

import (
	"net/http"
	"strings"

	contractshttp "github.com/goravel/framework/contracts/http"

	"smart-mzcmc/app/setup"
)

// SetupGate 在系统尚未初始化时拦住业务接口。
//
// 背景：全新部署时数据库是空的，除 /api/setup/* 以外的任何接口都要么报 500
// （表不存在），要么在用户表为空时把注册接口暴露出去。与其让前端拿到一堆
// 看不懂的错误，不如统一返回 503 + 明确的引导地址，让浏览器直接跳到向导页。
//
// 已初始化的部署上这个中间件只做一次原子读，不查库。
func SetupGate() contractshttp.Middleware {
	return &setupGateMiddleware{}
}

type setupGateMiddleware struct{}

// Signature 供 Goravel 识别中间件，需全局唯一。
func (m *setupGateMiddleware) Signature() string {
	return "smart-mzcmc-setup-gate"
}

func (m *setupGateMiddleware) Handle(ctx contractshttp.Context) {
	if !setup.NeedsSetup() {
		return
	}

	requestPath := ctx.Request().Path()

	// 初始化接口本身必须放行，否则就是「锁上门再把钥匙锁在里面」。
	// 健康检查放行给监控探针用：它能如实报出数据库不可用（503），
	// 比被这里拦成「需要初始化」更有信息量。
	if strings.HasPrefix(requestPath, "/api/setup") || requestPath == "/api/health" {
		return
	}

	if strings.HasPrefix(requestPath, "/api/") {
		// 必须用链式 Abort()：先 Json 再 Abort 会被 gin 重置成空响应。
		ctx.Response().Json(http.StatusServiceUnavailable, map[string]any{
			"code":      "setup_required",
			"error":     "系统尚未初始化，请先完成初始化向导",
			"setup_url": "/admin/setup",
		}).Abort()
		return
	}

	// /admin 与 /docs 由 StaticSites 中间件先接管，/public 是静态资源，
	// 这里只管「没被静态站点接走、又落到业务路由上」的路径 —— 实际只有首页 "/"。
	// 不动它们是因为本中间件排在 StaticSites 之后：静态站点写响应时没有
	// Abort，链路会继续走到这里，此时再写重定向会把已经写好的页面冲掉。
	if requestPath != "/" {
		return
	}

	ctx.Response().Redirect(http.StatusFound, "/admin/setup").Abort()
}
