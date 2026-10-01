package models

import (
	"strings"
	"time"
)

// AuditLog 是一条敏感操作记录。
//
// 之前 auditAction/auditRoleChange 只 log.Printf 到 stdout，而 stdout 日志
// 由 config/logging.go 定为 7 天轮转，且只覆盖三处操作（删用户、改角色、
// 系统更新）。这意味着「谁把谁降了权」「谁清掉了日志证据」根本查不到。
// 落库之后保留期与日志保留策略脱钩，管理后台也能直接检索。
//
// ActorUsername 存的是**脱敏后**的用户名：审计表会被导出、被人在后台翻看，
// 完整账号名没有必要。ActorID 保留原值，需要精确追溯时按 ID 去 users 查。
type AuditLog struct {
	ID            uint   `json:"id" gorm:"primaryKey"`
	ActorID       uint   `json:"actor_id" gorm:"index"`
	ActorUsername string `json:"actor_username" gorm:"size:64"`
	Action        string `json:"action" gorm:"size:64;index"`
	TargetType    string `json:"target_type" gorm:"size:32"`
	TargetID      string `json:"target_id" gorm:"size:64"`
	// Detail 是自由结构的 JSON 文本，内容随 Action 不同而不同。
	Detail    string    `json:"detail" gorm:"type:text"`
	IP        string    `json:"ip" gorm:"size:64"`
	CreatedAt time.Time `json:"created_at" gorm:"index"`
}

func (AuditLog) TableName() string {
	return "audit_logs"
}

// MaskUsername 把用户名脱敏成「首字符 + *** + 末字符」。
//
// 长度不足时全打码，避免出现「a*」这种等于没脱敏的结果。
func MaskUsername(name string) string {
	runes := []rune(name)
	switch {
	case len(runes) == 0:
		return ""
	case len(runes) <= 2:
		return strings.Repeat("*", len(runes))
	case len(runes) <= 4:
		return string(runes[:1]) + strings.Repeat("*", len(runes)-1)
	default:
		return string(runes[:1]) + "***" + string(runes[len(runes)-1:])
	}
}
