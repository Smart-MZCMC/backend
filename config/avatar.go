package config

import "github.com/goravel/framework/facades"

// 头像服务配置。
//
// 用的是 WeAvatar（https://weavatar.com）：Gravatar 的国内可用替代，不翻墙，
// 支持按邮箱取头像，接口与 Gravatar 兼容。
//
// 做成可配置是为了留后路：校园内网无外网时可以指向自建镜像或代理地址，
// 不用改代码。
func init() {
	config := facades.Config()
	config.Add("avatar", map[string]any{
		// 头像服务基地址。取 URL 时在其后拼 "/<md5>?s=<尺寸>"。
		"base_url": config.Env("AVATAR_BASE_URL", "https://weavatar.com/avatar"),

		// 头像边长（像素）。管理后台只展示 32px 与 40px 两种尺寸，取 96 是为了
		// 在高 DPI 屏上不发虚。
		"size": config.Env("AVATAR_SIZE", 96),
	})
}
