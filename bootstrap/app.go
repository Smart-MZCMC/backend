package bootstrap

import (
	contractsfoundation "github.com/goravel/framework/contracts/foundation"
	contractsconfiguration "github.com/goravel/framework/contracts/foundation/configuration"
	"github.com/goravel/framework/foundation"

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
		Create()
}
