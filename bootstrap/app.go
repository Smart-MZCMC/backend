package bootstrap

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"

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
			// 自动迁移。必须排在 rbac.Bootstrap() 之前：策略表 role_permissions
			// 本身就是一条迁移建的，先迁移，RBAC 才拿得到那张表。反过来的话
			// 每次全新启动都会先撞一次「no such table」再退回内嵌 policy.csv，
			// 日志里平白多一条降级告警，看起来像出故障。
			//
			// 失败就 Fatalf、不带着半套 schema 继续服务，理由有两条：
			//   一、更新流程里迁移失败本来就会换回备份程序（updater.go 第 5 步），
			//      「迁移失败就不该跑起来」这条规则项目里已经定过一次了，
			//      冷启动走另一套标准会让同一个故障有两种结局。
			//   二、迁移列表是顺序执行的，第 7 条失败时前 6 条已经落库。
			//      带着这种状态继续服务，报错会出现在毫不相干的地方（某个接口
			//      突然 500），比「服务没起来、日志里写着哪条迁移失败」难查得多。
			//
			// 刻意不加「跳过迁移强行启动」的开关：多一个旋钮就多一种「有人把它关了
			// 然后忘了」，而这条路径上没有任何安全的降级——schema 与代码对不上之后，
			// 错误只会出现在毫不相干的接口上。真要绕开，运维有更可控的办法：
			// 单独跑 migrate 看清原因（见下面提示的第 2 条），或临时把二进制换回
			// 上一版，那一版的代码本来就匹配旧 schema。
			if err := RunMigrations(); err != nil {
				log.Fatalf("%s", migrationFailureReport(err))
			}

			// 权限策略的启动装载：策略表为空则用 policy.csv 播种一次，
			// 之后以数据库为准。
			//
			// 为什么是 WithCallback 而不是 main 里的一行：WithCallback 在
			// Build() 末尾触发，那时数据库服务已注册、facades 可用；而 main
			// 里的位置都在 bootstrap.Boot() 之前，ORM 那时还没装配好。
			//
			// 它不会让启动失败——策略表坏掉时退回内嵌 policy.csv，并把这件事
			// 吵到日志与 GET /api/rbac/policy 的 warnings 里。直播不该因为
			// 一张策略表停摆。这与上面迁移失败就停是两种不同的故障：
			// 策略表读不出来时退回内嵌策略，服务功能完整；
			// 而迁移失败意味着表结构与代码对不上，没有「退化但正确」的选项。
			//
			// 装配存储用 rbac.SetStore：策略存在哪由 app/rbac 决定，这里只负责
			// 在正确的时刻告诉它「ORM 已就绪」。
			rbac.SetStore(rbac.DefaultStore())
			rbac.Bootstrap()
		}).
		Create()
}

// appName 取可执行文件名，用于把「怎么单独跑迁移」写进错误提示。
//
// 不硬编码 "smart-mzcmc"：发布包里的二进制名各渠道可能不同，写死的话提示
// 会让人照着敲一条不存在的命令。
func appName() string {
	if len(os.Args) == 0 {
		return "smart-mzcmc"
	}
	return filepath.Base(os.Args[0])
}

// migrationFailureReport 组织迁移失败时打进日志的那段话。
//
// 分行写、每行一件事，是因为读它的人是凌晨被 journalctl 叫起来的运维，而它常常
// 是 systemd Restart=always 崩溃重启循环里的最后一行。
//
// 三条处理建议是按「实际发生频率」排的，不是按「技术相关性」排的。实测下来
// `table "xxx" already exists` 占绝大多数（另一个实例/另一次 migrate 同时在跑），
// 所以「先看有没有第二个实例」排在最前面——这一条如果不说，运维看到
// already exists 的第一反应是「库坏了」，于是去恢复备份，而库根本没坏，
// 恢复备份反而把现场弄得更乱。
func migrationFailureReport(err error) string {
	var b strings.Builder
	b.WriteString("[Migrate] 数据库迁移失败，服务拒绝启动。\n")
	b.WriteString("  这不是程序缺陷：迁移是幂等的，反复执行都安全，失败只说明这一次没跑成。\n")

	var me *MigrationError
	if errors.As(err, &me) {
		b.WriteString("  失败迁移: " + me.Signature + "\n")
		b.WriteString("  原始错误: " + me.Err.Error() + "\n")
	} else {
		b.WriteString("  原始错误: " + err.Error() + "\n")
	}

	b.WriteString("  处理办法（按实际发生频率排序）：\n")
	b.WriteString("   1) 先确认没有第二个实例在同时启动或执行 migrate" +
		"（systemd 单元配重了、有人在旁边手动起了副本、容器起了两个副本）。" +
		"报 `already exists` 时九成是这种情况，数据库本身是好的。\n")
	b.WriteString("   2) 单独确认迁移状态，不启动任何服务：`" + appName() + " migrate`（可反复执行）。\n")
	b.WriteString("   3) 确认没有并发、单独跑 migrate 仍然失败，才需要人工处理这条迁移：" +
		"从数据库备份恢复后再启动，或对照 database/migrations/ 手工执行。\n")
	return b.String()
}
