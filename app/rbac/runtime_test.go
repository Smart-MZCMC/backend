package rbac

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"smart-mzcmc/app/models"
)

// 这一组用例覆盖的是「策略从文件搬进数据库、再被在线修改」这条链。
//
// 它与 rbac_test.go 的分工：那边断言的是**矩阵本身**（policy.csv 与设计意图
// 逐格一致），这边断言的是**搬运与改写**——播种出来的东西与文件一致、
// 改一项之后 Enforce 的结果真的变了、受保护的改动在**写入路径**上被挡住、
// 重载不通过时内存里仍是上一份可用策略。
//
// 为什么不用真实数据库：tests/test_case.go 那套会 Boot 整个应用，并在仓库里
// 留下一个真实的 database/smart-mzcmc.db（AGENTS.md 明确禁止）。所以这里给
// Store 注入一个内存实现——接口抽出来正是为了这件事（见 store.go 的说明）。
// 代价是 SQL 本身没有被覆盖到，那部分由 ormStore 的三行查询承担，风险很低：
// 它只做「select 全部」与「事务里先删后插」，没有任何条件拼接。

// memStore 是内存里的 role_permissions 表，形状与真表一一对应。
//
// 刻意保留两个真表才有的性质，因为它们正是这次要防的事故来源：
//   - 存的是**完整矩阵**（每个角色 × 每项权限一行，enabled 可能是 false），
//     所以「没写这一行」与「写了但不给」在读出来时能区分开。
//   - 同一个 (role, permission) 只允许一行。重复插入会报唯一约束冲突，
//     与真表上的部分唯一索引同一个效果。
type memStore struct {
	mu    sync.Mutex
	rows  []row
	fail  error
	seeds int

	// loadMutate 在第 n 次 Load（从 1 起）时被调用，用来模拟「这一次读出来
	// 的是一份违规数据」——现实里就是有人绕过写入路径直接改了表。
	//
	// 为什么需要它：写路径是「重载 → 算差集 → 写库 → 重载」，两次重载读的是
	// 同一张表。只想测第二次重载失败，就必须能按次序决定「哪一次读到坏数据」；
	// 一个只会一直返回坏数据的 store 会让**第一次**重载就失败，于是根本走不到
	// 回滚那一步。
	loadMutate func(call int)
	loadCalls  int

	// replaceFailFrom 让 ReplaceRole 从第 n 次（从 1 起）起失败，0 表示永不失败。
	// 分次而不是一刀切，是为了能造出「本次写入成功、回滚写入失败」这个组合。
	replaceFailFrom int
	replaceCalls    int
}

// paintCell 在**锁内**改一格，供 loadMutate 用。
//
// 为什么只改一格而不是整表重画：loadMutate 是在 Load 持锁期间被调用的，
// 既不能调 Load（自死锁），而整表重画会把刚写进去的改动一起抹掉——
// 而「写进去了、回滚没抹掉」恰恰是那些用例要验证的事实本身。
func (s *memStore) paintCell(role models.Role, perm string, enabled bool) {
	for i := range s.rows {
		if s.rows[i].role == role && s.rows[i].permission == perm {
			s.rows[i].enabled = enabled
		}
	}
}

// resetLoadCalls 把 Load 计数清零。
//
// 给那些「只想让写路径里的第 N 次 Load 读到坏数据」的用例用：绝对计数会
// 因为前面多跑了一次播种 Reload 而错位，而错位之后用例会因为一个看不懂的
// 前置条件失败（而不是因为它真正想验证的那件事）红掉。
func (s *memStore) resetLoadCalls() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadCalls = 0
}

type row struct {
	role       models.Role
	permission string
	enabled    bool
}

func newMemStore() *memStore { return &memStore{} }

// fromMatrix 按真表的播种方式铺一份矩阵进去（完整交叉积）。
func newMemStoreFromMatrix(m Matrix) *memStore {
	s := newMemStore()
	s.replaceAll(m)
	return s
}

func (s *memStore) replaceAll(m Matrix) {
	s.rows = nil
	for _, role := range models.AllRoles() {
		for _, perm := range AllPermissions() {
			s.rows = append(s.rows, row{role, perm, m[role][perm]})
		}
	}
}

// setAll 是 replaceAll 的加锁版，供**并发**用例使用。
//
// 单独一个入口而不是给 replaceAll 加锁：Seed 已经持着锁调 replaceAll，
// 同一个 Mutex 不可重入，加锁会直接死锁。
func (s *memStore) setAll(m Matrix) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replaceAll(m)
}

func (s *memStore) Load() (Matrix, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	s.loadCalls++
	if s.loadMutate != nil {
		s.loadMutate(s.loadCalls)
	}
	m := Matrix{}
	for _, r := range s.rows {
		cells, ok := m[r.role]
		if !ok {
			cells = map[string]bool{}
			m[r.role] = cells
		}
		cells[r.permission] = r.enabled
	}
	return m, nil
}

func (s *memStore) Seed(m Matrix) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.seeds++
	s.replaceAll(m)
	return nil
}

func (s *memStore) ReplaceRole(role models.Role, granted []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.replaceCalls++
	if s.replaceFailFrom > 0 && s.replaceCalls >= s.replaceFailFrom {
		return fmt.Errorf("写入 role_permissions 失败（测试构造，第 %d 次）", s.replaceCalls)
	}
	want := make(map[string]bool, len(granted))
	for _, perm := range granted {
		want[perm] = true
	}
	kept := s.rows[:0:0]
	for _, r := range s.rows {
		if r.role != role {
			kept = append(kept, r)
		}
	}
	for _, perm := range AllPermissions() {
		kept = append(kept, row{role, perm, want[perm]})
	}
	s.rows = kept
	return nil
}

// granted 读出某角色当前被授予的权限，供断言用。
func (s *memStore) granted(role models.Role) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.rows {
		if r.role == role && r.enabled {
			out = append(out, r.permission)
		}
	}
	sort.Strings(out)
	return out
}

// allRows 把整张表拍平成一个可比较的字符串序列，供「一个字节都不该写」
// 这类断言使用。
//
// 刻意把**不授予**的那些格也拍进去：只看被授予的集合的话，「把 A 换成 B」
// 这种改动有可能被漏看（两个集合的差集恰好为空是不可能的，但如果断言只比
// granted，一行的 enabled 从 true 翻成 false 加上另一行翻成 true 就看不出来）。
func (s *memStore) allRows() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, fmt.Sprintf("%s/%s=%v", r.role, r.permission, r.enabled))
	}
	return out
}

var _ Store = (*memStore)(nil)

// useStore 装上测试用的存储，并在用例结束后把内存里的策略与存储一起还原。
//
// 还原策略是必须的：这些用例改的是**包级**状态，而同一个包里还有
// rbac_test.go 那批断言 96 格矩阵的用例。它们靠 init() 装好的 embedded 策略
// 才有意义，被这里污染的话会红得莫名其妙。
//
// ⚠️ 这里**刻意不用 withPolicy** 还原 Enforcer：withPolicy 自己也会注册一个
// cleanup，把 active.enforcer 还原成**它被调用那一刻**的值——也就是本次用例
// 刚装上去的那一份。于是「还原」等于什么都没做，每一条用例都会把最后装上的
// Enforcer 留给后面的用例。绝大多数用例看不出来（它们都从 embedded 播种，
// 换回去是同一份），一旦有用例刻意装一份**不同**的策略（concurrency_test.go
// 就在做这件事），后面就会莫名其妙地红。
func useStore(t *testing.T, s Store) {
	t.Helper()
	savedStore := currentStore()
	savedEnforcer := loadedPolicy()
	savedSource := Source()
	savedWarnings := Warnings()

	SetStore(s)
	t.Cleanup(func() {
		SetStore(savedStore)
		active.Lock()
		active.enforcer = savedEnforcer
		active.source = savedSource
		active.warnings = savedWarnings
		active.Unlock()
	})
}

// TestSeed_表为空时用policyCSV播种且与设计意图逐格一致 守住迁移的起点。
//
// 这条最关键的地方是**期望表只有一份**：用的就是 rbac_test.go 里那张
// expectations（同一个包变量）。写成两份就等于允许两份期望各写各的，
// 而它们迟早会分叉——分叉之后哪一份是真的就没人知道了。
//
// 为什么必须逐格（96 格）而不是抽查：漏播一行的后果是该角色少一项能力，
// 而少能力不会让任何「放行方向」的断言失败。
func TestSeed_表为空时用policyCSV播种且与设计意图逐格一致(t *testing.T) {
	silenceLog(t)
	// 空表 —— 全新部署、迁移刚建完表时的状态。
	s := newMemStore()
	useStore(t, s)

	if err := Reload(); err != nil {
		t.Fatalf("空表应当触发播种并装载成功，实际 %v", err)
	}

	// 播种的次数必须是 1，而且只发生一次。
	s.mu.Lock()
	seeds := s.seeds
	s.mu.Unlock()
	if seeds != 1 {
		t.Errorf("空表应播种一次，实际 %d 次", seeds)
	}

	m, err := s.Load()
	if err != nil {
		t.Fatalf("读回矩阵失败：%v", err)
	}
	// 存的是完整矩阵：8 角色 × 12 权限 = 96 行，一张都不少。
	if got, want := len(s.rows), len(expectations)*len(allRolesEight); got != want {
		t.Errorf("矩阵应有 %d 行（角色 × 权限），实际 %d 行", want, got)
	}

	want := holderSet()
	cells := 0
	for _, e := range expectations {
		for _, role := range models.AllRoles() {
			cells++
			got := m[role][e.perm]
			expect := want[e.perm][role]
			if got != expect {
				t.Errorf("播种后 %s 对 %s 应为 %v，实际 %v", role.Label(), e.perm, expect, got)
			}
		}
	}
	if cells != len(expectations)*len(allRolesEight) {
		t.Fatalf("矩阵格数不对：跑了 %d 格，期望 %d 格", cells, len(expectations)*len(allRolesEight))
	}

	// 播种之后再 Reload 不该重新播种：表已经不再是空的。
	if err := Reload(); err != nil {
		t.Fatalf("第二次装载失败：%v", err)
	}
	s.mu.Lock()
	seeds = s.seeds
	s.mu.Unlock()
	if seeds != 1 {
		t.Errorf("表非空时不该再播种，累计播种 %d 次", seeds)
	}
}

// TestSeed_表非空时不覆盖现场改动 防的是「重启就把配置冲掉」。
//
// 在线路径上改过权限之后，重启后端必须保留那些改动。如果播种条件写成
// 「每次启动都重播一遍」，现场会得到一个所有人都点不动又说不清为什么的系统，
// 而现场唯一的线索是「昨天还好好的」。
func TestSeed_表非空时不覆盖现场改动(t *testing.T) {
	silenceLog(t)
	baseline, err := embeddedMatrix()
	if err != nil {
		t.Fatalf("读内嵌策略失败：%v", err)
	}
	m := cloneMatrix(baseline)
	leaderBefore := grantedIn(m, models.RoleLeader)
	if len(leaderBefore) < 2 {
		t.Fatalf("前提不成立：负责人本该持有多项权限，实际 %v", leaderBefore)
	}
	// 模拟现场改过：负责人不再有 log.export。
	m[models.RoleLeader][PermLogExport] = false
	s := newMemStoreFromMatrix(m)
	useStore(t, s)

	if err := Reload(); err != nil {
		t.Fatalf("装载失败：%v", err)
	}
	if Can(models.RoleLeader, PermLogExport) {
		t.Error("现场撤销的权限被播种冲掉了——重启不该覆盖数据库里的策略")
	}
	if got := PermissionsOf(models.RoleLeader); len(got) != len(leaderBefore)-1 {
		t.Errorf("负责人应恰好少一项权限，实际持有 %d 项（原 %d 项）",
			len(got), len(leaderBefore))
	}
}

// grantedIn 数一数矩阵里某角色被授予了几项。
func grantedIn(m Matrix, role models.Role) []string {
	var out []string
	for _, perm := range AllPermissions() {
		if m[role][perm] {
			out = append(out, perm)
		}
	}
	return out
}

func cloneMatrix(m Matrix) Matrix {
	out := Matrix{}
	for role, cells := range m {
		copied := make(map[string]bool, len(cells))
		for perm, enabled := range cells {
			copied[perm] = enabled
		}
		out[role] = copied
	}
	return out
}

// TestReload_从数据库装载的矩阵逐格等于播种结果 证明「库即事实来源」。
//
// 这条与上面那条互为镜像：上面断言「播下去的与设计意图一致」，这条断言
// 「装载出来的与播下去的完全一致」。少一条的话，就可能出现「播对了但读回来
// 变了」——比如读取时把 enabled 反过来，而这种错误在放行方向上完全看不出来。
func TestReload_从数据库装载的矩阵逐格等于播种结果(t *testing.T) {
	silenceLog(t)
	baseline, err := embeddedMatrix()
	if err != nil {
		t.Fatalf("读内嵌策略失败：%v", err)
	}
	s := newMemStoreFromMatrix(cloneMatrix(baseline))
	useStore(t, s)

	if err := Reload(); err != nil {
		t.Fatalf("装载失败：%v", err)
	}
	for _, e := range expectations {
		for _, role := range e.holders {
			if !Can(role, e.perm) {
				t.Errorf("%s 应持有 %s，装载后却没有", role.Label(), e.perm)
			}
		}
		for _, role := range models.AllRoles() {
			expect := false
			for _, holder := range e.holders {
				if holder == role {
					expect = true
				}
			}
			if Can(role, e.perm) != expect {
				t.Errorf("%s 对 %s：实际 %v，期望 %v（与 policy.csv 不一致）",
					role.Label(), e.perm, !expect, expect)
			}
		}
	}
}

// TestApplyRolePermissions_改一项立刻生效再改回来 是这条写入路径的正向路径。
//
// 必须走完整的三步（写库 → 装载 → Enforce 结果变化），因为出问题的地方恰恰
// 在中间那两步：只测「函数返回了 granted」会漏掉「库里改了但内存里没换」。
func TestApplyRolePermissions_改一项立刻生效再改回来(t *testing.T) {
	silenceLog(t)
	s := newMemStore()
	useStore(t, s)
	if err := Reload(); err != nil {
		t.Fatalf("播种失败：%v", err)
	}

	// 前置条件：导播本来没有 user.view。
	if Can(models.RoleDirector, PermUserView) {
		t.Fatal("前提不成立：导播本来就持有 user.view，下面的断言已无意义")
	}

	grant := append(PermissionsOf(models.RoleDirector), PermUserView)
	change, err := ApplyRolePermissions(models.RoleDirector, grant)
	if err != nil {
		t.Fatalf("授予 user.view 给导播应成功，实际 %v", err)
	}
	if len(change.Granted) != 1 || change.Granted[0] != PermUserView {
		t.Errorf("变更结果应报告新增了 user.view，实际 %v", change.Granted)
	}
	if len(change.Revoked) != 0 {
		t.Errorf("这次不该有取消项，实际 %v", change.Revoked)
	}
	if !Can(models.RoleDirector, PermUserView) {
		t.Error("授予之后导播应当立刻能执行 user.view（不重启、不等下一次重载）")
	}
	if got := s.granted(models.RoleDirector); !containsString(got, PermUserView) {
		t.Errorf("数据库里应已写入 user.view，实际 %v", got)
	}

	// 改回来。这一步同样要立刻生效。
	original := PermissionsOf(models.RoleDirector)
	original = removeString(original, PermUserView)
	change, err = ApplyRolePermissions(models.RoleDirector, original)
	if err != nil {
		t.Fatalf("撤销应成功，实际 %v", err)
	}
	if len(change.Revoked) != 1 || change.Revoked[0] != PermUserView {
		t.Errorf("变更结果应报告取消了 user.view，实际 %v", change.Revoked)
	}
	if Can(models.RoleDirector, PermUserView) {
		t.Error("撤销之后导播不该再能执行 user.view")
	}
}

// TestApplyRolePermissions_语义是集合替换而不是追加 防的是前端少传一项。
//
// 前端渲染的是一整张勾选表，提交的是全量。如果服务端按「追加」理解，
// 取消勾选就永远传不上去，而界面上那一格会自己弹回来——
// 表现为「点了没反应」，最难排查的一类症状。
func TestApplyRolePermissions_语义是集合替换而不是追加(t *testing.T) {
	silenceLog(t)
	s := newMemStore()
	useStore(t, s)
	if err := Reload(); err != nil {
		t.Fatalf("播种失败：%v", err)
	}

	// 挑一个**本来就持有多项**的角色，否则这次改动是空操作，
	// 断言就只是在验证「什么都没发生」。
	const target = models.RoleAdmin
	before := PermissionsOf(target)
	if len(before) < 4 {
		t.Fatalf("前提不成立：管理员本该持有多项权限，实际 %v", before)
	}

	// 只传两项 → 其余全部必须变成「不授予」。
	change, err := ApplyRolePermissions(target, []string{PermLogView, PermProjectView})
	if err != nil {
		t.Fatalf("缩小权限集合应成功，实际 %v", err)
	}
	if got := PermissionsOf(target); len(got) != 2 {
		t.Errorf("管理员应恰好持有 2 项权限，实际 %d 项：%v", len(got), got)
	}
	if want := len(before) - 2; len(change.Revoked) != want {
		t.Errorf("变更结果应报告取消了 %d 项，实际 %d 项", want, len(change.Revoked))
	}
	if Can(target, PermLogExport) {
		t.Error("不在提交集合里的权限必须变成不授予（追加语义在这里就露馅了）")
	}
	// 其余角色不受影响：改一个人的权限不该动到别人的。
	if !Can(models.RoleLeader, PermLogExport) {
		t.Error("改管理员的权限把负责人的权限也弄没了——这不是单角色写入")
	}
}

// TestApplyRolePermissions_受保护的东西一样都改不动 是写入路径上的守卫。
//
// protect_test.go 已经逐条测过 ValidateGrant / ValidateRevoke /
// ValidateRemoveRole 本身。这里补的是**另一半**：这些判定确实被接到了
// 写入路径上。只测纯函数的话，一个「校验写了但没调用」的接缝会全部漏过，
// 而那正是本次改动最可能出的错。
func TestApplyRolePermissions_受保护的东西一样都改不动(t *testing.T) {
	silenceLog(t)
	cases := []struct {
		name  string
		role  models.Role
		grant []string // nil 表示「沿用当前集合再加这些」
		drop  []string // 要从当前集合里去掉��项
		code  PolicyErrorCode
	}{
		{
			name:  "把系统维护权限授予管理员",
			role:  models.RoleAdmin,
			grant: []string{PermSystemMaintain},
			code:  ErrCodeProtected,
		},
		{
			name: "从超级管理员自己身上撤销系统维护权限",
			role: models.RoleSuperAdmin,
			drop: []string{PermSystemMaintain},
			code: ErrCodeProtected,
		},
		{
			name: "清空超级管理员的全部权限",
			role: models.RoleSuperAdmin,
			drop: AllPermissions(),
			code: ErrCodeProtected,
		},
		{
			name: "对超级管理员做一项无关的改动",
			role: models.RoleSuperAdmin,
			drop: []string{PermLogExport},
			code: ErrCodeProtected,
		},
		{
			name:  "授予一个不存在的权限",
			role:  models.RoleLeader,
			grant: []string{"project.memberr"},
			code:  ErrCodeUnknownPermission,
		},
		{
			name:  "对不存在的角色改权限",
			role:  models.Role("hacker"),
			grant: []string{PermLogView},
			code:  ErrCodeUnknownRole,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newMemStore()
			useStore(t, s)
			if err := Reload(); err != nil {
				t.Fatalf("播种失败：%v", err)
			}
			before := PermissionsOf(c.role)
			beforeRows := append([]string(nil), s.granted(c.role)...)

			next := append([]string(nil), before...)
			next = append(next, c.grant...)
			next = removeStrings(next, c.drop...)

			_, err := ApplyRolePermissions(c.role, next)
			if err == nil {
				t.Fatal("本该被拒绝，实际通过了")
			}
			if got := CodeOf(err); got != c.code {
				t.Errorf("错误类别应为 %q，实际 %q（%v）——前端靠它区分"+
					"「受保护不可改」与「你选的东西过期了」", c.code, got, err)
			}
			// 一个字节都不写：库里与生效中的策略都必须原封不动。
			if got := s.granted(c.role); strings.Join(got, ",") != strings.Join(beforeRows, ",") {
				t.Errorf("被拒绝的请求不该写库：\n  之前 %v\n  之后 %v", beforeRows, got)
			}
			if got := PermissionsOf(c.role); strings.Join(got, ",") != strings.Join(before, ",") {
				t.Errorf("被拒绝的请求不该改生效中的策略：\n  之前 %v\n  之后 %v", before, got)
			}
		})
	}
}

// TestApplyRolePermissions_一项不合法则整次拒绝 防的是写一半。
//
// 「逐条写、逐条报错」看起来更友好，但它会留下半成品：一次请求里既有合法项
// 又有非法项时，前三项已经落库，第四项被拒——于是界面上显示「保存失败」，
// 而策略其实已经改了一部分。被拒绝的请求必须是**完全没发生**。
func TestApplyRolePermissions_一项不合法则整次拒绝(t *testing.T) {
	silenceLog(t)
	s := newMemStore()
	useStore(t, s)
	if err := Reload(); err != nil {
		t.Fatalf("播种失败：%v", err)
	}

	leaderBefore := append([]string(nil), s.granted(models.RoleLeader)...)
	adminBefore := append([]string(nil), s.granted(models.RoleAdmin)...)

	// 管理员的权限集合：一项合法的新增（project.view 本来就有，改成加上
	// 一项他也还没有的）+ 一项绝对非法的（把 system.maintain 给他）。
	adminNow := append([]string(nil), PermissionsOf(models.RoleAdmin)...)
	adminNext := append(append([]string(nil), adminNow...), PermSystemMaintain)

	_, err := ApplyRolePermissions(models.RoleAdmin, adminNext)
	if err == nil {
		t.Fatal("含非法项的请求应被拒绝")
	}
	if got := CodeOf(err); got != ErrCodeProtected {
		t.Errorf("错误类别应为 %q，实际 %q", ErrCodeProtected, got)
	}

	// 合法的那部分也必须没写进去。
	if got := s.granted(models.RoleAdmin); strings.Join(got, ",") != strings.Join(adminBefore, ",") {
		t.Errorf("合法项也不该被写进去：\n  之前 %v\n  之后 %v", adminBefore, got)
	}
	if Can(models.RoleAdmin, PermSystemMaintain) {
		t.Error("被拒绝的请求让管理员拿到了系统维护权限——这是最严重的一种半成品")
	}
	// 其他角色当然也不该被动过。
	if got := s.granted(models.RoleLeader); strings.Join(got, ",") != strings.Join(leaderBefore, ",") {
		t.Errorf("改管理员的权限动到了负责人：\n  之前 %v\n  之后 %v", leaderBefore, got)
	}
}

// TestReload_违反受保护规则时拒绝装载且保留上一份可用策略 是 fail-closed 的核心。
//
// 构造的场景：有人绕过本项目直接改了数据库，把 system.maintain 授予了管理员。
// 期望是两件事同时成立——
//
//	Reload 返回错误，且**内存里仍是上一份可用策略**。
//
// 只返回错误是不够的：如果实现成「先换内存、再返回错误」，那么这一刻
// 生效的就已经是违规策略了，而所有管理员都拿到了系统维护权限。
// 只打日志更糟：那等于违规策略已经在生效，而界面改不回来。
func TestReload_违反受保护规则时拒绝装载且保留上一份可用策略(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(Matrix)
	}{
		{
			name: "把系统维护权限授予管理员",
			break_: func(m Matrix) {
				m[models.RoleAdmin][PermSystemMaintain] = true
			},
		},
		{
			name: "让受保护权限无人持有",
			break_: func(m Matrix) {
				m[models.RoleSuperAdmin][PermSystemMaintain] = false
			},
		},
		{
			name: "受保护权限只落在非受保护角色手里",
			break_: func(m Matrix) {
				m[models.RoleSuperAdmin][PermSystemMaintain] = false
				m[models.RoleDirector][PermSystemMaintain] = true
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			silenceLog(t)
			// 先装一份**合法**的策略，让「上一份可用策略」有确切内容。
			good := newMemStore()
			useStore(t, good)
			if err := Reload(); err != nil {
				t.Fatalf("装载合法策略失败：%v", err)
			}
			if !Can(models.RoleSuperAdmin, PermSystemMaintain) {
				t.Fatal("前置条件不成立：超管本该持有系统维护权限")
			}

			// 直接改表，绕过写入路径（这正是需要 fail-closed 的场景）。
			broken, err := embeddedMatrix()
			if err != nil {
				t.Fatalf("读内嵌策略失败：%v", err)
			}
			c.break_(broken)
			good.replaceAll(broken)

			if err := Reload(); err == nil {
				t.Fatal("装载违规策略应返回错误")
			} else if got := CodeOf(err); got != ErrCodeConflict {
				t.Errorf("错误类别应为 %q（前端要据此提示刷新），实际 %q", ErrCodeConflict, got)
			}

			// 内存里必须仍是上一份可用策略。
			if !Can(models.RoleSuperAdmin, PermSystemMaintain) {
				t.Error("重载被拒绝之后，超管竟然失去了系统维护权限——内存里被换成了违规策略")
			}
			if Can(models.RoleAdmin, PermSystemMaintain) {
				t.Error("重载被拒绝之后，管理员仍然拿到了系统维护权限——违规策略已经在生效")
			}
		})
	}
}

// TestApplyRolePermissions_重载不通过时回滚本次写入 补上写路径上的另一半。
//
// 上一条钉的是「Reload 自己不动内存」。这条钉的是「写完之后发现装不回来时，
// 数据库也要退回去」——否则库里留下一份装不上的策略，而生效中的还是旧的，
// 两者从此对不上，且没有任何界面能看出这个分叉。
func TestApplyRolePermissions_重载不通过时回滚本次写入(t *testing.T) {
	silenceLog(t)
	s := newMemStore()
	useStore(t, s)
	if err := Reload(); err != nil {
		t.Fatalf("播种失败：%v", err)
	}

	// 在**别的**角色上制造一处破坏：给管理员加上 system.maintain。
	// 这一处不是本次要改的角色，所以三条 Validate* 都不会在写入路径上
	// 拦住它——它必须由装载时的核对拦住（fail-closed），并触发回滚。
	broken, err := embeddedMatrix()
	if err != nil {
		t.Fatalf("读内嵌策略失败：%v", err)
	}
	broken[models.RoleAdmin][PermSystemMaintain] = true
	s.replaceAll(broken)
	if err := Reload(); err == nil {
		t.Fatal("前置条件不成立：这份矩阵本该被重载拒绝")
	}

	directorBefore := append([]string(nil), s.granted(models.RoleDirector)...)
	next := append(PermissionsOf(models.RoleDirector), PermUserView)
	_, err = ApplyRolePermissions(models.RoleDirector, next)
	if err == nil {
		t.Fatal("重载不通过时本次改动应被判为失败")
	}
	if got := CodeOf(err); got != ErrCodeConflict && got != ErrCodeUnavailable {
		t.Errorf("错误类别应为 %q 或 %q，实际 %q", ErrCodeConflict, ErrCodeUnavailable, got)
	}
	if got := s.granted(models.RoleDirector); strings.Join(got, ",") != strings.Join(directorBefore, ",") {
		t.Errorf("重载失败后本次写入应被回滚：\n  之前 %v\n  之后 %v", directorBefore, got)
	}
	if Can(models.RoleDirector, PermUserView) {
		t.Error("已被回滚的改动不该在生效策略里出现")
	}
}

// TestApplyRolePermissions_写库失败时不动生效中的策略 是最后一道 fail-closed。
//
// 磁盘满、数据库被锁、字段约束冲突——写失败时必须原地不动。反过来
// （先改内存再写库，或者写失败也照样重载）都会让人看到一份
// 「界面上改了、重启后又变回去」的策略，那是最难解释的一种状态。
//
// 分两种失败点各测一次，因为它们落在链路的不同位置：**读表**失败会让写路径
// 在动任何字节之前就停住（连差集都还没算），**写表**失败则是在算完差集之后。
// 后者才是「界面上选好了、点保存、库里没进去」那种症状的来源。
func TestApplyRolePermissions_写库失败时不动生效中的策略(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*memStore)
	}{
		{
			name:   "写库失败",
			break_: func(s *memStore) { s.replaceFailFrom = 1 },
		},
		{
			name:   "读库失败",
			break_: func(s *memStore) { s.fail = fmt.Errorf("磁盘已满（测试构造）") },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			silenceLog(t)
			s := newMemStore()
			useStore(t, s)
			if err := Reload(); err != nil {
				t.Fatalf("播种失败：%v", err)
			}
			before := PermissionsOf(models.RoleDirector)
			beforeRows := append([]string(nil), s.granted(models.RoleDirector)...)

			s.mu.Lock()
			c.break_(s)
			s.mu.Unlock()

			next := append(append([]string(nil), before...), PermUserView)
			_, err := ApplyRolePermissions(models.RoleDirector, next)
			if err == nil {
				t.Fatal("失败时本次改动应被判为失败")
			}
			if got := CodeOf(err); got != ErrCodeUnavailable {
				t.Errorf("错误类别应为 %q，实际 %q", ErrCodeUnavailable, got)
			}
			if Can(models.RoleDirector, PermUserView) {
				t.Error("失败之后生效策略不该变化")
			}
			if got := s.granted(models.RoleDirector); strings.Join(got, ",") != strings.Join(beforeRows, ",") {
				t.Errorf("失败之后库里不该留下任何改动：\n  之前 %v\n  之后 %v", beforeRows, got)
			}
		})
	}
}

// TestApplyRolePermissions_库里那份已经违规时一个字节都不写 防的是「顺手带进去」。
//
// 场景：有人绕过本项目直接改了表，把 system.maintain 授予了管理员。此时
// 本项目根本不知道，且改完之后**每一次**重载都会因违反受保护规则而被拒绝。
//
// 这时如果有人点一下「保存」：
//
//	若写路径不先确认基线可用，它会把本次改动写进库（即便改动本身完全合法），
//	库里于是多出一份没人审过的改动，而请求返回 409「已回滚」——
//
// 回滚能不能成功全看磁盘脸色。
//
// 所以期望是：**在动任何字节之前就停住**，库里一个格子都不变。
func TestApplyRolePermissions_库里那份已经违规时一个字节都不写(t *testing.T) {
	silenceLog(t)
	s := newMemStore()
	useStore(t, s)
	if err := Reload(); err != nil {
		t.Fatalf("播种失败：%v", err)
	}

	// 绕过写入路径直接改表（现实里就是有人手改了 role_permissions）。
	// 从此每一次 Load 都读到这一格违规——写路径随后任何一次重载都会失败。
	s.loadMutate = func(int) { s.paintCell(models.RoleAdmin, PermSystemMaintain, true) }
	if err := Reload(); err == nil {
		t.Fatal("前置条件不成立：这份矩阵本该被重载拒绝")
	}
	rowsBefore := append([]string(nil), s.allRows()...)

	// 一次完全合法的改动：给导播加一项它可以有的权限。
	next := append(PermissionsOf(models.RoleDirector), PermUserView)
	_, err := ApplyRolePermissions(models.RoleDirector, next)
	if err == nil {
		t.Fatal("库里那份已经违规时不该接受任何改动")
	}
	if got := CodeOf(err); got != ErrCodeConflict {
		t.Errorf("错误类别应为 %q，实际 %q", ErrCodeConflict, got)
	}
	if got := s.allRows(); strings.Join(got, "|") != strings.Join(rowsBefore, "|") {
		t.Errorf("一个字节都不该写：\n  之前 %v\n  之后 %v", rowsBefore, got)
	}
	if Can(models.RoleDirector, PermUserView) {
		t.Error("被拒绝的改动不该出现在生效策略里")
	}
}

// TestApplyRolePermissions_回滚失败后重启不会装上违规策略 是本次最该被钉住的一条。
//
// 它回答的是一个具体的、听起来很吓人的猜测：
//
//	「写库成功 → 重载失败 → 回滚；回滚本身也失败 →
//	  库里就留下一份违规策略 → 下次进程重启会把它加载起来吗？」
//
// 期望：**不会**。启动路径（Bootstrap → Reload）与运行期路径用的是同一个
// protectedRuleFindings，违规就是违规，启动时同样拒绝装载，于是退回内嵌
// policy.csv。内存里那份「上一份可用策略」在启动时本来就是内嵌的，所以重启
// 之后既不会装上违规策略，也不会让人误以为库里那份生效了。
//
// 这条同时钉住了另一个容易被忽略的事实：installEmbedded 必须**真的**把内嵌
// 策略装进内存。只把 source 标成 embedded 的话，重启后报出来的来源与实际
// 生效的那份会对不上——管理员会按错误的依据判断「现在到底听谁的」。
func TestApplyRolePermissions_回滚失败后重启不会装上违规策略(t *testing.T) {
	silenceLog(t)
	s := newMemStore()
	useStore(t, s)
	if err := Reload(); err != nil {
		t.Fatalf("播种失败：%v", err)
	}
	if !Can(models.RoleSuperAdmin, PermSystemMaintain) {
		t.Fatal("前置条件不成立：超管本该持有系统维护权限")
	}

	// 从「这一次 ApplyRolePermissions」开始计数：第 1 次 Load 是写路径自己的
	// 预重载（必须合法，否则它会在动任何字节之前就停住），第 2 次是写完之后
	// 的重载 —— 那一次读到违规数据，于是触发回滚。
	s.loadMutate = func(call int) {
		if call >= 2 {
			s.paintCell(models.RoleAdmin, PermSystemMaintain, true)
		}
	}
	s.resetLoadCalls()
	// 第二次 ReplaceRole 失败 —— 那正是回滚那一次。
	s.replaceFailFrom = 2

	next := append(PermissionsOf(models.RoleDirector), PermUserView)
	_, err := ApplyRolePermissions(models.RoleDirector, next)
	if err == nil {
		t.Fatal("重载不通过时本次改动应被判为失败")
	}

	// 此刻库里留下的是「回滚失败」之后的状态：本次改动还在，
	// 而被污染的违规行也还在。这一刻内存里是安全的（旧策略）。
	if Can(models.RoleDirector, PermUserView) {
		t.Error("重载失败之后生效策略不该包含本次改动")
	}
	if Can(models.RoleAdmin, PermSystemMaintain) {
		t.Fatal("前置条件不成立：内存里不该已经装上违规策略")
	}
	if !containsString(s.granted(models.RoleDirector), PermUserView) {
		t.Fatal("前置条件不成立：本构造里的回滚应当失败（库里留着本次改动）")
	}
	if !containsString(s.granted(models.RoleAdmin), PermSystemMaintain) {
		t.Fatal("前置条件不成立：库里应留着那份违规数据")
	}

	// ── 模拟进程重启 ──────────────────────────────────────────────────
	// 这里不新起进程：包级状态（store 与内存里的 Enforcer）就是进程状态，
	// 而「重启」在这套代码里等价于「重新调一次 Bootstrap」。Bootstrap 之所以
	// 能代表重启，是因为装配点只有它，而它只读 store 与 init 装的那份内嵌策略。
	Bootstrap()

	if Can(models.RoleAdmin, PermSystemMaintain) {
		t.Error("❗重启即中招：违规策略在启动时被装载了。" +
			"启动路径必须与运行期路径一样 fail-closed")
	}
	if !Can(models.RoleSuperAdmin, PermSystemMaintain) {
		t.Error("退回内嵌策略后超管应仍持有系统维护权限——退回不能变成全体失权")
	}
	if got := Source(); got != SourceEmbedded {
		t.Errorf("库里违规时启动应退回内嵌策略，实际来源 %q", got)
	}
	// 来源说 embedded，内存里就**必须**真是内嵌那份：库里那份留着本次改动，
	// 生效的却不能是它。只改 source 不装策略的话，管理员看到的来源与实际
	// 生效的那份就对不上，而他们正是照着这个来源判断「现在到底听谁的」。
	if containsString(PermissionsOf(models.RoleDirector), PermUserView) {
		t.Error("❗声明来自内嵌策略，但生效的却是库里那份——installEmbedded 必须真的装策略，" +
			"只改 source 等于报了一句谎")
	}
	if got, want := strings.Join(PermissionsOf(models.RoleDirector), ","),
		strings.Join(grantedIn(mustEmbeddedMatrixT(t), models.RoleDirector), ","); got != want {
		t.Errorf("重启后生效的应是内嵌策略：期望 %v，实际 %v", want, got)
	}
	// 而且必须把真正的原因说出来：不是「表坏了」，是「你表里的数据违规」。
	found := false
	for _, w := range Warnings() {
		if strings.Contains(w, "受保护规则") {
			found = true
		}
	}
	if !found {
		t.Errorf("退回内嵌策略时必须在告警里说明真正的原因（库里的数据违反受保护规则），"+
			"否则运维会去查「表为什么读不出来」，而真正的原因是有人手改了数据。实际 %v",
			Warnings())
	}
}

// mustEmbeddedMatrixT 是 embeddedMatrix 的 fatal-on-error 版，给用例用。
func mustEmbeddedMatrixT(t *testing.T) Matrix {
	t.Helper()
	m, err := embeddedMatrix()
	if err != nil {
		t.Fatalf("读内嵌策略失败：%v", err)
	}
	return m
}

// TestBootstrap_数据库读不出来时退回内嵌策略并给出告警 钉住「策略表坏掉不停播」。
//
// 这条是「服务不能因为一张策略表就起不来」的证据。反过来做（读不出来就
// 让启动失败）会让一次直播因为一张策略表停摆——而内嵌的 policy.csv
// 本来就是一份完整可用的策略，退回它没有任何安全性损失，只有可用性收益。
func TestBootstrap_数据库读不出来时退回内嵌策略并给出告警(t *testing.T) {
	cases := []struct {
		name string
		prep func(*memStore)
	}{
		{"表不存在（迁移没跑）", func(s *memStore) { s.fail = fmt.Errorf("no such table: role_permissions") }},
		{"磁盘只读", func(s *memStore) { s.fail = fmt.Errorf("attempt to write a readonly database") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			silenceLog(t)
			s := newMemStore()
			c.prep(s)
			useStore(t, s)

			// Bootstrap 不返回错误——它就不该让启动失败。
			Bootstrap()

			if got := Source(); got != SourceEmbedded {
				t.Errorf("策略表不可用时应退回 embedded，实际 %q", got)
			}
			// 退回之后必须仍然可用：全套矩阵与文件一致。
			if !Can(models.RoleSuperAdmin, PermSystemMaintain) {
				t.Error("退回内嵌策略后超管应仍持有系统维护权限——退回不能变成全体失权")
			}
			if !Can(models.RoleDirector, PermSwitchOperate) {
				t.Error("退回内嵌策略后导播应仍能操作切台")
			}
			// 而且必须把这件事说出来：悄悄退回是最坏的一种，
			// 它会让管理员以为自己在改数据库里的策略。
			found := false
			for _, w := range Warnings() {
				if strings.Contains(w, "role_permissions") || strings.Contains(w, "退回") {
					found = true
				}
			}
			if !found {
				t.Errorf("退回内嵌策略必须在 warnings 里说清原因，实际 %v", Warnings())
			}
		})
	}
}

// TestBootstrap_启动时把库里的现场改动带进内存 防的是「重启丢配置」。
//
// 与 Reload 那条重复是有意的：Reload 是内部机制，Bootstrap 是它唯一的调用方。
// 少测一层，就可能在装配处把它换成一个「重新播种」的版本而不被发现。
func TestBootstrap_启动时把库里的现场改动带进内存(t *testing.T) {
	silenceLog(t)
	m, err := embeddedMatrix()
	if err != nil {
		t.Fatalf("读内嵌策略失败：%v", err)
	}
	m[models.RoleLeader][PermLogExport] = false
	useStore(t, newMemStoreFromMatrix(m))

	Bootstrap()

	if Source() != SourceDatabase {
		t.Errorf("库里有数据时应从数据库装载，实际来源 %q", Source())
	}
	if Can(models.RoleLeader, PermLogExport) {
		t.Error("启动装载丢了现场撤销的权限")
	}
}

// TestApplyRolePermissions_没有变化时也返回非nil切片 防的是 JSON 里出现 null。
//
// encoding/json 把 Go 的 nil 切片编成 null。null 在 JS 里既没有 .length 也没有
// .map，前端一句 `res.granted.length` 就会在**保存成功的那一瞬**抛 TypeError：
// 页面崩掉、停在编辑态，而后端返回的是 200。现场看起来像「改崩了」，真正
// 的原因只是「这次没有新增」——排查方向会被彻底带偏。
//
// 空集合是**合法且有意义的**状态（这次确实什么都没变），所以它必须以 []
// 出现，而不是以「没有值」出现。构造点只有 ApplyRolePermissions 一处，
// 所以这里钉住 `Change` 本身，而不是钉住端点——端点那层另外有两条用例。
func TestApplyRolePermissions_没有变化时也返回非nil切片(t *testing.T) {
	silenceLog(t)
	s := newMemStore()
	useStore(t, s)
	if err := Reload(); err != nil {
		t.Fatalf("播种失败：%v", err)
	}

	cases := []struct {
		name       string
		next       func(current []string) []string
		wantGrant  []string
		wantRevoke []string
	}{
		{
			name: "原样提交（什么都没变）",
			next: func(current []string) []string {
				return append([]string(nil), current...)
			},
		},
		{
			name: "只新增不取消",
			next: func(current []string) []string {
				return append(append([]string(nil), current...), PermUserView)
			},
			wantGrant: []string{PermUserView},
		},
		{
			name: "只取消不新增",
			next: func(current []string) []string {
				return removeString(current, PermLogView)
			},
			wantRevoke: []string{PermLogView},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			change, err := ApplyRolePermissions(models.RoleDirector, c.next(PermissionsOf(models.RoleDirector)))
			if err != nil {
				t.Fatalf("改动应成功，实际 %v", err)
			}
			// 逐个判 nil，而不是比长度：长度对而值为 nil 的切片正是那个 bug。
			if change.Granted == nil {
				t.Error("Change.Granted 是 nil 切片——JSON 里会是 null 而不是 []")
			}
			if change.Revoked == nil {
				t.Error("Change.Revoked 是 nil 切片——JSON 里会是 null 而不是 []")
			}
			if got := strings.Join(change.Granted, ","); got != strings.Join(c.wantGrant, ",") {
				t.Errorf("Granted 应为 %v，实际 %v", c.wantGrant, change.Granted)
			}
			if got := strings.Join(change.Revoked, ","); got != strings.Join(c.wantRevoke, ",") {
				t.Errorf("Revoked 应为 %v，实际 %v", c.wantRevoke, change.Revoked)
			}
			// 再从 JSON 的角度确认一次：这一层就是它真正去的地方。
			raw, err := json.Marshal(change)
			if err != nil {
				t.Fatalf("序列化 Change 失败：%v", err)
			}
			for _, field := range []string{`"Granted":null`, `"Revoked":null`} {
				if strings.Contains(string(raw), field) {
					t.Errorf("序列化结果里出现 %s：%s", field, raw)
				}
			}
		})
	}
}

// TestView_快照与生效策略一致 防的是界面显示与实际放行对不上。
//
// 界面渲染的 holders/grants 全靠这份快照。它一旦与 Can 的结果分叉，
// 排查方向会被彻底带偏：界面显示某角色没有某项权限，人就去反复勾选，
// 而真相是它有——被脏行挡住了，刷新多少次都没用。
func TestView_快照与生效策略一致(t *testing.T) {
	silenceLog(t)
	s := newMemStore()
	useStore(t, s)
	if err := Reload(); err != nil {
		t.Fatalf("播种失败：%v", err)
	}

	view := View()
	if view.Source != SourceDatabase {
		t.Errorf("快照的 source 应为 %q，实际 %q", SourceDatabase, view.Source)
	}
	if len(view.Permissions) != len(AllPermissions()) {
		t.Fatalf("快照应包含全部 %d 项权限，实际 %d 项", len(AllPermissions()), len(view.Permissions))
	}
	if len(view.Roles) != len(models.AllRoles()) {
		t.Fatalf("快照应包含全部 %d 个角色，实际 %d 个", len(models.AllRoles()), len(view.Roles))
	}
	// 空的 warnings 必须是 [] 而不是 null——前端 for...of 遇到 null 会报错。
	if view.Warnings == nil {
		t.Error("warnings 为 nil，会被 JSON 编成 null，前端遍历它时会出错")
	}
	// grants 同样不能是 null。
	for _, r := range view.Roles {
		if r.Grants == nil {
			t.Errorf("角色 %s 的 grants 为 nil", r.Value)
		}
	}

	// 快照里**每一个**会被 JSON 序列化的切片字段都必须非 nil。
	//
	// 逐个字段点名，而不是只盯 grants/warnings 两个：这份 JSON 是前后端之间
	// 唯一的契约，少写一个字段名就等于下一次有人加字段时又漏一次——而症状是
	// 前端在**保存成功的那一瞬**崩掉并停在编辑态，看起来像后端改崩了。
	// Permissions / Roles 用 nil 判断就够了：make(..., 0, n) 之后它们必然非 nil，
	// 一旦有人改成 var 声明，这里立刻报。
	if view.Permissions == nil {
		t.Error("view.Permissions 为 nil（JSON 里是 null，前端 for...of 直接抛异常）")
	}
	if view.Roles == nil {
		t.Error("view.Roles 为 nil（JSON 里是 null，前端 for...of 直接抛异常）")
	}
	for _, p := range view.Permissions {
		if p.Holders == nil {
			t.Errorf("权限 %s 的 holders 为 nil（JSON 里是 null，前端读 .length 会抛异常）", p.Name)
		}
	}

	byPerm := map[string]PermissionView{}
	for _, p := range view.Permissions {
		byPerm[p.Name] = p
	}
	for _, p := range view.Permissions {
		for _, holder := range p.Holders {
			if !Can(models.Role(holder), p.Name) {
				t.Errorf("快照说 %s 持有 %s，但实际放行结果是否定的", holder, p.Name)
			}
		}
		if p.Protected != IsProtected(p.Name) {
			t.Errorf("%s 的 protected 应为 %v，实际 %v", p.Name, IsProtected(p.Name), p.Protected)
		}
		if p.Label != Label(p.Name) {
			t.Errorf("%s 的中文名应为 %q，实际 %q", p.Name, Label(p.Name), p.Label)
		}
	}

	for _, r := range view.Roles {
		role := models.Role(r.Value)
		grants := PermissionsOf(role)
		if strings.Join(r.Grants, ",") != strings.Join(grants, ",") {
			t.Errorf("角色 %s 的 grants 与实际不符：快照 %v，实际 %v", r.Value, r.Grants, grants)
		}
		if r.Protected != IsProtectedRole(role) {
			t.Errorf("角色 %s 的 protected 应为 %v，实际 %v", r.Value, IsProtectedRole(role), r.Protected)
		}
		if r.Level != role.Level() {
			t.Errorf("角色 %s 的等级应为 %d，实际 %d", r.Value, role.Level(), r.Level)
		}
	}

	// 受保护的那一项必须出现在快照里，且带 protected=true。
	// 少了它，前端会把受保护的格子画成可点，用户点了才收到一句「不允许」。
	if !byPerm[PermSystemMaintain].Protected {
		t.Error("快照里 system.maintain 的 protected 应为 true")
	}
	if len(byPerm[PermSystemMaintain].Holders) != 1 ||
		byPerm[PermSystemMaintain].Holders[0] != string(models.RoleSuperAdmin) {
		t.Errorf("system.maintain 的持有者应只有超管，实际 %v", byPerm[PermSystemMaintain].Holders)
	}
	// 受保护权限的理由必须在 warnings 里，否则界面没有理由去解释「为什么不能改」。
	hasReason := false
	for _, w := range Warnings() {
		if strings.Contains(w, PermSystemMaintain) {
			hasReason = true
		}
	}
	if !hasReason {
		t.Errorf("warnings 里应说明受保护权限的理由，实际 %v", Warnings())
	}
}

// TestMatrixWarnings_矩阵残缺时必须吵 防的是「静悄悄地少一半能力」。
//
// 这些不致命（没有越权），但它们是现场唯一能看到的线索：某个角色一项权限
// 都没有的时候，现场只看得见「这个账号什么都点不了」，查不出原因。
func TestMatrixWarnings_矩阵残缺时必须吵(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(Matrix)
		want   string
	}{
		{
			name:   "某个角色一项权限都没有",
			break_: func(m Matrix) { m[models.RoleLogistics] = allDisabled() },
			want:   "后勤",
		},
		{
			name:   "矩阵里少了一个角色",
			break_: func(m Matrix) { delete(m, models.RoleDirector) },
			want:   "导播",
		},
		{
			name:   "出现未知角色",
			break_: func(m Matrix) { m[models.Role("ghost")] = allDisabled() },
			want:   "ghost",
		},
		{
			name:   "出现未知权限",
			break_: func(m Matrix) { m[models.RoleDirector]["project.viewr"] = true },
			want:   "project.viewr",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			silenceLog(t)
			m, err := embeddedMatrix()
			if err != nil {
				t.Fatalf("读内嵌策略失败：%v", err)
			}
			c.break_(m)
			warnings := matrixWarnings(m)
			joined := strings.Join(warnings, " | ")
			if !strings.Contains(joined, c.want) {
				t.Errorf("告警里应提到 %q，实际 %v", c.want, warnings)
			}
		})
	}

	// 反方向：完好无损的矩阵不该吵。告警一多就没人看了。
	m, err := embeddedMatrix()
	if err != nil {
		t.Fatalf("读内嵌策略失败：%v", err)
	}
	if got := matrixWarnings(m); len(got) != 0 {
		t.Errorf("完好的矩阵不该有告警，实际 %v", got)
	}
}

func allDisabled() map[string]bool {
	cells := map[string]bool{}
	for _, perm := range AllPermissions() {
		cells[perm] = false
	}
	return cells
}

// TestSource_退回内嵌之后warnings不会被下一次播种悄悄抹掉 防的是「提示消失」。
//
// 告警只有一次机会被看见：管理员打开权限页、看到「策略来自 embedded」、
// 知道要先重启。重启之后就没事了。所以每次退回都要重新说一遍，
// 不能因为「上一次也是 embedded」就省掉。
func TestSource_退回内嵌之后warnings不会被下一次播种悄悄抹掉(t *testing.T) {
	silenceLog(t)
	s := newMemStore()
	s.fail = fmt.Errorf("no such table: role_permissions")
	useStore(t, s)

	Bootstrap()
	first := strings.Join(Warnings(), " | ")

	// 故障恢复。
	s.mu.Lock()
	s.fail = nil
	s.mu.Unlock()
	if err := Reload(); err != nil {
		t.Fatalf("恢复后装载失败：%v", err)
	}
	if Source() != SourceDatabase {
		t.Errorf("恢复后来源应为 database，实际 %q", Source())
	}
	// 恢复之后不该还挂着「退回内嵌」那条，否则界面上会一直显示一个
	// 已经不成立的告警——而误报的告警比没有告警更坏。
	for _, w := range Warnings() {
		if strings.Contains(w, "退回内嵌") {
			t.Errorf("已恢复却还挂着退回告警：%q", w)
		}
	}
	if first == "" {
		t.Error("退回内嵌策略时应当至少产生一条告警")
	}
}

// TestStore_没有装配存储时Reload报错而不是静默成功 是装配层的兜底。
//
// 如果「没有存储」被当成「空表」，就会走播种分支——而播种同样需要存储，
// 于是变成一个看不懂的错误。这里明确报「未装配」，让装配遗漏一眼能认出来。
func TestStore_没有装配存储时Reload报错而不是静默成功(t *testing.T) {
	silenceLog(t)
	saved := currentStore()
	SetStore(nil)
	t.Cleanup(func() { SetStore(saved) })

	err := Reload()
	if err == nil {
		t.Fatal("没有装配存储时 Reload 应报错")
	}
	if got := CodeOf(err); got != ErrCodeUnavailable {
		t.Errorf("错误类别应为 %q，实际 %q", ErrCodeUnavailable, got)
	}
}

// TestApplyRolePermissions_未装配存储时拒绝改动 防的是「界面显示成功但什么也没变」。
//
// 这类不一致比直接报错难查得多：管理员以为改完了，刷新一下又变回去，
// 而日志里什么都没有。
func TestApplyRolePermissions_未装配存储时拒绝改动(t *testing.T) {
	silenceLog(t)
	saved := currentStore()
	SetStore(nil)
	t.Cleanup(func() { SetStore(saved) })

	_, err := ApplyRolePermissions(models.RoleDirector, []string{PermLogView})
	if err == nil {
		t.Fatal("没有装配存储时不应接受权限改动")
	}
}

// TestStore_表结构与真表一致 防的是「内存实现测试通过、真表上失败」。
//
// 这里断言的是 models.RolePermission 的表名与列名——它们必须与迁移里建的
// 一一对应。memStore 是手写的表，它与真表的一致性没有别的保障，
// 而一旦对不上（例如列名写错），上面所有用例都会绿着通过，
// 线上却是「表结构不匹配」这类只在运行时才暴露的问题。
func TestStore_表结构与真表一致(t *testing.T) {
	if got := (models.RolePermission{}).TableName(); got != "role_permissions" {
		t.Errorf("表名应为 role_permissions，实际 %q", got)
	}
	// 迁移里建的索引名。
	const indexName = "role_permissions_role_permission_unique"
	src, err := os.ReadFile(filepath.Join("..", "..", "database", "migrations",
		"20261102000001_create_role_permissions_table.go"))
	if err != nil {
		t.Fatalf("读迁移文件失败：%v", err)
	}
	migration := string(src)
	for _, want := range []string{
		indexName,
		`"role", 20`,
		`"permission", 64`,
		`Boolean("enabled")`,
		// 部分唯一索引：SQLite 的 UNIQUE 允许多个 NULL，但模型里两列都是
		// 普通 string，GORM 插入的是空串——不排除空串就会撞唯一约束。
		"WHERE role IS NOT NULL AND role != ''",
	} {
		if !strings.Contains(migration, want) {
			t.Errorf("迁移里应包含 %q", want)
		}
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func removeString(list []string, drop string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

func removeStrings(list []string, drops ...string) []string {
	out := list
	for _, d := range drops {
		out = removeString(out, d)
	}
	return out
}
