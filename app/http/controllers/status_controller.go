package controllers

import (
	"github.com/goravel/framework/contracts/http"

	"smart-mzcmc/app/ws"
)

type StatusController struct{}

// Version 当前后端版本号。会随发布手动同步，改动见 CHANGELOG。
const Version = "1.4.0"

// MinClientVersion 是所有客户端都必须满足的最低适配版本。
//
// 为什么要有「最低适配版本」，而不是直接比客户端版本和后端版本的大小：
// 后端发新版不代表客户端必须跟着重建。绝大多数改动（改文案、修 bug）对客户端
// 完全透明，此时若按「版本不等就告警」处理，每次发版所有客户端都会被提醒一遍，
// 久而久之这个横幅就没人看了 —— 告警只有在该响的时候响才有意义。
//
// 所以语义分两档：
//   - 客户端 < MinClientVersion：老版本真的会有功能异常，硬提示必须更新
//   - MinClientVersion <= 客户端 < Version：只是落后，软提示建议更新
//
// 什么时候该动它：只有在引入了老客户端无法承受的后端改动时才上调，平时不动。
//
// 1.4.0 刻意没有上调：切台协议（shot_state）没变，欢迎消息新增的字段是附加的，
// 项目成员校验默认关闭，新增接口也只是多出来的。老客户端连新后端一切照旧，
// 所以它们看到的应该是琥珀色「建议更新」，而不是红色「必须更新」。
const MinClientVersion = "1.3.0"

func NewStatusController() *StatusController {
	return &StatusController{}
}

func (c *StatusController) ServerStatus(ctx http.Context) http.Response {
	return ctx.Response().Json(200, map[string]any{
		"status":       "running",
		"online_count": ws.DefaultHub.GetOnlineCount(),
		"version":      Version,
		// 客户端据此判断自己是不是太旧。字段是新增的，老客户端不认也不受影响
		// （它们本来就没有版本提示逻辑）。
		"min_client_version": MinClientVersion,
	})
}
