package models

// Role 是用户的系统角色。
//
// 角色名是 app/rbac 策略（policy.csv）里的主键，改这里的名字必须同步改那份
// 策略，否则该角色的所有具名权限会静悄悄地全部失效（策略自检会在启动日志里
// 报「未知角色」，rbac 的测试也会直接红）。
//
// 这里同时带一组等级常量，但**等级不再是路由准入的依据**：路由守卫已全部
// 换成 app/rbac 的具名权限（见 routes/web.go），等级只回答「能不能操作
// 某个人」。早期版本的注释写着「让权限判断只认等级」——那句话已经不成立了。
// 见下方 levelUnknown 一组常量的说明与 app/rbac 的包注释。
type Role string

const (
	// RoleSuperAdmin 超级管理员。独占：系统在线更新、管理其他超级管理员。
	RoleSuperAdmin Role = "super_admin"
	// RoleAdmin 管理员。用户、项目、权限分配、日志导出与清理。
	RoleAdmin Role = "admin"
	// RoleLeader 负责人。查看与调度、管理采访点、可导出数据、授权项目成员，
	// 但**不参与导播工作**——等级高不等于能碰切台，那道门是独立的
	// switch.operate 权限（见 app/rbac/policy.csv）。
	RoleLeader Role = "leader"
	// RoleDirector 导播。操作被分配项目的切台与上报。
	RoleDirector Role = "director"
	// RolePackaging 包装。包装端客户端的登录身份，只订阅与展示。
	// 权限面比解说端宽一档：它要在自己的设置里切项目、读写本地配置。
	RolePackaging Role = "packaging"
	// RoleCommentator 解说。解说端客户端的登录身份，只订阅与展示。
	RoleCommentator Role = "commentator"
	// RolePreProduction 前期。素材与采访点准备。与解说同档——
	// 两者都只消费现场产生的数据，权限面相同。
	RolePreProduction Role = "pre_production"
	// RoleLogistics 后勤。设备与场地协调，只读为主。最低档。
	RoleLogistics Role = "logistics"
)

// 角色等级。数字越大权限越高。
//
// ⚠️ 等级**不再决定能不能进某个接口**。路由守卫已全部换成 app/rbac 的具名
// 权限（见 routes/web.go），等级只回答两件事：
//   - 「能不能操作某个人」——controllers/authz.go 的 decideRoleChange、
//     decideDeleteUser、guardGrant；
//   - 杂项判断（IsPrivileged 等）。
//
// 为什么等级当初被引入，又为什么现在退居二线：等级能表达「高低」，表达不了
// 「负责人能看、不能改」——负责人 40 级比导播 30 还高，任何 <= 40 的门槛都
// 放他进来。把等级当准入的另一个代价是「漏改一处不会报错」：新增角色自动
// 继承所有更低等级的接口，于是新角色往往在没人想过的情况下拿到了不该有的
// 能力。具名权限的显式清单把这件事摆到纸面上，漏补一行的后果是该角色少
// 一项能力（立刻有人喊），而不是多一项（没人喊）。
//
// 这套梯子曾把导播放在最低（10）、后勤 20，反而比导播高。重排后的定位：
// 导播是现场唯一真正操作设备的人，排在管理员之下第一档；后勤退到最低。
//
// 为什么不连续（30/25/20/10）：留出的空档是给将来加角色的，避免每次都要
// 重新给整条梯子编号——而重编号一旦漏改某个判断，权限就会静悄悄变掉。
//
// 导播这一档唯一的专属入口是切台，那道门是 switch.operate 权限
// （app/rbac/policy.csv），**不依赖等级**——等级表达不了「导播能、负责人
// 不能」这种形状，而它是业务硬要求（负责人不参与导播工作）。
const (
	levelUnknown       = 0
	levelLogistics     = 10
	levelPreProduction = 20
	levelCommentator   = 20
	levelPackaging     = 25
	levelDirector      = 30
	levelLeader        = 40
	levelAdmin         = 50
	levelSuperAdmin    = 60
)

var roleLevels = map[Role]int{
	RoleLogistics:     levelLogistics,
	RolePreProduction: levelPreProduction,
	RoleCommentator:   levelCommentator,
	RolePackaging:     levelPackaging,
	RoleDirector:      levelDirector,
	RoleLeader:        levelLeader,
	RoleAdmin:         levelAdmin,
	RoleSuperAdmin:    levelSuperAdmin,
}

var roleLabels = map[Role]string{
	RoleSuperAdmin:    "超级管理员",
	RoleAdmin:         "管理员",
	RoleLeader:        "负责人",
	RoleDirector:      "导播",
	RolePackaging:     "包装",
	RoleCommentator:   "解说",
	RolePreProduction: "前期",
	RoleLogistics:     "后勤",
}

// allRoles 按权限从高到低排列，用于前端下拉与校验。
var allRoles = []Role{
	RoleSuperAdmin,
	RoleAdmin,
	RoleLeader,
	RoleDirector,
	RolePackaging,
	RoleCommentator,
	RolePreProduction,
	RoleLogistics,
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
//
// 语义是「等级 >= 最低要求」，不是「角色等于最低要求」。所以：
//   - 更高等级的角色自动获得低等级接口的访问权；
//   - 同等级的不同角色**互相也满足**——解说与前期同为 20，
//     于是 AtLeast(RoleCommentator) 会放行前期账号。这是有意的：
//     两者权限面本来就一样，谁当门槛都无所谓。
//
// ⚠️ 现在只用它回答「能不能操作某个人」，别再用它做接口准入。
//
// 推论：凡是要「只允许某一个角色」的判断，不能用 AtLeast 表达——
// 切台守卫就是活例子，leader 等级高于 director 却不能抢锁。
// 权限迁移之前这道门是单独写的白名单，现在它是 app/rbac 的 switch.operate。
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
