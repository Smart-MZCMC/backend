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
		// 默认 logistics(20)：六个角色里只有导播(10)被挡在外面，其余都能进。
		// 理由是「除了导播之外，其余角色都是在电脑上干活的」，网页后台对
		// 它们都有意义；导播的工作在导播室里用原生应用完成，不需要后台。
		// 需要更严就把环境变量调高，例如 ADMIN_MIN_ROLE=leader。
		"admin_min_role": config.Env("ADMIN_MIN_ROLE", "logistics"),
	})
}
