package controllers

import (
	"testing"

	"smart-mzcmc/app/models"
)

func user(id uint, role models.Role) models.User {
	return models.User{ID: id, Username: "u" + string(rune('0'+id)), Role: string(role)}
}

// decideRoleChange / decideDeleteUser 是授权决策的纯逻辑部分。
// 这些用例覆盖的是「谁能操作谁」——一旦这里漏了一条规则，就是一个越权入口。

func TestDecideRoleChange_DeniesEscalation(t *testing.T) {
	// 管理员想把自己或同事升成超级管理员：必须拒绝。
	// 这是最关键的一条——否则「管理员」和「超级管理员」没有区别，
	// 第一次管理员登录就能把自己提上去。
	if err := decideRoleChange(user(1, models.RoleAdmin), user(2, models.RoleDirector),
		models.RoleSuperAdmin); err == nil {
		t.Fatal("管理员授予超管：应拒绝")
	}

	// 超管授予超管：允许。
	if err := decideRoleChange(user(1, models.RoleSuperAdmin), user(2, models.RoleAdmin),
		models.RoleSuperAdmin); err != nil {
		t.Fatalf("超管授予超管：应允许，实际 %v", err)
	}
}

func TestDecideRoleChange_DeniesTouchingSuperior(t *testing.T) {
	// 管理员不能改超管。
	if err := decideRoleChange(user(1, models.RoleAdmin), user(2, models.RoleSuperAdmin),
		models.RoleDirector); err == nil {
		t.Fatal("管理员修改超管：应拒绝")
	}

	// 负责人不能改管理员（等级更高）。
	if err := decideRoleChange(user(1, models.RoleLeader), user(2, models.RoleAdmin),
		models.RoleDirector); err == nil {
		t.Fatal("负责人修改管理员：应拒绝")
	}

	// 但可以改比自己低的。
	if err := decideRoleChange(user(1, models.RoleAdmin), user(2, models.RoleLogistics),
		models.RoleLeader); err != nil {
		t.Fatalf("管理员修改后勤：应允许，实际 %v", err)
	}
}

func TestDecideRoleChange_DeniesSelfDemotion(t *testing.T) {
	// 自己把自己降权 -> 拒绝（否则会把自己锁在门外）
	if err := decideRoleChange(user(1, models.RoleAdmin), user(1, models.RoleAdmin),
		models.RoleDirector); err == nil {
		t.Fatal("管理员自降为导播：应拒绝")
	}

	// 超管自降为管理员同样拒绝：卸任这种动作应该由另一位超管来做，
	// 这样「最后一个超管」的判断始终发生在被别人执行的那一侧。
	if err := decideRoleChange(user(1, models.RoleSuperAdmin), user(1, models.RoleSuperAdmin),
		models.RoleAdmin); err == nil {
		t.Fatal("超管自降为管理员：应拒绝")
	}

	// 把自己降到同等级 -> 允许（不是降权）
	if err := decideRoleChange(user(1, models.RoleAdmin), user(1, models.RoleAdmin),
		models.RoleAdmin); err != nil {
		t.Fatalf("管理员设为同等级：应允许，实际 %v", err)
	}
}

// 等级高低决定的是「能用哪些功能」，不是「能管人」。
// 改角色、删账号都要求管理员及以上：否则负责人能自己升成管理员，
// 后勤能删掉导播账号。这条是写测试时才发现的漏洞。
func TestDecideRoleChange_RequiresAdminTier(t *testing.T) {
	for _, actor := range []models.Role{
		models.RoleLeader, models.RolePreProduction,
		models.RoleLogistics, models.RoleDirector,
	} {
		if err := decideRoleChange(user(1, actor), user(2, models.RoleDirector),
			models.RoleLogistics); err == nil {
			t.Errorf("%s 调整角色：应拒绝（等级低于管理员）", actor)
		}
	}
}

func TestDecideDeleteUser_Rules(t *testing.T) {
	cases := []struct {
		name   string
		actor  models.Role
		target models.Role
		allow  bool
	}{
		{"超管删超管", models.RoleSuperAdmin, models.RoleSuperAdmin, true},
		{"超管删管理员", models.RoleSuperAdmin, models.RoleAdmin, true},
		{"超管删导播", models.RoleSuperAdmin, models.RoleDirector, true},
		{"管理员删管理员", models.RoleAdmin, models.RoleAdmin, true},
		{"管理员删后勤", models.RoleAdmin, models.RoleLogistics, true},
		{"管理员删超管", models.RoleAdmin, models.RoleSuperAdmin, false},
		{"负责人删后勤", models.RoleLeader, models.RoleLogistics, false},
		{"后勤删导播", models.RoleLogistics, models.RoleDirector, false},
		{"前期删负责人", models.RolePreProduction, models.RoleLeader, false},
		{"导播删导播", models.RoleDirector, models.RoleDirector, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := decideDeleteUser(user(1, c.actor), user(2, c.target))
			if c.allow && err != nil {
				t.Fatalf("应允许，实际被拒: %v", err)
			}
			if !c.allow && err == nil {
				t.Fatal("应拒绝，实际允许")
			}
		})
	}
}

func TestDecideDeleteUser_RequiresAdminTier(t *testing.T) {
	// 等级高不代表能管人。下面这些角色都低于管理员，一律不得删账号，
	// 哪怕对方比自己等级还低。
	for _, actor := range []models.Role{
		models.RoleLeader, models.RolePreProduction,
		models.RoleLogistics, models.RoleDirector,
	} {
		if err := decideDeleteUser(user(1, actor), user(2, models.RoleDirector)); err == nil {
			t.Errorf("%s 删除导播账号：应拒绝（等级低于管理员）", actor)
		}
	}
}

func TestDecideDeleteUser_DeniesSelf(t *testing.T) {
	// 超管也不能删自己——否则一旦删掉，就再没有超管。
	if err := decideDeleteUser(user(1, models.RoleSuperAdmin), user(1, models.RoleSuperAdmin)); err == nil {
		t.Fatal("删除自己：应拒绝")
	}
}

func TestGuardGrant_MatchesRoleLevels(t *testing.T) {
	cases := []struct {
		actor models.Role
		grant models.Role
		allow bool
	}{
		{models.RoleSuperAdmin, models.RoleSuperAdmin, true},
		{models.RoleSuperAdmin, models.RoleDirector, true},
		{models.RoleAdmin, models.RoleAdmin, true},
		{models.RoleAdmin, models.RoleDirector, true},
		{models.RoleAdmin, models.RoleSuperAdmin, false},
		{models.RoleLeader, models.RoleAdmin, false},
		{models.RoleLogistics, models.RoleLogistics, true},
		{models.RoleLogistics, models.RoleDirector, true},
		{models.RoleDirector, models.RoleLogistics, false},
	}
	for _, c := range cases {
		got := guardGrant(user(1, c.actor), c.grant)
		if c.allow != (got == nil) {
			t.Errorf("%s 授予 %s：期望 allow=%v，实际 err=%v", c.actor, c.grant, c.allow, got)
		}
	}
}

func TestRoleLevelOrdering(t *testing.T) {
	// 等级顺序必须严格递减，否则中间件会放错人。
	order := []models.Role{
		models.RoleSuperAdmin,
		models.RoleAdmin,
		models.RoleLeader,
		models.RolePreProduction,
		models.RoleLogistics,
		models.RoleDirector,
	}
	for i := 0; i+1 < len(order); i++ {
		if order[i].Level() <= order[i+1].Level() {
			t.Errorf("%s(%d) 应高于 %s(%d)",
				order[i], order[i].Level(), order[i+1], order[i+1].Level())
		}
		if !order[i].AtLeast(order[i+1]) {
			t.Errorf("%s 应满足 AtLeast(%s)", order[i], order[i+1])
		}
		if order[i+1].AtLeast(order[i]) {
			t.Errorf("%s 不应满足 AtLeast(%s)", order[i+1], order[i])
		}
	}
}

func TestRoleInvalidRejected(t *testing.T) {
	// users.role 是 varchar(20) 且无 CHECK 约束，脏数据可能出现。
	// 非法角色等级为 0，任何守卫都必须拒绝它——包括「权限不低于自己」这条，
	// 否则 0 >= 0 会把脏数据当成最高权限之外的什么都不是，然后被放行。
	bad := models.Role("hacker")
	if bad.Valid() {
		t.Error("未知角色不应合法")
	}
	if bad.Level() != 0 {
		t.Errorf("未知角色等级应为 0，实际 %d", bad.Level())
	}
	if bad.AtLeast(models.RoleDirector) {
		t.Error("未知角色不应满足任何等级门槛")
	}
	if bad.Label() != string(bad) {
		t.Errorf("未知角色标签应原样返回以便排查，实际 %q", bad.Label())
	}

	// 全角色集合里不能有非法项
	for _, r := range models.AllRoles() {
		if !r.Valid() {
			t.Errorf("AllRoles 含有非法角色 %q", r)
		}
		if r.Label() == "" || r.Label() == string(r) {
			t.Errorf("角色 %q 缺少中文标签（得到 %q）", r, r.Label())
		}
	}
}

func TestRoleSuperAdminGetsAdminRoutes(t *testing.T) {
	// 这次重构的核心保证：路由守卫是「等级制」而不是「等值比较」，
	// 所以超管自动获得管理员接口访问权，不需要在每个调用点补名字。
	if !models.RoleSuperAdmin.AtLeast(models.RoleAdmin) {
		t.Fatal("超管必须自动具备管理员权限")
	}
	if !models.RoleAdmin.AtLeast(models.RoleLeader) {
		t.Fatal("管理员必须自动具备负责人权限")
	}
	if models.RoleDirector.AtLeast(models.RoleLeader) {
		t.Fatal("导播不应具备负责人权限")
	}
}
