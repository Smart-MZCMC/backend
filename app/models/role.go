package models

// Role 是用户的系统角色。
//
// 之前角色是散落在各处的裸字符串比较（`role != "admin"`），没有统一定义，
// 于是「超级管理员」这类新角色会被每一处硬编码的等值判断挡下来——
// RequireRole("admin") 不会放行 super_admin，只能靠人肉在两处路由上补名字。
// 这里把角色集中成带等级的定义，让权限判断只认等级，角色之间自然形成高低。
type Role string

const (
	// RoleSuperAdmin 超级管理员。独占：系统在线更新、管理其他超级管理员。
	RoleSuperAdmin Role = "super_admin"
	// RoleAdmin 管理员。用户、项目、权限分配、日志导出与清理。
	RoleAdmin Role = "admin"
	// RoleLeader 负责人。业务侧的查看与调度，管理采访点，可导出数据。
	RoleLeader Role = "leader"
	// RolePreProduction 前期。素材与采访点准备，管理采访点。
	RolePreProduction Role = "pre_production"
	// RoleLogistics 后勤。设备与场地协调，只读为主。
	RoleLogistics Role = "logistics"
	// RoleDirector 导播。操作被分配项目的切台与上报。
	RoleDirector Role = "director"
)

// 角色等级。数字越大权限越高。
//
// 为什么用等级而不是集合：新增角色时只要给它一个等级，它自动获得所有更低
// 等级接口的访问权，不必回到每个 RequireRole 调用点把名字补一遍——那正是
// 加超级管理员时最容易漏的地方。
const (
	levelUnknown       = 0
	levelDirector      = 10
	levelLogistics     = 20
	levelPreProduction = 30
	levelLeader        = 40
	levelAdmin         = 50
	levelSuperAdmin    = 60
)

var roleLevels = map[Role]int{
	RoleDirector:      levelDirector,
	RoleLogistics:     levelLogistics,
	RolePreProduction: levelPreProduction,
	RoleLeader:        levelLeader,
	RoleAdmin:         levelAdmin,
	RoleSuperAdmin:    levelSuperAdmin,
}

var roleLabels = map[Role]string{
	RoleSuperAdmin:    "超级管理员",
	RoleAdmin:         "管理员",
	RoleLeader:        "负责人",
	RolePreProduction: "前期",
	RoleLogistics:     "后勤",
	RoleDirector:      "导播",
}

// allRoles 按权限从高到低排列，用于前端下拉与校验。
var allRoles = []Role{
	RoleSuperAdmin,
	RoleAdmin,
	RoleLeader,
	RolePreProduction,
	RoleLogistics,
	RoleDirector,
}

// AllRoles 返回全部合法角色，按权限从高到低。
func AllRoles() []Role {
	out := make([]Role, len(allRoles))
	copy(out, allRoles)
	return out
}

// Level 返回角色等级。未知角色为 0，即任何守卫都会拒绝。
func (r Role) Level() int {
	if l, ok := roleLevels[r]; ok {
		return l
	}
	return levelUnknown
}

// Valid 报告角色是否在合法集合内。
//
// 注意 DB 里 users.role 是 varchar(20) 且没有 CHECK 约束，历史数据或绕过
// 本项目的写入都可能塞进别的字符串，所以任何接受外部 role 的入口都必须先过
// 这一关，不能只看等级（未知角色的等级是 0，自然会被守卫拦下，但参数校验
// 需要的是一条能直接返回给用户的错误信息）。
func (r Role) Valid() bool {
	_, ok := roleLevels[r]
	return ok
}

// AtLeast 报告本角色是否达到要求的最低等级。
func (r Role) AtLeast(min Role) bool {
	return r.Valid() && r.Level() >= min.Level()
}

// Label 返回角色的中文名。未知角色原样返回，便于排查脏数据。
func (r Role) Label() string {
	if l, ok := roleLabels[r]; ok {
		return l
	}
	return string(r)
}

// IsPrivileged 报告该角色是否可以管理用户与项目。
func (r Role) IsPrivileged() bool {
	return r.AtLeast(RoleAdmin)
}
