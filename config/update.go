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

		// 更新源。留空表示走 GitHub 官方 Release。
		//
		// 校园内网通常没有外网，这时可以指到自建镜像（Gitea / 内网文件服务），
		// 例如 http://192.168.1.10/releases。留空时走 GitHub。
		"server": config.Env("UPDATE_SERVER", ""),

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
