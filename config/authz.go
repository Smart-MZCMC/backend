package config

import (
	"smart-mzcmc/app/facades"
)

func init() {
	config := facades.Config()
	config.Add("authz", map[string]any{
		// require_project_membership 决定 user_projects 表是否真的参与鉴权。
		//
		// 默认 false。这张表从第一天就在，但此前只被管理接口增删查，从未
		// 决定过任何人能看什么：后勤账号能看到全部项目，控制权接口只从 URL
		// 取 projectId，WebSocket 更是知道 project_id 就能监听整个项目。
		//
		// 之所以默认关闭：现有部署里未必给每个人都配了项目授权，直接打开会
		// 让现场当场连不上。关闭时中间件只记日志、不拦截，先把「哪些人会
		// 被拦」写进日志，确认授权配齐后再把它改成 true。
		"require_project_membership": config.Env("REQUIRE_PROJECT_MEMBERSHIP", false),

		// admin_min_role 决定谁能登录**管理后台网页**。
		//
		// 为什么单独一个开关、而不是在 /api/auth/login 里直接拦：导播端、
		// 采访端、解说端这些原生应用走的是同一个登录接口，导播账号本来就该
		// 能登录进去（它用的是原生界面，不是网页后台）。在这里拦等于把它们
		// 一起打死，属于破坏性变更。所以网页后台改走 /api/auth/admin-login，
		// 由它额外校验这一条。
		//
		// 默认 leader：只有负责人及以上能登录网页后台。
		//
		// 网页后台是「管理与查看」的界面，导播、包装、解说、前期、后勤的工作
		// 都在各自的原生客户端或现场设备上完成，没有理由进后台——导播尤其如此：
		// 它的切台动作走 /api/locks/*，而网页后台里没有任何切台入口。
		//
		// ⚠️ 这一项的含义随角色等级重排变过，必须连着梯子一起看：
		// 重排前后勤 20 / 导播 10，于是默认值 logistics 恰好等于「除导播外都能进」，
		// 导播被顺带挡在后台外。改成 leader 后包装(25)、解说(20)、前期(20)、
		// 后勤(10) 也一并挡在门外——这是有意的，后台不是他们的工位。
		//
		// 注意「除导播以外的所有角色」在新梯子上无法用等级表达出来（导播 30 夹在
		// 中间，floor 一旦 ≥40 就会把下面那批一起挡掉）。要放开某几档只能调这个值，
		// 而它只能选一道门槛，选不出「排除中间某个人」这种形状。
		// 需要更宽就设环境变量，例如 ADMIN_MIN_ROLE=logistics。
		"admin_min_role": config.Env("ADMIN_MIN_ROLE", "leader"),
	})
}
