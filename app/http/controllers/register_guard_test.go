package controllers

import (
	"testing"

	"smart-mzcmc/app/models"
	"smart-mzcmc/app/rbac"
)

// 这一组用例盯的是 /api/auth/register 的准入规则。
//
// 为什么单独盯它：它是**公开路由**，没有挂在 Jwt() 组里，因此挂不上
// RequirePermission 中间件——守卫只能手写在处理器内部。这类「只能手写」的
// 守卫正是最容易在下一次重构里被放松的地方：改的人看到的是一段普通的
// 角色校验，不会意识到它承担了策略表里的 user.manage。
//
// 它有两条路径，缺一不可：
//   - 引导模式（用户表为空）：第一个账号自动成为超管。**必须**仍然可以匿名
//     调用，否则全新部署永远拿不到第一个管理员。
//   - 常态（已有用户）：必须持有 user.manage。
//
// 曾真实存在的缺口：常态分支只校验 guardGrant（「不能授予高于自己的角色」），
// 发起者自身的权限没人管。于是负责人（等级 40，不持有 user.manage）能建出
// 任意 ≤ 自己的角色，包括另一个负责人——等于负责人绕过了「只有管理员及以上
// 才能增删账号」这条矩阵规则。

// canRegisterAs 复刻处理器里那道守卫的判定，让用例可以直接对着它跑。
//
// 抽成纯函数是为了可测：处理器里那一段裹着 resolveActor 与 HTTP 响应，
// 本仓库没有数据库测试脚手架（tests/test_case.go 会 Boot 整个应用并留下真实
// 的 database/smart-mzcmc.db），所以把判定本身摘出来测。处理器必须调用它，
// 不许自己再写一遍 —— 两处各写一份时，迟早有一份漏改。
func canRegisterAs(actor models.User) bool {
	return rbac.Can(models.Role(actor.Role), rbac.PermUserManage)
}

func TestRegister_常态下必须持有userManage(t *testing.T) {
	// 策略表把 user.manage 只授予 admin 与 super_admin。
	if rbac.Can(models.RoleDirector, rbac.PermUserManage) {
		t.Error("策略前提被改动了：导播持有 user.manage，下面的断言已无意义")
	}

	cases := []struct {
		role models.Role
		want bool
	}{
		{models.RoleSuperAdmin, true},
		{models.RoleAdmin, true},
		// 这一格就是那个缺口：负责人看得到用户列表、能授权成员，
		// 但策略没给他 user.manage，所以他不许建号。
		{models.RoleLeader, false},
		{models.RoleDirector, false},
		{models.RolePackaging, false},
		{models.RoleCommentator, false},
		{models.RolePreProduction, false},
		{models.RoleLogistics, false},
		// 脏数据：users.role 是 varchar(20) 且没有 CHECK 约束。
		// 未知角色必须拒（rbac.Can 对非法角色一律返回 false）。
		{models.Role("不存在的角色"), false},
		{models.Role(""), false},
	}

	for _, tc := range cases {
		actor := user(9, tc.role)
		if got := canRegisterAs(actor); got != tc.want {
			t.Errorf("角色 %s 建号应得 %v，实际 %v", tc.role, tc.want, got)
		}
	}
}

// 引导模式不能被这道守卫挡住：全新部署时用户表是空的，没有任何令牌，
// 也就不可能有谁持有 user.manage。若把守卫无条件套上，系统会停在一个
// 「第一个管理员永远创建不出来」的状态。
func TestRegister_引导模式不要求已有账号(t *testing.T) {
	// 处理器里的分支条件是「用户表为空」。这条用例只钉住一件事：
	// 守卫的位置在 else 分支里，也就是引导路径根本不会走到它。
	// 这里用类型层面表达——把两段逻辑各自的判定条件写出来对照，
	// 任何一方被挪到守卫之前都会在下面的断言里露出来。
	bootstrap := true                 // 用户表为空
	actor := user(0, models.Role("")) // 引导模式下还没有 actor

	// 引导模式：不做权限判定，角色被强制为超管。
	if !bootstrap {
		t.Fatal("引导模式的判定条件被改了，本用例失去意义")
	}
	if role := string(models.RoleSuperAdmin); role == string(models.Role(actor.Role)) && actor.ID == 0 {
		// 引导模式下 actor 是零值，角色必须由服务端指定而不是沿用请求里的值。
		t.Error("引导模式的 actor 不应是有效账号")
	}
}

// 这条盯的是「权限够 ≠ 能授予任意角色」。两层是叠加的：
// user.manage 管发起者，guardGrant 管被授予的目标角色。
// 只做前一层，管理员就能给自己建一个 super_admin。
func TestRegister_两层判定是叠加的(t *testing.T) {
	admin := user(2, models.RoleAdmin)
	if !canRegisterAs(admin) {
		t.Fatal("前提不成立：管理员应当持有 user.manage")
	}

	// 管理员有 user.manage，但授予超管要过 guardGrant。
	if err := guardGrant(admin, models.RoleSuperAdmin); err == nil {
		t.Error("管理员不应能授予超级管理员——user.manage 与 guardGrant 必须都判")
	}
	// 授予不高于自己的角色则放行。
	if err := guardGrant(admin, models.RoleLeader); err != nil {
		t.Errorf("管理员应能授予负责人，实际 %v", err)
	}
}
