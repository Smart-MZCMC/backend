package config

import "github.com/goravel/framework/facades"

// 在线更新配置。
//
// 默认关闭。这是一个会下载并替换服务自身可执行文件的功能，风险远高于普通的
// 后台操作，所以必须显式打开，而不是「配了就生效」。
func init() {
	config := facades.Config()
	config.Add("update", map[string]any{
		// 总开关。为 false 时 /api/system/update* 一律拒绝执行。
		"enabled": config.Env("UPDATE_ENABLED", false),

		// server 是**更新源的 API 基址**，不是文件服务器地址。
		//
		// 留空表示走 GitHub 官方 API。校园内网通常没有外网，这时可以指到
		// 自建的 GitHub 兼容 API（例如 Gitea），形如
		// http://192.168.1.10/api/v1 —— 注意要带上它自己的 /api/v1 前缀，
		// 本项目请求的是 {server}/repos/{owner}/{repo}/releases/latest。
		"server": config.Env("UPDATE_SERVER", ""),

		// download_mirror 是**资产下载**镜像的前缀，与 server 无关。
		//
		// 两者要分开，是因为它们解决的是不同的问题：server 解决「查不到版本」
		// （api.github.com 在校园网里有时也连不上），download_mirror 解决的
		// 是「查到了但下不动」——GitHub 的 Release 资产走的是 github.com 的
		// 下载域名，而那个域名经常被 TCP 阻断（本项目在推代码时就撞上过），
		// 于是表现为「能查到有新版，一点更新就卡住不动」。
		//
		// 写法是前缀包裹原始 URL，即最终请求 {前缀}{原始URL}：
		//   UPDATE_DOWNLOAD_MIRROR=https://ghfast.top/
		//   UPDATE_DOWNLOAD_MIRROR=https://gh-proxy.com/
		//   UPDATE_DOWNLOAD_MIRROR=http://192.168.1.10/github-proxy/
		// 留空表示直接从 GitHub 下载。
		//
		// 走镜像**不会削弱完整性保证**：sha256 校验照做且必须通过，
		// 镜像返回一个 HTML 错误页或旧版本都会在 verifyChecksum 处被拒绝。
		// 注意 checksums.txt 也经镜像取，取不到就整体拒绝执行而不是跳过校验。
		"download_mirror": config.Env("UPDATE_DOWNLOAD_MIRROR", ""),

		// GitHub 仓库，owner/repo。仅在 server 为空时用到。
		"repo": config.Env("UPDATE_REPO", "Smart-MZCMC/backend"),

		// 发布包资产名，必须与 release.yml 产出的文件名一致。
		"asset": config.Env("UPDATE_ASSET", "backend-linux-amd64.tar.gz"),

		// 允许自动替换可执行文件并重启进程。
		//
		// 保持 false 时，apply 只把新版本下载、校验并解压到 update/staged/，
		// 由运维确认后手工替换——适合先看清会发生什么。
		//
		// 置为 true 需要一个前提：进程必须由 systemd 之类的管理器托管，
		// 替换后本进程退出才能被重新拉起。若是直接 ./smart-mzcmc 起的，
		// 退出就等于停服。这也是默认关闭的另一个原因。
		"allow_replace": config.Env("UPDATE_ALLOW_REPLACE", false),
	})
}
