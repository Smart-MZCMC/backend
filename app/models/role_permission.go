package models

import "time"

// RolePermission 是「角色 × 权限」矩阵里的一格。
//
// 为什么存**完整矩阵**而不是只存「已授予的行**：
//   - 只存差异时，「这一格没出现」有两种含义——「没授予」与「压根没写过」。
//     而这两种在出故障时结论完全相反：前者是配置，后者是数据被截断。
//   - 在线编辑界面渲染的是一整张表，缺行会被当成「不可用」而不是「未授予」，
//     于是有人会去补，补出来的顺序与 policy.csv 不一致，日后无法比对。
//   - 96 行（8 角色 × 12 权限）的体量不值得为省这几行去换掉可读性。
//
// 所以 Enabled 显式落库：true = 授予，false = 明确不给。矩阵与 app/rbac
// 声明的权限集合一一对应，多一行少一行都是异常（见 app/rbac 的 matrixWarnings）。
type RolePermission struct {
	ID uint `json:"id" gorm:"primaryKey"`
	// Role 是 models.Role 的字面值。
	//
	// 存字符串而不是 models.Role：这是从请求里来的数据，写进库之前必须过
	// ValidateGrant/ValidateRemoveRole，读出来之后还要再判一次 Valid()，
	// 期间不能假设它一定是合法角色（users.role 那张表已经因为同样的假设
	// 踩过一次）。
	Role string `json:"role" gorm:"size:20;index"`
	// Permission 同样是裸字符串，理由同上。
	Permission string    `json:"permission" gorm:"size:64;index"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (RolePermission) TableName() string {
	return "role_permissions"
}
