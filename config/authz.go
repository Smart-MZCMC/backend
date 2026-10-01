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
	})
}
