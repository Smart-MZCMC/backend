package rbac

import (
	"strings"
	"testing"

	"smart-mzcmc/app/models"
)

// 受保护不变量是这套权限系统里**唯一几条不能靠配置补救**的规则。
// 别的规则错了，症状是「某人看不到某个功能」；这两条错了，症状是
// 「所有管理员失去系统维护权限，而且现场没有任何界面能改回来」——
// 只能 SSH 手改数据库。
//
// 所以这几条用例的重点不是「现在对不对」，而是**守卫本身不许被悄悄摘掉**。

// TestProtected_系统维护权限只授予超级管理员 是需求里点名要的那条。
//
// 断言走 GetPermissionsForUser 而不是 Can：Can 只回答「能不能」，而这里要
// 回答的是「策略里到底有没有这一行」。两者不一样——一个不存在的权限和一个
// 存在的权限，Can 都会返回 false，但前者连「谁持有它」都答不上来，而正是
// 这个差别决定了将来写入路径能不能拿它做校验。
func TestProtected_系统维护权限只授予超级管理员(t *testing.T) {
	const perm = PermSystemMaintain

	others := []models.Role{
		models.RoleAdmin, models.RoleLeader, models.RoleDirector, models.RolePackaging,
		models.RoleCommentator, models.RolePreProduction, models.RoleLogistics,
	}
	for _, role := range others {
		t.Run(string(role), func(t *testing.T) {
			for _, p := range permissionsOf(t, role) {
				if p == perm {
					t.Errorf("%s 不该持有受保护权限 %s——"+
						"在线更新会替换服务自身的可执行文件并重启进程，"+
						"这条权限必须只在最高角色手里", role.Label(), perm)
				}
			}
		})
	}

	// 反方向也要断：超级管理员必须持有它。
	//
	// 少了这条，守卫会「正确地」拒绝一切，同时所有人都不持有它——
	// 于是系统照样锁死，而且守卫还会说「一切正常」。这两种坏法症状一样，
	// 但只有这条用例能分开它们。
	held := false
	for _, p := range permissionsOf(t, models.RoleSuperAdmin) {
		if p == perm {
			held = true
		}
	}
	if !held {
		t.Fatalf("超级管理员必须持有 %s：没有人持有它时，系统就锁死了", perm)
	}
}

// TestProtected_受保护清单本身被盯住 防的是「把守卫从清单里删掉」。
//
// 这是整条守卫最容易失效的方式：有人遇到麻烦，正确的做法是改 policy.csv，
// 错误的做法是改这份清单——而后者**不会有任何用例失败**，因为所有断言
// 都是「清单里有的要成立」，清单空了它们就自动全过。
//
// 所以这里除了「system.maintain 必须在清单里」，还要断反方向：
// 清单里**不许**多出别的条目。多出来意味着有人为了图省事把一堆权限
// 标成受保护（在线编辑界面会变成一大片灰色，而且没人说得清为什么）。
func TestProtected_受保护清单本身被盯住(t *testing.T) {
	if !IsProtected(PermSystemMaintain) {
		t.Error("system.maintain 必须是受保护权限——把它从清单里删掉，" +
			"将来第一个勾掉它的人就能把系统锁死，且没有任何测试会发现")
	}
	if !IsProtectedRole(models.RoleSuperAdmin) {
		t.Error("super_admin 必须是受保护角色——它是「系统维护权限只属于最高角色」" +
			"这条规则唯一的受益者，删掉它整条守卫就空转了")
	}

	// 受保护权限：只有 system.maintain。audit.view 刻意不在其中
	// （已确认：操作审计保持管理员及以上，不开放给负责人）。
	for _, p := range AllPermissions() {
		want := p == PermSystemMaintain
		if got := IsProtected(p); got != want {
			t.Errorf("权限 %s 的受保护状态：实际 %v，期望 %v", p, got, want)
		}
	}

	// 受保护角色：只有 super_admin。
	for _, r := range models.AllRoles() {
		want := r == models.RoleSuperAdmin
		if got := IsProtectedRole(r); got != want {
			t.Errorf("角色 %s(%s) 的受保护状态：实际 %v，期望 %v",
				r, r.Label(), got, want)
		}
	}

	// 两个清单都非空，否则「受保护」这个词就没有意义了。
	if len(ProtectedPermissions()) != 1 {
		t.Errorf("受保护权限清单应为 1 项，实际 %v", ProtectedPermissions())
	}
	if len(ProtectedRoles()) != 1 {
		t.Errorf("受保护角色清单应为 1 项，实际 %v", ProtectedRoles())
	}
}

// TestProtected_授予即拒绝 是需求里点名要的那条纯函数用例。
//
// 「把 system.maintain 授予非超管」必须被拒绝。注意它测的是**判定层**，
// 而不是 Enforce：Enforce 对 admin + system.maintain 返回 false 只是
// 「现在还没授予」，一旦有人授予了就变成 true——那一格没有任何静态检查能挡住。
// 能挡住的只有写入之前必须问的那一句，也就是 ValidateGrant。
func TestProtected_授予即拒绝(t *testing.T) {
	others := []models.Role{
		models.RoleAdmin, models.RoleLeader, models.RoleDirector, models.RolePackaging,
		models.RoleCommentator, models.RolePreProduction, models.RoleLogistics,
	}
	for _, role := range others {
		err := ValidateGrant(role, PermSystemMaintain)
		if err == nil {
			t.Errorf("把 %s 授予 %s：应拒绝", PermSystemMaintain, role.Label())
			continue
		}
		// 错误信息要能直接贴给点按钮的那个人看。
		if !IsProtectedError(err) {
			t.Errorf("%s 的错误应标记为受保护类，实际 %v", role.Label(), err)
		}
		if !strings.Contains(err.Error(), PermSystemMaintain) {
			t.Errorf("错误信息应说明是哪个权限被拒，实际 %q", err)
		}
		if !strings.Contains(err.Error(), role.Label()) {
			t.Errorf("错误信息应说明是谁不能拿，实际 %q", err)
		}
	}

	// 超管自己必须能拿——否则守卫就成了「谁都别想维护系统」。
	if err := ValidateGrant(models.RoleSuperAdmin, PermSystemMaintain); err != nil {
		t.Errorf("超级管理员应被允许持有 %s，实际 %v", PermSystemMaintain, err)
	}
}

// TestProtected_撤销受保护权限一律拒绝 防的是锁死最容易发生的那一格。
//
// 只挡住「别人能不能拿」而允许「持有者自己放手」，锁死照样会发生——
// 而且这一格最容易被想漏，因为它长得像「超级管理员自己调低自己的权限」，
// 看上去像一次合理的降权操作。
func TestProtected_撤销受保护权限一律拒绝(t *testing.T) {
	err := ValidateRevoke(models.RoleSuperAdmin, PermSystemMaintain)
	if err == nil {
		t.Fatal("从超级管理员自己身上撤销 system.maintain：应拒绝。" +
			"一旦撤销，所有管理员立刻失去系统维护权限，且没有任何界面能改回来")
	}
	if !IsProtectedError(err) {
		t.Errorf("错误应标记为受保护类，实际 %v", err)
	}
	if !strings.Contains(err.Error(), PermSystemMaintain) {
		t.Errorf("错误信息应说明是哪个权限，实际 %q", err)
	}

	// 连不存在的角色也要走到「受保护」那一格之前——因为受保护权限
	// 无论挂在谁身上都不能动，角色存不存在改变不了这个结论。
	if err := ValidateRevoke(models.Role("hacker"), PermSystemMaintain); err == nil {
		t.Error("非法角色撤销受保护权限也应报错")
	}
}

// TestProtected_移除受保护角色一律拒绝 挡住第 1 条守卫失效的那个前提。
//
// super_admin 被整体移除之后，system.maintain 仍然「只允许超管持有」，
// 却没有任何人持有——守卫照样通过，系统照样锁死。所以角色清单本身也要受保护。
func TestProtected_移除受保护角色一律拒绝(t *testing.T) {
	err := ValidateRemoveRole(models.RoleSuperAdmin)
	if err == nil {
		t.Fatal("移除超级管理员：应拒绝")
	}
	if !IsProtectedError(err) {
		t.Errorf("错误应标记为受保护类，实际 %v", err)
	}
	if !strings.Contains(err.Error(), models.RoleSuperAdmin.Label()) {
		t.Errorf("错误信息应说明是哪个角色，实际 %q", err)
	}

	// 反方向：普通角色当然可以移除，否则这条守卫就是「谁都不许改」。
	for _, role := range []models.Role{
		models.RoleAdmin, models.RoleLeader, models.RoleDirector, models.RolePackaging,
		models.RoleCommentator, models.RolePreProduction, models.RoleLogistics,
	} {
		if err := ValidateRemoveRole(role); err != nil {
			t.Errorf("移除 %s 应允许，实际 %v", role.Label(), err)
		}
	}
}

// TestValidate_普通授权与撤销仍然放行 防的是守卫把功能锁死。
//
// 一条把所有写入都拒掉的守卫，等于没有守卫：在线编辑界面会被整个废掉，
// 于是有人会绕过它直接改数据库——那正是我们要防的事。
func TestValidate_普通授权与撤销仍然放行(t *testing.T) {
	cases := []struct {
		perm string
		role models.Role
	}{
		{PermProjectMember, models.RoleDirector}, // 迁移矩阵里导演并没有这项
		{PermLogExport, models.RoleLeader},
		{PermUserView, models.RoleLogistics},
		{PermSwitchOperate, models.RoleAdmin},
		{PermLogView, models.RoleCommentator},
	}
	for _, c := range cases {
		if err := ValidateGrant(c.role, c.perm); err != nil {
			t.Errorf("授予 %s 给 %s：应允许，实际 %v", c.perm, c.role.Label(), err)
		}
		if err := ValidateRevoke(c.role, c.perm); err != nil {
			t.Errorf("撤销 %s 从 %s：应允许，实际 %v", c.perm, c.role.Label(), err)
		}
	}
}

// TestValidate_未知角色与未知权限一律拒绝 写入路径也要 fail-closed。
//
// 这一条与「受保护」无关，纯粹是兜底：写入路径拿到一个不认识的角色或权限，
// 那是 bug，绝不能当成「那就不受限制了」。
//
// 特别要挡住「判断某项权限受不受保护」这件事本身被跳过：将来如果实现成
// `if IsProtected(perm) && !isProtectedRole(role) { ... }` 这种写法，
// 权限名不认识时 IsProtected 返回 false，整条守卫就绕过去了。
func TestValidate_未知角色与未知权限一律拒绝(t *testing.T) {
	if err := ValidateGrant(models.Role("hacker"), PermSystemMaintain); err == nil {
		t.Error("给不存在的角色授权：应拒绝")
	} else if IsProtectedError(err) {
		t.Error("未知角色应报「角色不存在」而不是「受保护」——分类错了界面会误提示")
	}

	if err := ValidateGrant(models.RoleAdmin, "system.maintaint"); err == nil {
		t.Error("授予不存在的权限：应拒绝")
	} else if IsProtectedError(err) {
		t.Error("未知权限应报「权限不存在」而不是「受保护」——分类错了界面会误提示")
	}

	if err := ValidateRevoke(models.RoleAdmin, "project.memberr"); err == nil {
		t.Error("撤销不存在的权限：应拒绝")
	}
	if err := ValidateRemoveRole(models.Role("hacker")); err == nil {
		t.Error("移除不存在的角色：应拒绝")
	}
	if err := ValidateGrant(models.RoleAdmin, ""); err == nil {
		t.Error("空权限名：应拒绝")
	}
	if err := ValidateRevoke(models.RoleLogistics, ""); err == nil {
		t.Error("空权限名：应拒绝")
	}
}

// TestPolicyError_分类可被界面判别 是给在线编辑那一层留的接口。
//
// 界面要用它决定「这一格画成禁用」还是「提示已过期，请刷新」。
// 如果拿不到可判别的类别，它只能对所有错误弹同一句话，于是受保护的那类
// 会被当成「你选的东西过期了」，用户刷新一百次也刷不出来。
func TestPolicyError_分类可被界面判别(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"授予受保护权限给普通角色",
			ValidateGrant(models.RoleAdmin, PermSystemMaintain), true},
		{"撤销受保护权限",
			ValidateRevoke(models.RoleSuperAdmin, PermSystemMaintain), true},
		{"移除受保护角色", ValidateRemoveRole(models.RoleSuperAdmin), true},
		{"未知角色", ValidateGrant(models.Role("hacker"), PermLogView), false},
		{"未知权限", ValidateGrant(models.RoleAdmin, "nope"), false},
		{"无错误", nil, false},
	}
	for _, c := range cases {
		if got := IsProtectedError(c.err); got != c.want {
			t.Errorf("%s：IsProtectedError=%v，期望 %v", c.name, got, c.want)
		}
		if c.err == nil {
			continue
		}
		if c.err.Error() == "" {
			t.Errorf("%s：错误信息是空串", c.name)
		}
	}
}

// TestPolicyError_受保护错误要说清为什么 是给人看的，不是给机器看的。
//
// 界面只会把 Error() 原样弹出来。一句「不允许」会让点按钮的人以为是系统坏了，
// 于是去找管理员要密码——而正确做法是压根不让他点。
func TestPolicyError_受保护错误要说清为什么(t *testing.T) {
	err := ValidateGrant(models.RoleAdmin, PermSystemMaintain)
	msg := err.Error()
	for _, want := range []string{
		PermSystemMaintain,            // 哪一项权限
		models.RoleAdmin.Label(),      // 谁不能拿
		models.RoleSuperAdmin.Label(), // 谁能拿
		"重启",                          // 为什么不能给
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("受保护错误信息应包含 %q，实际 %q", want, msg)
		}
	}
}
