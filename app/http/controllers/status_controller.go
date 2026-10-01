package controllers

import (
	"github.com/goravel/framework/contracts/http"

	"smart-mzcmc/app/ws"
)

type StatusController struct{}

// Version 当前后端版本号。会随发布手动同步，改动见 CHANGELOG。
const Version = "1.4.4"

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
// 1.4.2 刻意没有上调。这一版的改动逐条对过老客户端：
//   - WS 广播新增 sender_name / sender_role：附加字段，老客户端不认也不受影响；
//   - shot_state 现在也发给采访端：老采访端对非 system 的消息直接 return，
//     收到也只是忽略；
//   - /api/logs 新增 sender：附加字段；
//   - /api/setup/status 在已初始化后不再下发部署细节：唯一读取方是随本版
//     一起发布的管理后台，老缓存版本走到 needs_setup=false 分支也不会碰这些字段；
//   - audit_logs.created_at 统一成 UTC：纯数据格式修正，不对外。
//
// 没有一条会让老客户端连上新后端出现功能异常，所以它们看到的应该是琥珀色
// 「建议更新」，而不是红色「必须更新」。
//
// ⚠️ 唯一的行为变化是 ADMIN_MIN_ROLE：默认 logistics，导播账号不再能登录
// 网页后台。原生导播端走的是 /api/auth/login，完全不受影响。
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
