package bootstrap

import (
	contractsfoundation "github.com/goravel/framework/contracts/foundation"
	contractsconfiguration "github.com/goravel/framework/contracts/foundation/configuration"
	"github.com/goravel/framework/foundation"

	"smart-mzcmc/app/rbac"
	"smart-mzcmc/config"
	"smart-mzcmc/routes"
)

func Boot() contractsfoundation.Application {
	return foundation.Setup().
		WithMigrations(Migrations).
		WithRouting(func() {
			routes.Web()
			routes.Grpc()
		}).
		WithMiddleware(func(handler contractsconfiguration.Middleware) {
			// 管理后台（/admin）与文档站（/docs）的静态托管。
			// 中间件让静态文件优先于业务路由，未命中则放行给业务路由，
			// 详见 routes/staticSite.go 的说明。
			handler.Append(routes.StaticSites())
			// 静态站点之后挂初始化网关：命中静态文件的请求已经被接管，
			// 网关只需处理真正落到业务路由上的请求。顺序反过来的话，
			// /admin/setup 向导页本身都会被拦掉。
			handler.Append(routes.SetupGate())
		}).
		WithProviders(Providers).
		WithConfig(config.Boot).
		WithCallback(func() {
			// 权限策略的启动装载：策略表为空则用 policy.csv 播种一次，
			// 之后以数据库为准。
			//
			// 为什么是 WithCallback 而不是 main 里的一行：WithCallback 在
			// Build() 末尾触发，那时数据库服务已注册、facades 可用；而 main
			// 里的位置都在 bootstrap.Boot() 之前，ORM 那时还没装配好。
			//
			// 它不会让启动失败——策略表坏掉时退回内嵌 policy.csv，并把这件事
			// 吵到日志与 GET /api/rbac/policy 的 warnings 里。直播不该因为
			// 一张策略表停摆。
			//
			// 装配存储用 rbac.SetStore：策略存在哪由 app/rbac 决定，这里只负责
			// 在正确的时刻告诉它「ORM 已就绪」。
			rbac.SetStore(rbac.DefaultStore())
			rbac.Bootstrap()
		}).
		Create()
}
