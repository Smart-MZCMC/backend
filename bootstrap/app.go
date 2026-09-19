package bootstrap

import (
	contractsconfiguration "github.com/goravel/framework/contracts/foundation/configuration"
	contractsfoundation "github.com/goravel/framework/contracts/foundation"
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
		}).
		WithProviders(Providers).
		WithConfig(config.Boot).
		Create()
}
