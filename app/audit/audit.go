// Package audit 负责把敏感操作写进 audit_logs 表。
//
// 单独成包的原因：写审计的两处调用点分属 controllers 与 plugins，
// 让 plugins 反向 import controllers 只为拿一个写日志的函数并不合理。
// 这个包只依赖 models 与框架的 http 契约，谁都能引。
package audit

import (
	"encoding/json"
	"log"
	"time"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
)

// Record 是一次待落库的审计记录。
//
// Action 是机器可读的动作名（如 project.create），供界面按类型筛选；
// Summary 是给人看的一句话，会写进 Detail。
type Record struct {
	Action     string
	Summary    string
	TargetType string
	TargetID   string
	Detail     map[string]any
}

// Write 把一条审计记录写进 audit_logs，同时打一份到 stdout。
//
// 为什么必须入库：stdout 日志由 config/logging.go 定为 7 天轮转，而审计
// 要回答的是「上个月谁清掉了日志」这类问题；更要紧的是「清理日志」这个
// 动作本身会删掉证据，只留在文件日志里等于把记录和被删的证据放在同一个
// 篮子里。
//
// 写库失败只记日志、不影响主流程：审计是旁路，不能因为它挂了就让正在
// 直播的系统做不了操作。
func Write(ctx http.Context, actor models.User, record Record) {
	detail := record.Summary
	if len(record.Detail) > 0 {
		if raw, err := json.Marshal(record.Detail); err == nil {
			if detail != "" {
				detail += " " + string(raw)
			} else {
				detail = string(raw)
			}
		}
	}

	log.Printf("[AUDIT] 操作者 %s(#%d, %s) 执行 %s %s",
		actor.Username, actor.ID, models.Role(actor.Role).Label(), record.Action, record.Summary)

	entry := models.AuditLog{
		ActorID: actor.ID,
		// 只存脱敏后的用户名：审计表会被导出、被人在后台翻看。
		ActorUsername: models.MaskUsername(actor.Username),
		Action:        record.Action,
		TargetType:    record.TargetType,
		TargetID:      record.TargetID,
		Detail:        detail,
		IP:            clientIP(ctx),
		// 必须显式转 UTC。time.Now() 带本地时区，GORM 会把偏移量一起写进
		// 列里，于是这里存的是 `2026-10-02T00:13:19+08:00`，而其余靠
		// GORM 自动时间戳的表存的是 `2026-10-01T16:13:19Z`。
		//
		// 混存的后果不是显示难看，而是**查不出来**：created_at 在 SQLite 里
		// 是 TEXT，比较按字符串逐字符进行，而筛选条件经 parseTimeFilter 归一
		// 成 UTC（`...16:14:00Z`）。`'2026-10-02T00:13:19+08:00' <=
		// '2026-10-01T16:14:00Z'` 为假，于是任何带时间筛选的查询都返回 0 条，
		// 而管理后台默认就带了「最近 7 天」——表现正是「明明有记录，审计页却是空的」。
		CreatedAt: time.Now().UTC(),
	}
	if err := facades.Orm().Query().Create(&entry); err != nil {
		log.Printf("[AUDIT] 审计记录写库失败 action=%s: %v", record.Action, err)
	}
}

// clientIP 取客户端地址。取不到时返回空串而不是 "unknown"——空串在界面上
// 显示为「-」，比一个看起来像真实值的占位符更不容易误导人。
func clientIP(ctx http.Context) (ip string) {
	if ctx == nil {
		return ""
	}
	// 部分调用点（后台任务）的 ctx 可能处于半初始化状态，不能让它把
	// 整条审计记录带崩。
	defer func() {
		if r := recover(); r != nil {
			ip = ""
		}
	}()
	return ctx.Request().Ip()
}

// ActorFrom 从上下文取当前操作者。
//
// 与 controllers.actorFrom 是同一件事，但那个函数在 controllers 包内，
// plugins 引不到。返回 false 表示上下文里没有已认证的用户。
func ActorFrom(ctx http.Context) (models.User, bool) {
	user, ok := ctx.Value("user").(models.User)
	if !ok || user.ID == 0 {
		return models.User{}, false
	}
	return user, true
}
