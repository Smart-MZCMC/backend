package rbac

import (
	"io"
	"log"
	"strings"
	"testing"

	"smart-mzcmc/app/models"
)

// 这里是本次「等级门槛 → 具名权限」迁移的**规范**。
//
// expectations 逐格写死了设计意图：每一项权限授予哪些角色。下面的矩阵测试
// 拿它去对真实的 Casbin 逐格断言，而不是只断言「跑起来没报错」——
// 后者只能证明策略能被解析，证明不了「负责人到底能干什么」与设计一致。
// 这张表就是验收证据。

// expectation 是一项权限及其持有者。
type expectation struct {
	perm string
	// holders 授予该权限的角色。空数组意味着「没有任何角色可执行」，
	// 但那条状态本身是个错误——策略自检里另有一条用例盯着它。
	holders []models.Role
}

// allRolesEight 当前全部 8 个角色。「所有角色」的字面意思。
var allRolesEight = []models.Role{
	models.RoleSuperAdmin, models.RoleAdmin, models.RoleLeader, models.RoleDirector,
	models.RolePackaging, models.RoleCommentator, models.RolePreProduction,
	models.RoleLogistics,
}

var expectations = []expectation{
	{
		perm: PermLogView,
		// 全员。缺任何一个，那个角色的工作界面就看不到现场发生了什么。
		holders: allRolesEight,
	},
	{
		perm: PermProjectView,
		// 全员。项目是所有人工作的容器，没有人能被挡在「有哪些项目」之外。
		holders: allRolesEight,
	},
	{
		perm: PermUserView,
		// 负责人及以上。
		//
		// ⚠️ 迁移前没有这一项：GET /api/admin/users 和「删账号」挤在同一个
		// RequireRole(RoleAdmin) 组里，于是负责人能授权成员、能管采访点，
		// 却连被授权的人是谁都看不到（点开是 403）。
		holders: []models.Role{models.RoleSuperAdmin, models.RoleAdmin, models.RoleLeader},
	},
	{
		perm: PermAuditView,
		// 管理员及以上。审计是给管理员复盘用的，不是给被审计的人看的。
		holders: []models.Role{models.RoleSuperAdmin, models.RoleAdmin},
	},
	{
		perm: PermLogExport,
		// 负责人及以上。业务侧要看报表导出；导播与后勤用不到，而导出
		// 是能把整个项目历史落成文件带出系统的动作。
		holders: []models.Role{models.RoleSuperAdmin, models.RoleAdmin, models.RoleLeader},
	},
	{
		perm: PermInterviewManage,
		// 前期及以上。负责人在业务上管采访点，管理员与超管兜底。
		holders: []models.Role{
			models.RoleSuperAdmin, models.RoleAdmin, models.RoleLeader,
			models.RolePreProduction,
		},
	},
	{
		perm: PermProjectMember,
		// 负责人及以上。
		//
		// ⚠️ 迁移前是「管理员及以上」。这一格是本次**唯一**的行为降级：
		// 负责人现在也能授权/回收项目成员。依据是业务要求——负责人管排期
		// 必然要调整谁能上哪个项目；等 authz.require_project_membership
		// 打开之后，user_projects 就真的决定谁能读哪个项目，只让管理员改
		// 的话负责人得天天找人代劳。
		holders: []models.Role{models.RoleSuperAdmin, models.RoleAdmin, models.RoleLeader},
	},
	{
		perm: PermSwitchOperate,
		// 导播 + 管理员及以上。**唯一一组不连续的角色**：导播(30)、管理员(50)、
		// 超管(60)，中间空着包装(25)与负责人(40)。
		//
		// 这一格逐字替换掉了迁移前的 RequireSwitchingOperator 白名单
		// （CanOperateSwitching：导播 || AtLeast(管理员)）。集合相同是硬要求——
		// 一旦不等价，删掉那个中间件就等于悄悄改了一道「谁能抢设备控制权」
		// 的门。下面 TestSwitchOperate_与被删掉的白名单逐字等价 专门钉住它。
		holders: []models.Role{models.RoleSuperAdmin, models.RoleAdmin, models.RoleDirector},
	},
	{
		perm: PermLogCleanup,
		// 管理员及以上。清理是真删，而且删的是审计线索本身。
		holders: []models.Role{models.RoleSuperAdmin, models.RoleAdmin},
	},
	{
		perm: PermProjectManage,
		// 管理员及以上。负责人管的是「人上哪个项目」，不是「有哪些项目」。
		holders: []models.Role{models.RoleSuperAdmin, models.RoleAdmin},
	},
	{
		perm: PermUserManage,
		// 管理员及以上。
		//
		// ⚠️ 这一格只回答「能不能进这个接口」。「能不能操作**这个人**」
		// （不能碰同级或更高、不能自降权、不能动最后一个超管）仍然由
		// controllers/authz.go 的 decideRoleChange / decideDeleteUser 判断，
		// 那些 AtLeast **原样保留**。把两者合成一条就是「第一个管理员登录
		// 就能给自己升成超管」。
		holders: []models.Role{models.RoleSuperAdmin, models.RoleAdmin},
	},
	{
		perm: PermSystemMaintain,
		// 仅超管。系统更新会替换服务自身的可执行文件并重启进程。
		holders: []models.Role{models.RoleSuperAdmin},
	},
}

// holderSet 把 expectations 翻成「权限 → 持有角色集合」，测试里到处要用。
func holderSet() map[string]map[models.Role]bool {
	out := make(map[string]map[models.Role]bool, len(expectations))
	for _, e := range expectations {
		set := make(map[models.Role]bool, len(e.holders))
		for _, r := range e.holders {
			set[r] = true
		}
		out[e.perm] = set
	}
	return out
}

// TestPolicy_迁移矩阵逐格等于设计意图 是本次改动最重要的一条用例。
//
// 它对「每一项权限 × 每一个角色」实际调一次 Casbin 的 Enforce，然后断言结果
// 等于 expectations。为什么必须逐格而不是抽查几条：
//   - 只测「负责人不能删账号」这类直觉里的规则，测不出「包装端拿到了
//     project.manage」这种**误授予**——而误授予才是真正危险的那一侧。
//   - 抽查的选取方式本身就是有偏的：人只会去测自己记得的几条。
//
// 96 格全跑的成本是零（策略在内存里），所以没有理由不跑满。
func TestPolicy_迁移矩阵逐格等于设计意图(t *testing.T) {
	if Default() == nil {
		t.Fatal("策略没加载起来，后续所有断言都没意义")
	}
	want := holderSet()

	cells := 0
	for _, e := range expectations {
		for _, role := range models.AllRoles() {
			cells++
			got := Can(role, e.perm)
			expect := want[e.perm][role]
			if got != expect {
				t.Errorf("%s 执行 %s：实际 %v，期望 %v", role.Label(), e.perm, got, expect)
			}
		}
	}
	if cells != len(expectations)*len(allRolesEight) {
		t.Fatalf("矩阵格数不对：跑了 %d 格，期望 %d 格", cells, len(expectations)*len(allRolesEight))
	}
}

// TestPolicy_规范表与已声明权限完全对应 防的是「加了权限忘了更新规范」。
//
// 反方向也要挡住：在规范表里写一个 Go 侧根本没声明的权限名，同样是错的——
// 那说明规范与代码已经脱节，而这张表正是验收依据，不能有一行对不上。
func TestPolicy_规范表与已声明权限完全对应(t *testing.T) {
	declared := make(map[string]bool)
	for _, p := range AllPermissions() {
		declared[p] = true
	}
	specced := make(map[string]bool)
	for _, e := range expectations {
		if !Known(e.perm) {
			t.Errorf("规范表里的 %q 不是已声明的权限（app/rbac 的常量里没有）", e.perm)
		}
		if specced[e.perm] {
			t.Errorf("规范表里 %s 出现了两次", e.perm)
		}
		specced[e.perm] = true
	}
	for _, p := range AllPermissions() {
		if !specced[p] {
			t.Errorf("权限 %s 已声明却没有写进规范表——迁移矩阵必须覆盖它", p)
		}
	}
}

// TestPolicy_每个权限至少被授予一个角色 防的是「写了权限忘了授权」。
//
// 表现是某项能力对所有人 403，现场表现为「这个按钮点了没反应」，而日志里
// 只有一行权限拒绝。这类漏配不会被任何一条「谁能做」的用例发现——它们
// 断言的都是放行方向。
func TestPolicy_每个权限至少被授予一个角色(t *testing.T) {
	for _, perm := range AllPermissions() {
		holders := Holders(perm)
		if len(holders) == 0 {
			t.Errorf("权限 %s（%s）没有任何角色可以执行——多半是 policy.csv 忘了写这一段",
				perm, Label(perm))
		}
	}
}

// TestPolicy_每个角色至少有一个权限 防的是「把某个角色整个写空了」。
//
// 新加角色时最容易犯的错：只在一两个权限下加了它，于是它登录进来什么都
// 看不到，但没有人会去查「为什么这个角色什么都点不了」——现场只会觉得这
// 个账号坏了。这条用例会先一步报出来。
func TestPolicy_每个角色至少有一个权限(t *testing.T) {
	for _, role := range models.AllRoles() {
		perms := permissionsOf(t, role)
		if len(perms) == 0 {
			t.Errorf("角色 %s(%s) 一项权限都没有，新加角色时忘了在 policy.csv 里授权",
				role, role.Label())
		}
	}
}

// TestPolicy_超管拥有全部权限 钉住「最高角色不该有洞」。
//
// 超管是这套系统的兜底账号：所有其他账号都坏了、规则都改错了、策略配错了
// 的时候，它是唯一还能进系统的人。少任何一项权限都等于在某条故障路径上
// 把最后一个逃生口焊死了。
func TestPolicy_超管拥有全部权限(t *testing.T) {
	got := permissionsOf(t, models.RoleSuperAdmin)
	have := make(map[string]bool, len(got))
	for _, p := range got {
		have[p] = true
	}
	for _, perm := range AllPermissions() {
		if !have[perm] {
			t.Errorf("超级管理员缺少权限 %s（%s）", perm, Label(perm))
		}
	}
	if len(got) != len(AllPermissions()) {
		t.Errorf("超管持有 %d 项权限，已声明权限共 %d 项——多的那些是 policy.csv 里的脏行",
			len(got), len(AllPermissions()))
	}
}

// TestPolicy_策略只引用已知角色与已知权限 挡住「策略里打错字」。
//
// 写错的后果是**静默**的：那一行谁也匹配不上，于是少写的那项权限对所有
// 人关闭（表现为某个按钮点了没反应），而 policy.csv 读起来完全正常。
// 启动时 auditPolicy 会打日志，测试里则把它变成硬失败。
func TestPolicy_策略只引用已知角色与已知权限(t *testing.T) {
	rules, err := Default().GetPolicy()
	if err != nil {
		t.Fatalf("读策略失败：%v", err)
	}
	for _, rule := range rules {
		if len(rule) != 2 {
			t.Errorf("策略行字段数不是 2（model.conf 只声明了 sub 与 obj）：%v", rule)
			continue
		}
		if !models.Role(rule[0]).Valid() {
			t.Errorf("策略里出现未知角色 %q（授予权限 %s）", rule[0], rule[1])
		}
		if !Known(rule[1]) {
			t.Errorf("策略里出现未知权限 %q（授予角色 %s）", rule[1], rule[0])
		}
	}
}

// TestCan_未知权限名一律拒绝 防的是「路由上打错了一个字」变成静默失效。
//
// 这条与「策略里没写这一项」是两回事：策略漏写是配置问题，未知权限名是代码
// 问题。它必须被拒绝——如果这里放行，等于任何一处字符串笔误都变成一道
// 没有门的门（虽然现实里没人会因为手写对了假权限名而被放行，但反过来，
// 一旦有人故意传一个假权限名，「未知的都拒绝」是唯一说得通的默认）。
//
// 每一次拒绝都会打一行日志（这是故意的：线上真出现错字时要吵）。这里把
// 日志丢掉，免得 56 行刷屏把用例结果淹掉。
func TestCan_未知权限名一律拒绝(t *testing.T) {
	silenceLog(t)
	bogus := []string{
		"",
		"project",         // 只给了对象，没给动作
		"project.memberr", // 少一个字母（member 的笔误）
		"project.member.*",
		"*",
		"PROJECT.MEMBER",  // 大小写敏感，策略里全小写
		"project.member ", // 带尾随空格
	}
	for _, perm := range bogus {
		for _, role := range models.AllRoles() {
			if Can(role, perm) {
				t.Errorf("%s 执行未知权限 %q 竟然被放行", role.Label(), perm)
			}
		}
	}
	if Known("project.memberr") {
		t.Error("Known 对拼错的权限名返回了 true")
	}
}

// TestCan_未知角色一律拒绝 防的是脏数据被当成合法角色。
//
// users.role 是 varchar(20) 且没有 CHECK 约束，绕过本项目的写入能塞进任何
// 字符串。这类角色等级为 0，在旧的等级制下会被自然拦下；但现在判的是
// 「策略里有没有这一行」，一个没见过的角色名同样匹配不上——两道防线都在，
// 任何一道单独存在就够了，这里两道都要。
func TestCan_未知角色一律拒绝(t *testing.T) {
	silenceLog(t)
	bogus := []models.Role{
		"",
		"hacker",
		"superadmin",   // 少一个下划线
		"ADMIN",        // 大小写敏感
		"super_admin ", // 带尾随空格
		"Leader",
	}
	for _, role := range bogus {
		if role.Valid() {
			t.Errorf("%q 不该是合法角色", role)
		}
		for _, perm := range AllPermissions() {
			if Can(role, perm) {
				t.Errorf("非法角色 %q 执行 %s 竟然被放行", role, perm)
			}
		}
	}
}

// TestPolicy_给不存在的角色加权限不会放行任何人 是 fail-closed 的核心一条。
//
// 场景：有人往 policy.csv 里写了一个不存在的角色（比如把角色名拼错，
// 或者提前为一个还没加进来的角色铺路），并给了它 system.maintain。
// 期望是**任何人都不因此通过**——包括超级管理员。
//
// 这条为什么值得单独写：Casbin 的 matcher 是「请求的两个值都要与某条策略
// 逐字相等」，多出来的策略行只会让「集合更大」，而这里的集合一大，
// 超管就会因为多了这条无关的行而在别的方向上被误判（或者反过来，
// 有人以为写了就生效了）。把它钉成硬失败，才不会等到现场才发现。
func TestPolicy_给不存在的角色加权限不会放行任何人(t *testing.T) {
	poisoned, err := load(modelConf, policyCSV+"\n"+
		"# 下面这一行模拟误配：给一个系统不认识的角色发最高权限。\n"+
		"p, ghost_role, system.maintain\n"+
		"p, hacker, project.manage\n")
	if err != nil {
		t.Fatalf("加载被污染的策略本该成功（多写的行不影响解析）：%v", err)
	}

	for _, role := range models.AllRoles() {
		allowed, err := poisoned.Enforce(string(role), PermSystemMaintain)
		if err != nil {
			t.Fatalf("Enforce 出错：%v", err)
		}
		expect := role == models.RoleSuperAdmin
		if allowed != expect {
			t.Errorf("%s 执行 system.maintain：实际 %v，期望 %v"+
				"（策略里那两行幽灵角色不该影响任何真实角色）",
				role.Label(), allowed, expect)
		}
	}

	// 反方向也要成立：幽灵角色自己进不来。
	if models.Role("ghost_role").Valid() {
		t.Error("ghost_role 不该被当成合法角色")
	}
	if allowed, _ := poisoned.Enforce("ghost_role", PermSystemMaintain); !allowed {
		t.Log("注意：Enforce 按字面比较时 ghost_role 确实匹配到了自己那一行。" +
			"这不影响安全——Can 在调 Enforce 之前先用 Valid() 挡住了它，见下一条断言")
	}
	if Can(models.Role("ghost_role"), PermSystemMaintain) {
		t.Error("Can 必须先用 Valid() 挡住非法角色，不能只靠 Enforce")
	}
}

// TestCan_策略没加载起来时一律拒绝 验证 fail-closed 的最后一道。
//
// Enforcer 是包级全局，正常情况下永远非 nil（加载失败只会在 init 里打日志）。
// 这里把全局换成 nil 模拟「init 加载失败」，验证 Can 不是「拿不到策略就放行」。
//
// 反过来写就是整套系统当场失窃：策略文件写坏了一行，所有接口全开。
func TestCan_策略没加载起来时一律拒绝(t *testing.T) {
	silenceLog(t)
	saved := defaultEnforcer
	defaultEnforcer = nil
	t.Cleanup(func() { defaultEnforcer = saved })

	for _, role := range models.AllRoles() {
		for _, perm := range AllPermissions() {
			if Can(role, perm) {
				t.Errorf("策略未加载时 %s 执行 %s 竟然被放行——这是 fail-open，必须拒绝",
					role.Label(), perm)
			}
		}
	}
}

// TestLoad_策略文本有问题时返回错误 让 init 的 fail-closed 有依据。
//
// 覆盖三种坏法：模型语法坏、策略为空、字段数对不上。它们的共同点是
// **load 必须返回 error**，于是 init 保持 Enforcer 为 nil，Can 全拒。
func TestLoad_策略文本有问题时返回错误(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		policy string
	}{
		{"模型语法坏", "[request_definition]\nr = sub, obj\n", "p, admin, " + PermUserView + "\n"},
		{"策略为空", modelConf, ""},
		{"模型段缺失", "[request_definition]\nr = sub\n", "p, admin\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := load(c.model, c.policy); err == nil {
				t.Error("本该报错，实际加载成功——init 就不会 fail-closed 了")
			}
		})
	}
}

// legacyCanOperateSwitching 是被删掉的 middleware.CanOperateSwitching 的逐字复刻。
//
// 一行一行照抄，包括那条注释里的推理：role == director 放行，
// 否则 AtLeast(admin) 放行。之所以要把它抄进测试而不是随手写个
// `{director, admin, super_admin}` 的集合，是因为**抄错了这条测试就白写**，
// 而抄的过程本身就是提醒：「原来这里有个 `||`，说明它确实表达过
// 「等级够也行」」这个意图，不能简化成一次等值比较。
func legacyCanOperateSwitching(role models.Role) bool {
	if !role.Valid() {
		return false
	}
	if role == models.RoleDirector {
		return true
	}
	// 原实现用的是 AtLeast(RoleAdmin) 而不是等值比较：
	// 将来加更高的角色时它自动获得干预能力。
	return role.AtLeast(models.RoleAdmin)
}

// TestSwitchOperate_与被删掉的白名单逐字等价 是删除 RequireSwitchingOperator
// 的**唯一**依据。
//
// switch.operate 覆盖了 RequireSwitchingOperator 的全部职责，于是那个中间件
// 被删掉了——但删掉一道「谁能抢设备控制权」的门，必须有证据，不能靠「看起来
// 一样」。这条对**每一个角色**（含非法角色）断言两个判断完全相等，包括那条
// 最反直觉的：负责人等级高于导播，两个判断都拒绝。
//
// 哪天这条红了，说明策略被改动却没有同步核对过「谁还能抢锁」。
func TestSwitchOperate_与被删掉的白名单逐字等价(t *testing.T) {
	for _, role := range models.AllRoles() {
		got := Can(role, PermSwitchOperate)
		want := legacyCanOperateSwitching(role)
		if got != want {
			t.Errorf("%s 执行 switch.operate：实际 %v，迁移前的白名单是 %v——"+
				"两者必须完全等价，否则删除 RequireSwitchingOperator 就改了「谁能抢锁」",
				role.Label(), got, want)
		}
	}
	for _, bogus := range []models.Role{"", "hacker", "DIRECTOR"} {
		if Can(bogus, PermSwitchOperate) != legacyCanOperateSwitching(bogus) {
			t.Errorf("非法角色 %q 的判断不一致", bogus)
		}
	}

	// 负责人这一格单独钉一下：它 40 级比导播 30 高，任何 AtLeast(RoleDirector)
	// 写的门槛都会放他进来，而业务上负责人不参与导播工作。等级越高越放行
	// 这个直觉在这道门上恰好是错的。
	if !models.RoleLeader.AtLeast(models.RoleDirector) {
		t.Fatal("前置条件：负责人的等级确实高于导播")
	}
	if Can(models.RoleLeader, PermSwitchOperate) {
		t.Fatal("负责人等级高于导播，但仍不得操作切台")
	}
	if !Can(models.RoleDirector, PermSwitchOperate) {
		t.Fatal("导播必须能操作切台——这道门是正向路径，堵死它等于把现场锁死")
	}
}

// TestDeniedMessage_说清缺哪个权限以及调用者是什么角色。
//
// 「权限不足」四个字是排查不了任何事的：收到的人既不知道缺哪一项、也不知道
// 能不能找别人代做。这条断言钉住三个必须出现的信息：权限名、能做的人、
// 当前角色。少一个都让这条错误信息退回成「权限不足」。
func TestDeniedMessage_说清缺哪个权限以及调用者是什么角色(t *testing.T) {
	msg := DeniedMessage(PermProjectMember, models.RoleDirector)

	for _, want := range []string{
		PermProjectMember,           // 缺的是哪一项权限
		Label(PermProjectMember),    // 这项权限是干什么的
		models.RoleLeader.Label(),   // 谁可以做
		models.RoleDirector.Label(), // 我现在是什么角色
		"权限不足",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("拒绝信息应包含 %q，实际 %q", want, msg)
		}
	}

	// 不能出现「权限不足」就完事，也不能把持有者列表漏成空。
	if strings.TrimSpace(msg) == "权限不足" {
		t.Error("拒绝信息退化成了一句废话")
	}
}

// TestDeniedMessage_只有一个持有者时不说成空列表。
//
// system.maintain 只有超管一个持有者。这里防的是拼句子时把「仅 X 可执行」
// 与「仅 、 可执行」混掉——后者会让人以为这条权限坏了。
func TestDeniedMessage_只有一个持有者时不说成空列表(t *testing.T) {
	msg := DeniedMessage(PermSystemMaintain, models.RoleAdmin)
	if !strings.Contains(msg, "仅 "+models.RoleSuperAdmin.Label()+" 可执行") {
		t.Errorf("应说明只有超级管理员能执行，实际 %q", msg)
	}
	// 空列表的痕迹：「仅 、」「仅  可执行」「仅 可执行」。
	for _, hollow := range []string{"仅 、", "仅  可", "仅 可"} {
		if strings.Contains(msg, hollow) {
			t.Errorf("持有者列表拼空了（出现 %q）：%q", hollow, msg)
		}
	}
}

// TestHolders_与迁移矩阵一致 交叉验证 Holders 没有说谎。
//
// Holders 是错误信息的数据源，它是从策略现算的。万一哪天它算错了
// （比如某个 Valid 判断被改坏），403 的文案就会开始误导人——比没有文案
// 更糟：现场会照着文案去找一个其实也办不了的人。
func TestHolders_与迁移矩阵一致(t *testing.T) {
	for _, e := range expectations {
		got := Holders(e.perm)
		if len(got) != len(e.holders) {
			t.Errorf("权限 %s 的持有者数量：实际 %d（%v），期望 %d",
				e.perm, len(got), roleNames(got), len(e.holders))
			continue
		}
		for _, role := range e.holders {
			if !contains(got, role) {
				t.Errorf("权限 %s 的持有者里应有 %s，实际 %v", e.perm, role.Label(), roleNames(got))
			}
		}
	}
}

// TestLabel_未知权限原样返回 便于排查路由上的错字。
func TestLabel_未知权限原样返回(t *testing.T) {
	if got := Label("project.memberr"); got != "project.memberr" {
		t.Errorf("未知权限应原样返回以便定位错字，实际 %q", got)
	}
	if Label(PermLogCleanup) == PermLogCleanup {
		t.Error("已声明的权限应有中文名")
	}
}

// TestLabel_每项权限都有中文说明 防的是新增权限忘了写说明。
//
// 错误信息里会用到它。缺了的话 DeniedMessage 会退化成只报一个英文权限名，
// 而那正是这条错误信息要解决的问题。
func TestLabel_每项权限都有中文说明(t *testing.T) {
	for _, perm := range AllPermissions() {
		label, ok := permissionLabels[perm]
		if !ok {
			t.Errorf("权限 %s 缺少中文说明（permissionLabels 里没有）", perm)
			continue
		}
		if strings.TrimSpace(label) == "" {
			t.Errorf("权限 %s 的中文说明是空串", perm)
		}
		if label == perm {
			t.Errorf("权限 %s 的中文说明等于权限名本身，等于没写", perm)
		}
	}
}

// permissionsOf 从**策略本身**读出某角色的权限清单。
//
// 刻意不走 Can（逐项问一遍），而是走 GetPermissionsForUser：问策略
// 「你给这个角色记了什么」与问 Can「他能不能做某件事」是两个问题，
// 前者才能发现策略里多出来的脏行。
func permissionsOf(t *testing.T, role models.Role) []string {
	t.Helper()
	rules, err := Default().GetPermissionsForUser(string(role))
	if err != nil {
		t.Fatalf("读取 %s 的权限失败：%v", role, err)
	}
	out := make([]string, 0, len(rules))
	for _, rule := range rules {
		if len(rule) != 2 {
			t.Errorf("策略行字段数不是 2：%v", rule)
			continue
		}
		if rule[0] != string(role) {
			t.Errorf("GetPermissionsForUser(%s) 返回了别人的规则：%v", role, rule)
		}
		out = append(out, rule[1])
	}
	return out
}

func contains(roles []models.Role, want models.Role) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}

func roleNames(roles []models.Role) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, string(r))
	}
	return out
}

// silenceLog 把标准日志输出丢掉，用例结束后恢复。
//
// 只给「会按格子刷屏」的用例用：Can 对每一次拒绝都打一行日志是**故意的**
// （线上真出现错字时要吵），但 8 角色 × 7 个错字 = 56 行会把用例结果淹掉。
// 不能用 t.Setenv 之类的方式绕——要的就是「日志还在，只是不吵我」。
func silenceLog(t *testing.T) {
	t.Helper()
	saved := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(saved) })
}
