package controllers

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	contractshttp "github.com/goravel/framework/contracts/http"

	"smart-mzcmc/app/audit"
	"smart-mzcmc/app/models"
	"smart-mzcmc/app/rbac"
)

// 这一组用例盯的是**端点这一层**，与 app/rbac 里的那组分工明确：
//
//	app/rbac/runtime_test.go  测的是写入路径本身（校验 → 写库 → 重载 → 回滚）
//	本文件                    测的是端点这一层：请求怎么被解析、错误怎么变成
//	                          响应码与 code、以及审计有没有写
//
// 少任何一层都会漏掉一整类接缝错误：「校验写了但没调用」在纯函数测试里看不见，
// 「code 映射错了」在写入路径测试里也看不见——而前端正是靠 code 区分
// 「受保护不可改」与「你选的东西过期了」。

// fakeStore 是内存里的 role_permissions 表。
//
// 刻意与 app/rbac 的那个 memStore 分开写而不是共用：那个在 app/rbac 包内、
// 不导出；这里如果想共用就得把它导出，而导出一个「能让包外替换策略存储」
// 的东西等于给旁路开了个新口子。两个测试各自持有一份是刻意的。
type fakeStore struct {
	mu     sync.Mutex
	rows   []fakeRow
	fail   error
	seeds  int
	audit  []fakeAudit
	reject error
}

type fakeRow struct {
	role       models.Role
	permission string
	enabled    bool
}

// fakeAudit 是一次审计写入。
type fakeAudit struct {
	action string
	target string
	detail map[string]any
}

func newFakeStore() *fakeStore { return &fakeStore{} }

// baselineMatrix 是「没被任何用例动过」的权限矩阵，只在第一次用到时取一次。
//
// 刻意**快照一次**而不是每次都从 rbac.View() 现取：View() 读的是内存里生效的
// 那份，而上一条用例的改动会留在那里。于是第二次运行时铺下去的矩阵就带着上一次
// 的残留——`go test -count=3` 会直接把它暴露成一条「granted 实际为 []」的
// 莫名其妙失败。用一份固定的基线，每条用例的起点就与执行顺序、与跑了几遍
// 都无关了。
var baselineMatrix = sync.OnceValue(func() map[models.Role]map[string]bool {
	// 走 rbac.View() 而不是直接读 policy.csv：View 是 app/rbac 唯一导出的读出口，
	// 测试不引入第二条读策略的路——那种「测试自己解析一遍文件」的做法迟早会与
	// 生产解析分叉，而分叉之后测试还在绿。
	view := rbac.View()
	m := map[models.Role]map[string]bool{}
	for _, role := range models.AllRoles() {
		cells := map[string]bool{}
		for _, perm := range rbac.AllPermissions() {
			cells[perm] = false
		}
		m[role] = cells
	}
	for _, r := range view.Roles {
		for _, perm := range r.Grants {
			m[models.Role(r.Value)][perm] = true
		}
	}
	return m
})

// fromEmbedded 按固定基线铺一份完整矩阵。
func (s *fakeStore) fromEmbedded() error {
	s.seed(cloneBaseline())
	return nil
}

func cloneBaseline() map[models.Role]map[string]bool {
	src := baselineMatrix()
	out := map[models.Role]map[string]bool{}
	for role, cells := range src {
		copied := map[string]bool{}
		for perm, enabled := range cells {
			copied[perm] = enabled
		}
		out[role] = copied
	}
	return out
}

func (s *fakeStore) seed(m map[models.Role]map[string]bool) {
	s.rows = nil
	for _, role := range models.AllRoles() {
		for _, perm := range rbac.AllPermissions() {
			s.rows = append(s.rows, fakeRow{role, perm, m[role][perm]})
		}
	}
}

func (s *fakeStore) Load() (rbac.Matrix, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	m := rbac.Matrix{}
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

func (s *fakeStore) Seed(m rbac.Matrix) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.seeds++
	s.seed(m)
	return nil
}

func (s *fakeStore) ReplaceRole(role models.Role, granted []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	if s.reject != nil {
		return s.reject
	}
	want := map[string]bool{}
	for _, p := range granted {
		want[p] = true
	}
	kept := s.rows[:0:0]
	for _, r := range s.rows {
		if r.role != role {
			kept = append(kept, r)
		}
	}
	for _, p := range rbac.AllPermissions() {
		kept = append(kept, fakeRow{role, p, want[p]})
	}
	s.rows = kept
	return nil
}

// granted 读出某角色当前被授予的权限，供断言用。
func (s *fakeStore) granted(role models.Role) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var parts []string
	for _, r := range s.rows {
		if r.role == role && r.enabled {
			parts = append(parts, r.permission)
		}
	}
	return strings.Join(parts, ",")
}

var _ rbac.Store = (*fakeStore)(nil)

// withPolicyStore 装上测试存储与一个会落进 fakeAudit 的审计桩。
//
// 审计为什么要打桩：audit.Write 真的写 audit_logs 表，而本仓库没有连库的
// 测试脚手架（tests/test_case.go 会 Boot 整个应用并留下真实的
// database/smart-mzcmc.db，AGENTS.md 明确禁止）。审计的**判定**是这里真正
// 要测的东西——「成功与被拒都要落」——而落库本身已经由 app/audit 覆盖。
func withPolicyStore(t *testing.T) *fakeStore {
	t.Helper()
	s := newFakeStore()
	if err := s.fromEmbedded(); err != nil {
		t.Fatalf("铺初始矩阵失败：%v", err)
	}
	rbac.SetStore(s)
	// 主动重载一次，把「来源」与「生效中的策略」都定死在这一份基线上。
	//
	// 之前这里是靠**泄漏**成立的：cleanup 里 SetStore(nil) 之后调 Reload，
	// 而没有存储的 Reload 会直接返回错误、**不动内存**，所以上一条用例的
	// 来源与策略一起留给了下一条。断言 source == database 的用例因此能过，
	// 但那是靠运气，而 `go test -count=3` 会让运气用完。
	if err := rbac.Reload(); err != nil {
		t.Fatalf("从基线装载策略失败：%v", err)
	}

	savedSink := auditSink
	auditSink = func(_ contractshttp.Context, _ models.User, record audit.Record) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.audit = append(s.audit, fakeAudit{
			action: record.Action,
			target: record.TargetType + "/" + record.TargetID,
			detail: record.Detail,
		})
	}
	t.Cleanup(func() {
		auditSink = savedSink
		// 卸掉存储，回到「只有内嵌策略」的状态：app/rbac 包内其他用例断言的
		// 是那一份。这里**不**指望 SetStore(nil) 之后的 Reload 能还原——
		// 没有存储的 Reload 会直接返回错误且不动内存（这是它该有的行为），
		// 所以真正的还原发生在下一条用例的 withPolicyStore 里：它会铺上
		// 固定基线再主动 Reload 一次。
		rbac.SetStore(nil)
	})
	return s
}

func TestPolicyEndpoint_授予一项权限走完整链路并落审计(t *testing.T) {
	s := withPolicyStore(t)
	actor := user(1, models.RoleSuperAdmin)

	before := rbac.PermissionsOf(models.RoleDirector)

	grant := append(append([]string(nil), before...), rbac.PermUserView)
	ctx := newPolicyCtx(t, actor, map[string]string{"role": "director"},
		map[string]any{"permissions": toAny(grant)})

	resp := replyOf(t, ctx, (NewRbacController()).UpdateRolePermissions(ctx))

	if resp.status != 200 {
		t.Fatalf("授予应返回 200，实际 %d：%v", resp.status, resp.body)
	}
	if !rbac.Can(models.RoleDirector, rbac.PermUserView) {
		t.Error("授予之后导播应当立刻持有 user.view")
	}
	// 响应体经过 JSON 往返，所以这里拿到的是 []any 而不是 []string——
	// 这正是前端看到的样子，断言也应该按那个形状来。
	if got := jsonList(resp.body["granted"]); strings.Join(got, ",") != rbac.PermUserView {
		t.Errorf("响应里的 granted 应为 [%s]，实际 %v", rbac.PermUserView, resp.body["granted"])
	}
	if resp.body["role"] != "director" {
		t.Errorf("响应里应回显被改的角色，实际 %v", resp.body["role"])
	}
	if resp.body["source"] != rbac.SourceDatabase {
		t.Errorf("响应里应带上生效中的策略来源，实际 %v", resp.body["source"])
	}

	// 审计：谁、改了哪个角色、增删分别是什么。
	if len(s.audit) != 1 {
		t.Fatalf("成功的改动应落一条审计，实际 %d 条", len(s.audit))
	}
	entry := s.audit[0]
	if entry.action != "rbac.role_permissions" {
		t.Errorf("审计动作应为 rbac.role_permissions，实际 %q", entry.action)
	}
	if entry.target != "role/director" {
		t.Errorf("审计目标应为 role/director，实际 %q", entry.target)
	}
	// 增与删必须分开列：出事故时要回答的是「谁多了一项能力」或「谁失去了哪一项」，
	// 混在一个数组里就得人工比对才知道方向。
	detailGranted, _ := entry.detail["granted"].([]string)
	if len(detailGranted) != 1 || detailGranted[0] != rbac.PermUserView {
		t.Errorf("审计 detail 里的 granted 应为 [user.view]，实际 %v", entry.detail["granted"])
	}
	detailRevoked, ok := entry.detail["revoked"].([]string)
	if !ok || len(detailRevoked) != 0 {
		t.Errorf("审计 detail 里的 revoked 应为空，实际 %v", entry.detail["revoked"])
	}
}

// TestPolicyEndpoint_被拒绝的尝试也落审计 是这一组里最该有的一条。
//
// 有人试图削掉超管的 system.maintain，是这套系统里最该被人知道的一个时刻——
// 而它恰好是**什么都没发生**的那种时刻。只记「改了什么」的审计永远看不到它。
func TestPolicyEndpoint_被拒绝的尝试也落审计(t *testing.T) {
	cases := []struct {
		name     string
		role     string
		body     map[string]any
		wantCode rbac.PolicyErrorCode
		wantAct  string
	}{
		{
			name:     "把系统维护权限授予管理员",
			role:     "admin",
			body:     map[string]any{"permissions": []any{rbac.PermSystemMaintain}},
			wantCode: rbac.ErrCodeProtected,
			wantAct:  "rbac.role_permissions_denied",
		},
		{
			name: "从超管自己身上撤销系统维护权限",
			role: "super_admin",
			// 只提交一项 → 其余全被取消，包括 system.maintain。
			body:     map[string]any{"permissions": []any{rbac.PermLogView}},
			wantCode: rbac.ErrCodeProtected,
			wantAct:  "rbac.role_permissions_denied",
		},
		{
			name:     "对超管做任何改动",
			role:     "super_admin",
			body:     map[string]any{"permissions": []any{rbac.PermLogView, rbac.PermSystemMaintain}},
			wantCode: rbac.ErrCodeProtected,
			wantAct:  "rbac.role_permissions_denied",
		},
		{
			name:     "提交不存在的权限",
			role:     "leader",
			body:     map[string]any{"permissions": []any{"project.memberr"}},
			wantCode: rbac.ErrCodeUnknownPermission,
			wantAct:  "rbac.role_permissions_denied",
		},
		{
			name:     "提交不存在的角色",
			role:     "hacker",
			body:     map[string]any{"permissions": []any{rbac.PermLogView}},
			wantCode: rbac.ErrCodeUnknownRole,
			wantAct:  "rbac.role_permissions_denied",
		},
		{
			name:     "请求体里没有 permissions",
			role:     "leader",
			body:     map[string]any{"foo": "bar"},
			wantCode: rbac.ErrCodeUnknownPermission,
			wantAct:  "rbac.role_permissions_denied",
		},
		{
			name:     "permissions 不是数组",
			role:     "leader",
			body:     map[string]any{"permissions": "log.view"},
			wantCode: rbac.ErrCodeUnknownPermission,
			wantAct:  "rbac.role_permissions_denied",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := withPolicyStore(t)
			before := rbac.PermissionsOf(models.Role(c.role))

			ctx := newPolicyCtx(t, user(1, models.RoleSuperAdmin),
				map[string]string{"role": c.role}, c.body)
			resp := replyOf(t, ctx, (NewRbacController()).UpdateRolePermissions(ctx))

			if resp.status == 200 {
				t.Fatalf("本该被拒绝，实际 200：%v", resp.body)
			}
			// 前端靠 code 区分「受保护不可改」与「你选的东西过期了，刷新」，
			// 所以这里必须逐类对上。
			if got := resp.body["code"]; got != string(c.wantCode) {
				t.Errorf("响应 code 应为 %q，实际 %v", c.wantCode, got)
			}
			if msg, _ := resp.body["error"].(string); msg == "" {
				t.Error("错误响应必须带一句能直接显示的中文说明")
			}

			// 被拒绝时状态码要能区分处置方式。
			wantStatus := map[rbac.PolicyErrorCode]int{
				rbac.ErrCodeProtected:         403,
				rbac.ErrCodeUnknownRole:       400,
				rbac.ErrCodeUnknownPermission: 400,
			}
			if want, ok := wantStatus[c.wantCode]; ok && resp.status != want {
				t.Errorf("状态码应为 %d，实际 %d", want, resp.status)
			}

			// 一个字节都不写。
			after := rbac.PermissionsOf(models.Role(c.role))
			if strings.Join(before, ",") != strings.Join(after, ",") {
				t.Errorf("被拒绝的请求不该改变生效中的策略：\n  之前 %v\n  之后 %v", before, after)
			}
			if rbac.Can(models.RoleAdmin, rbac.PermSystemMaintain) {
				t.Error("被拒绝的请求让管理员拿到了系统维护权限——这是最严重的一种漏洞")
			}

			// 审计：被拒绝的尝试必须落，而且用**单独的动作名**。
			// 混在成功那个动作里的话，审计页按类型筛选就找不到它了。
			if len(s.audit) != 1 {
				t.Fatalf("被拒绝的尝试应落一条审计，实际 %d 条", len(s.audit))
			}
			if s.audit[0].action != c.wantAct {
				t.Errorf("审计动作应为 %q，实际 %q", c.wantAct, s.audit[0].action)
			}
			if got, ok := s.audit[0].detail["code"].(string); !ok || got != string(c.wantCode) {
				t.Errorf("审计 detail 里的 code 应为 %q，实际 %v", c.wantCode, s.audit[0].detail["code"])
			}
			if reason, _ := s.audit[0].detail["reason"].(string); reason == "" {
				t.Error("审计必须记下被拒的理由——只知道「他被拒了」答不了「他想干什么」")
			}
			// 请求里提交的那一组也要留着：有人反复尝试时，只知道「被拒了」
			// 是看不出他在试什么的。
			if _, ok := s.audit[0].detail["requested"]; !ok {
				t.Error("审计 detail 里应保留他提交的那一组权限")
			}
		})
	}
}

// TestPolicyEndpoint_状态码区分受保护与过期 防的是前端提示误导人。
//
// 受保护的那一格在界面上应当**渲染成不可点**，用户根本点不到；真点到了
// （比如界面用的是旧版本）也要给出「这是规则，不是我请求写错了」的信号。
// 给 400 会让前端与运维都去查代码，而真正的原因是策略。
func TestPolicyEndpoint_状态码区分受保护与过期(t *testing.T) {
	withPolicyStore(t)

	protectedResp := mustReject(t, "admin",
		map[string]any{"permissions": []any{rbac.PermSystemMaintain}})
	if protectedResp.status != 403 {
		t.Errorf("触碰受保护的东西应返回 403（规则不允许），实际 %d", protectedResp.status)
	}

	staleResp := mustReject(t, "leader",
		map[string]any{"permissions": []any{"project.memberr"}})
	if staleResp.status != 400 {
		t.Errorf("提交了不存在的权限应返回 400（请求写错了），实际 %d", staleResp.status)
	}
}

// TestPolicyEndpoint_重载冲突返回409 防的是「保存成功但没生效」。
//
// 这一类与前两类的处置完全不同：前两类是「改不了」，这一类是「这次没生效，
// 刷新看看」。合成同一类的话，前端会弹「不可修改」而管理员刷新一百次
// 也看不到任何变化。
func TestPolicyEndpoint_重载冲突返回409(t *testing.T) {
	s := withPolicyStore(t)

	// 在别的角色上制造一处破坏，绕过写入路径直接改表。
	m, err := s.Load()
	if err != nil {
		t.Fatalf("读矩阵失败：%v", err)
	}
	m[models.RoleAdmin][rbac.PermSystemMaintain] = true
	s.seed(m)
	// 先把内存里的策略装回合法的，否则它自己就带着违规项了。
	if err := rbac.Reload(); err == nil {
		t.Fatal("前置条件不成立：这份矩阵本该被重载拒绝")
	}

	directorBefore := rbac.PermissionsOf(models.RoleDirector)
	grant := append(append([]string(nil), directorBefore...), rbac.PermUserView)
	ctx := newPolicyCtx(t, user(1, models.RoleSuperAdmin),
		map[string]string{"role": "director"},
		map[string]any{"permissions": toAny(grant)})

	resp := replyOf(t, ctx, (NewRbacController()).UpdateRolePermissions(ctx))

	if resp.status != 409 {
		t.Fatalf("重载不通过应返回 409，实际 %d：%v", resp.status, resp.body)
	}
	if got := resp.body["code"]; got != string(rbac.ErrCodeConflict) {
		t.Errorf("响应 code 应为 %q，实际 %v", rbac.ErrCodeConflict, got)
	}
	// 回滚之后库里与生效中的策略都应回到改动前。
	if after := rbac.PermissionsOf(models.RoleDirector); strings.Join(after, ",") !=
		strings.Join(directorBefore, ",") {
		t.Errorf("被回滚的改动不该生效：\n  之前 %v\n  之后 %v", directorBefore, after)
	}
	// 被拒绝的尝试同样要落审计——「他试了但没成」正是要追的那类记录。
	if len(s.audit) != 1 || s.audit[0].action != "rbac.role_permissions_denied" {
		t.Errorf("冲突那次也应落审计，实际 %v", s.audit)
	}
}

// TestPolicyEndpoint_策略表不可用返回503 防的是「把服务端故障报成请求错误」。
//
// 说成 400 会让运维去查前端与后端参数，而真正的原因是磁盘满或数据库被锁。
func TestPolicyEndpoint_策略表不可用返回503(t *testing.T) {
	s := withPolicyStore(t)
	s.mu.Lock()
	s.fail = fmt.Errorf("disk I/O error（测试构造）")
	s.mu.Unlock()

	ctx := newPolicyCtx(t, user(1, models.RoleSuperAdmin),
		map[string]string{"role": "director"},
		map[string]any{"permissions": []any{rbac.PermLogView}})

	resp := replyOf(t, ctx, (NewRbacController()).UpdateRolePermissions(ctx))

	if resp.status != 503 {
		t.Fatalf("策略表不可用应返回 503，实际 %d：%v", resp.status, resp.body)
	}
	if got := resp.body["code"]; got != string(rbac.ErrCodeUnavailable) {
		t.Errorf("响应 code 应为 %q，实际 %v", rbac.ErrCodeUnavailable, got)
	}
}

// TestPolicyEndpoint_未登录一律401 挡住「没令牌也能改权限」。
//
// 守卫（system.maintain）在路由上，但控制器自己也从 ctx 取操作者——
// 那个 ActorFrom 是写审计的硬依赖（审计要记 actor_id 与脱敏用户名）。
// 少这一道的话，一次未认证的改动会留下一条没有操作者的审计记录。
func TestPolicyEndpoint_未登录一律401(t *testing.T) {
	s := withPolicyStore(t)
	ctx := newPolicyCtx(t, models.User{}, map[string]string{"role": "director"},
		map[string]any{"permissions": []any{rbac.PermLogView}})

	resp := replyOf(t, ctx, (NewRbacController()).UpdateRolePermissions(ctx))

	if resp.status != 401 {
		t.Errorf("未登录应返回 401，实际 %d", resp.status)
	}
	if len(s.audit) != 0 {
		t.Errorf("未登录的请求不该落审计（有操作者才落得出来），实际 %v", s.audit)
	}
}

// TestPolicyEndpoint_空权限列表是合法请求 防的是「把合法状态报成格式错误」。
//
// {"permissions":[]} 意为「把这个角色的权限全部收走」。它虽然危险
// （那个角色登录后什么都点不了），但它是**合法**的语义：把它报成 400
// 会让前端与运维去查请求格式，而真正的原因是有人真的提交了空集合。
// 该不该允许由 matrixWarnings 里的告警去提示，不是由请求解析去拦。
func TestPolicyEndpoint_空权限列表是合法请求(t *testing.T) {
	s := withPolicyStore(t)
	before := s.granted(models.RoleDirector)
	if before == "" {
		t.Fatal("前置条件不成立：导播本该持有若干权限")
	}

	ctx := newPolicyCtx(t, user(1, models.RoleSuperAdmin),
		map[string]string{"role": "director"},
		map[string]any{"permissions": []any{}})
	resp := replyOf(t, ctx, (NewRbacController()).UpdateRolePermissions(ctx))

	if resp.status != 200 {
		t.Fatalf("空权限集合应被接受，实际 %d：%v", resp.status, resp.body)
	}
	if got := rbac.PermissionsOf(models.RoleDirector); len(got) != 0 {
		t.Errorf("导播应已没有任何权限，实际仍有 %v", got)
	}
	// 但必须吵出来：这个角色现在什么都点不了，现场只看得见「账号坏了」。
	warnings := rbac.Warnings()
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "导播") {
			found = true
		}
	}
	if !found {
		t.Errorf("清空一个角色的权限必须在 warnings 里吵出来，实际 %v", warnings)
	}
}

// TestShowPolicy_响应结构就是前端契约 防的是「字段名悄悄改了」。
//
// 这份 JSON 是前后端之间唯一的契约，没有任何类型替它兜着（前端是手写的
// fetch）。所以字段名、类型、以及「空集合必须是 [] 而不是 null」都要在这里
// 钉住——null 会让前端的 for...of 直接抛异常，而那种崩溃在现场表现为
// 「权限页面整个白屏」，排查方向会跑去查构建。
func TestShowPolicy_响应结构就是前端契约(t *testing.T) {
	withPolicyStore(t)

	resp := callShowPolicy(t)
	if resp.status != 200 {
		t.Fatalf("应返回 200，实际 %d", resp.status)
	}

	for _, field := range []string{"source", "warnings", "permissions", "roles"} {
		if _, ok := resp.body[field]; !ok {
			t.Errorf("响应缺少字段 %q", field)
		}
	}
	if resp.body["source"] != rbac.SourceDatabase {
		t.Errorf("source 应为 %q，实际 %v", rbac.SourceDatabase, resp.body["source"])
	}
	// 空集合必须是空数组而不是 nil：nil 会被 JSON 编成 null。
	if resp.body["warnings"] == nil {
		t.Error("warnings 不能是 nil（JSON 会编成 null，前端遍历时会抛异常）")
	}

	permissions, ok := resp.body["permissions"].([]any)
	if !ok {
		t.Fatalf("permissions 的类型不对：%T", resp.body["permissions"])
	}
	if len(permissions) != len(rbac.AllPermissions()) {
		t.Errorf("permissions 应有 %d 项，实际 %d", len(rbac.AllPermissions()), len(permissions))
	}
	var maintain map[string]any
	for _, item := range permissions {
		p, isMap := item.(map[string]any)
		if !isMap {
			t.Fatalf("permissions 的一项不是对象：%T", item)
		}
		if name, _ := p["name"].(string); name == "" {
			t.Errorf("权限项缺少 name：%v", p)
		}
		if label, _ := p["label"].(string); label == "" {
			t.Errorf("权限项缺少 label：%v", p)
		}
		// holders 必须是数组（不是 null）：前端要遍历它来渲染角色列。
		if _, isList := p["holders"].([]any); !isList {
			t.Errorf("权限 %v 的 holders 应是数组（null 会让前端遍历时抛异常）：%v", p["name"], p["holders"])
		}
		// protected 必须存在，值是布尔。缺了这个字段时前端会把所有格子画成可点，
		// 而它的症状是「用户点了才被拒」——比画成灰色糟糕得多。
		if _, isBool := p["protected"].(bool); !isBool {
			t.Errorf("权限 %v 的 protected 应是布尔：%v", p["name"], p["protected"])
		}
		if p["name"] == rbac.PermSystemMaintain {
			maintain = p
		}
	}
	if maintain == nil {
		t.Fatal("permissions 里没有 system.maintain")
	}
	if maintain["protected"] != true {
		t.Error("system.maintain 的 protected 应为 true——界面靠它把那一格画成不可点")
	}
	holders, _ := maintain["holders"].([]any)
	if len(holders) != 1 || holders[0] != string(models.RoleSuperAdmin) {
		t.Errorf("system.maintain 的 holders 应只有超管，实际 %v", maintain["holders"])
	}

	roles, ok := resp.body["roles"].([]any)
	if !ok {
		t.Fatalf("roles 的类型不对：%T", resp.body["roles"])
	}
	if len(roles) != len(models.AllRoles()) {
		t.Errorf("roles 应有 %d 个，实际 %d", len(models.AllRoles()), len(roles))
	}
	var superAdmin map[string]any
	for _, item := range roles {
		r, isMap := item.(map[string]any)
		if !isMap {
			t.Fatalf("roles 的一项不是对象：%T", item)
		}
		if value, _ := r["value"].(string); value == "" {
			t.Errorf("角色项缺少 value：%v", r)
		}
		if label, _ := r["label"].(string); label == "" {
			t.Errorf("角色项缺少 label：%v", r)
		}
		if level, _ := r["level"].(float64); level == 0 {
			t.Errorf("角色项缺少 level：%v", r)
		}
		if _, isBool := r["protected"].(bool); !isBool {
			t.Errorf("角色 %v 的 protected 应是布尔：%v", r["value"], r["protected"])
		}
		if _, isList := r["grants"].([]any); !isList {
			t.Errorf("角色 %v 的 grants 应是数组（null 会让前端遍历时抛异常）", r["value"])
		}
		if r["value"] == string(models.RoleSuperAdmin) {
			superAdmin = r
		}
	}
	if superAdmin == nil {
		t.Fatal("roles 里没有 super_admin")
	}
	if superAdmin["protected"] != true {
		t.Error("super_admin 的 protected 应为 true")
	}
	if grants, _ := superAdmin["grants"].([]any); len(grants) != len(rbac.AllPermissions()) {
		t.Errorf("超管应持有全部 %d 项权限，实际 %d", len(rbac.AllPermissions()), len(grants))
	}
}

// TestShowPolicy_退回内嵌时source必须说清楚 防的是「以为自己在改数据库」。
//
// embedded 状态下改动仍然会写进数据库，但要等下次重载才生效，而当前生效的
// 还是文件里的那一版。不显示 source 的话，管理员会在「保存成功」之后继续
// 按旧策略排查问题——而真正的原因是策略还没重新装载。
func TestShowPolicy_退回内嵌时source必须说清楚(t *testing.T) {
	s := withPolicyStore(t)
	s.mu.Lock()
	s.fail = fmt.Errorf("no such table: role_permissions")
	s.mu.Unlock()

	rbac.Bootstrap()
	resp := callShowPolicy(t)

	if resp.body["source"] != rbac.SourceEmbedded {
		t.Errorf("策略表不可用时 source 应为 %q，实际 %v", rbac.SourceEmbedded, resp.body["source"])
	}
	// 且仍然可用：退回不能变成全体失权。
	if !rbac.Can(models.RoleSuperAdmin, rbac.PermSystemMaintain) {
		t.Error("退回内嵌策略后超管应仍持有系统维护权限")
	}
}

// TestPolicyEndpoint_没有新增或没有取消时给的是空数组而不是null 防的是前端崩在成功那一瞬。
//
// encoding/json 把 Go 的 nil 切片编成 null，而 null 在 JS 里既没有 .length
// 也没有 .map：前端只要写一句 `res.granted.length` 就会在**保存成功的那一瞬**
// 抛 TypeError，页面崩掉、还停在编辑态——服务端已经改了，界面说「报错了」。
// 那种症状会把排查方向彻底带偏（看起来像后端改崩了，其实后端返回的是 200）。
//
// 这条用例盯的是**契约本身**：即便调用方忘了归一，JSON 里也必须是 []。
// 前端那道 toNameList/normaliseRolePermissions 是补丁，防线必须落在这一侧。
//
// 两种「空」都要覆盖，因为它们落在不同的分支上：
//   - 什么都没变：granted 与 revoked 同时为空
//   - 只新增不取消：revoked 为空而 granted 非空
func TestPolicyEndpoint_没有新增或没有取消时给的是空数组而不是null(t *testing.T) {
	cases := []struct {
		name       string
		wantGrant  string
		wantRevoke string
		build      func(t *testing.T) []string
	}{
		{
			name: "什么都没变（两个都该是空数组）",
			build: func(t *testing.T) []string {
				return append([]string(nil), rbac.PermissionsOf(models.RoleDirector)...)
			},
		},
		{
			name:       "只新增不取消（revoked 该是空数组）",
			wantGrant:  rbac.PermUserView,
			wantRevoke: "",
			build: func(t *testing.T) []string {
				return append(append([]string(nil), rbac.PermissionsOf(models.RoleDirector)...), rbac.PermUserView)
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := withPolicyStore(t)
			ctx := newPolicyCtx(t, user(1, models.RoleSuperAdmin),
				map[string]string{"role": "director"},
				map[string]any{"permissions": toAny(c.build(t))})
			resp := replyOf(t, ctx, (NewRbacController()).UpdateRolePermissions(ctx))
			if resp.status != 200 {
				t.Fatalf("应返回 200，实际 %d：%v", resp.status, resp.body)
			}

			// 响应体：字段必须在，且必须是 JSON 数组而不是 null。
			assertJSONListNotNull(t, "响应体 granted", resp.body, "granted")
			assertJSONListNotNull(t, "响应体 revoked", resp.body, "revoked")
			if got := strings.Join(jsonList(resp.body["granted"]), ","); got != c.wantGrant {
				t.Errorf("granted 应为 %q，实际 %q", c.wantGrant, got)
			}
			if got := strings.Join(jsonList(resp.body["revoked"]), ","); got != c.wantRevoke {
				t.Errorf("revoked 应为 %q，实际 %q", c.wantRevoke, got)
			}

			// 审计：同一组值进 audit_logs.detail。它是给人复盘用的，
			// 但同样会被程序读，所以形状必须一样。
			if len(s.audit) != 1 {
				t.Fatalf("应落一条审计，实际 %d 条：%v", len(s.audit), s.audit)
			}
			for field, want := range map[string]string{
				"granted": c.wantGrant, "revoked": c.wantRevoke,
			} {
				v, present := s.audit[0].detail[field]
				if !present {
					t.Errorf("审计 detail 缺少 %q", field)
					continue
				}
				// ⚠️ 不能只判 v == nil：把一个**有类型的** nil 切片装进 any
				// 之后，接口本身并不等于 nil（它带着 []string 这个类型），
				// 于���那种写法会安静地放过真正的 bug。真正要问的是
				// 「序列化之后是不是 null」——那正是 audit.Write 会做的事。
				assertMarshalsToArrayNotNull(t, "审计 detail 的 "+field, v)
				list, isList := v.([]string)
				if !isList {
					t.Errorf("审计 detail 的 %q 应为 []string，实际 %T", field, v)
					continue
				}
				if got := strings.Join(list, ","); got != want {
					t.Errorf("审计 detail 的 %q 应为 %q，实际 %q", field, want, got)
				}
			}
		})
	}
}

// assertJSONListNotNull 断言 body[field] 是 JSON 数组而不是 null。
//
// 走的是 policyResponse.Json 已经做过的那次 marshal/unmarshal，所以看到的就是
// 前端看到的形状：null 解回 map[string]any 就是 nil，而 [] 解回 []any{}。
func assertJSONListNotNull(t *testing.T, what string, body map[string]any, field string) {
	t.Helper()
	v, present := body[field]
	if !present {
		t.Errorf("%s 缺少字段 %q", what, field)
		return
	}
	if v == nil {
		t.Errorf("%s 的 %q 是 null——前端对它读 .length/.map 会直接抛 TypeError，"+
			"必须给 [] （空集合是合法状态，不是「没有值」）", what, field)
		return
	}
	if _, isList := v.([]any); !isList {
		t.Errorf("%s 的 %q 应是 JSON 数组，实际类型 %T", what, field, v)
	}
}

// TestPolicyEndpoint_请求体畸形时审计里的requested也是空数组 同一个坑的另一半。
//
// 被拒绝的那条审计里有一项 requested，装着「他提交了什么」。而请求体畸形时
// 它拿不到任何权限——传进来的就是 nil 切片，于是 audit_logs 里同样出现
// "requested":null。
//
// 这里没有改成「省略」：省掉之后，「他提交了空的」与「我们没记下来」在审计里
// 就再也分不开了，而这两件事要查的地方完全不同。给 [] 才能把两者分开，
// 也和 granted/revoked 的形状保持一致。
func TestPolicyEndpoint_请求体畸形时审计里的requested也是空数组(t *testing.T) {
	s := withPolicyStore(t)

	ctx := newPolicyCtx(t, user(1, models.RoleSuperAdmin),
		map[string]string{"role": "leader"}, map[string]any{"foo": "bar"})
	resp := replyOf(t, ctx, (NewRbacController()).UpdateRolePermissions(ctx))
	if resp.status != 400 {
		t.Fatalf("畸形请求体应返回 400，实际 %d：%v", resp.status, resp.body)
	}
	if len(s.audit) != 1 {
		t.Fatalf("应落一条审计，实际 %d 条：%v", len(s.audit), s.audit)
	}
	v, present := s.audit[0].detail["requested"]
	if !present {
		t.Fatal("审计 detail 缺少 requested——答不了「他试图干什么」")
	}
	assertMarshalsToArrayNotNull(t, "审计 detail 的 requested", v)
	if list, isList := v.([]string); !isList || len(list) != 0 {
		t.Errorf("requested 应为空的 []string，实际 %#v", v)
	}
}

// assertMarshalsToArrayNotNull 断言 v 序列化之后是一个 JSON 数组，而不是 null。
//
// **必须走序列化**，不能只判 v == nil：一个有类型的 nil 切片（`[]string(nil)`）
// 装进 any 之后接口并不等于 nil，所以 `v == nil` 会安静地放过真正的 bug。
// 而 null 正是这次要根除的那个形状——它进到 audit_logs.detail 与 HTTP 响应体里。
func assertMarshalsToArrayNotNull(t *testing.T, what string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s 无法序列化：%v", what, err)
	}
	if string(raw) == "null" {
		t.Errorf("%s 序列化后是 null 而不是 []——"+
			"消费方读它的 .length/.map 会直接抛 TypeError。"+
			"空集合是合法状态，必须以 [] 出现", what)
		return
	}
	if !strings.HasPrefix(string(raw), "[") {
		t.Errorf("%s 序列化后不是 JSON 数组：%s", what, raw)
	}
}

// TestPolicyEndpoint_审计记的是实际生效的那一组而不是请求里声称的那一组。
//
// 这是审计最容易说谎的一处：端点把请求里的 permissions 原样交给
// rbac.ApplyRolePermissions，而返回的 Change 是「实际生效的增删」。两者在
// 「部分失败」或「集合里有本来就有的项」时是不同的——
//
//	请求声称要把导播的权限改成 {log.view, user.view}，
//	而导播本来就有 log.view → 真正新增的只有 user.view。
//
// 审计必须记后者。记前者的话，「谁多了一项能力」这个问题就答不出来了。
func TestPolicyEndpoint_审计记的是实际生效的那一组而不是请求里声称的那一组(t *testing.T) {
	s := withPolicyStore(t)

	before := append([]string(nil), rbac.PermissionsOf(models.RoleDirector)...)
	if len(before) < 2 {
		t.Fatalf("前提不成立：导播本该持有多项权限，实际 %v", before)
	}
	// 请求里**重复**一遍已经有的项，再加一项它没有的。
	// 「重复」是为了连「去重之后才算差集」这一点也一起钉住。
	submitted := append([]string{}, before...)
	submitted = append(submitted, before...)
	submitted = append(submitted, rbac.PermUserView)

	ctx := newPolicyCtx(t, user(1, models.RoleSuperAdmin),
		map[string]string{"role": "director"},
		map[string]any{"permissions": toAny(submitted)})
	resp := replyOf(t, ctx, (NewRbacController()).UpdateRolePermissions(ctx))
	if resp.status != 200 {
		t.Fatalf("应返回 200，实际 %d：%v", resp.status, resp.body)
	}

	// 审计里的 granted 必须等于「生效前 → 生效后」的差集，
	// 而不是请求里多出来的那几项。
	after := rbac.PermissionsOf(models.RoleDirector)
	wantGranted, wantRevoked := diffOf(before, after)
	if len(s.audit) != 1 {
		t.Fatalf("应落一条审计，实际 %d 条：%v", len(s.audit), s.audit)
	}
	gotGranted, _ := s.audit[0].detail["granted"].([]string)
	gotRevoked, _ := s.audit[0].detail["revoked"].([]string)
	if strings.Join(sorted(gotGranted), ",") != strings.Join(wantGranted, ",") {
		t.Errorf("审计的 granted 应是实际新增的 %v（请求里声称的是 %v），实际 %v",
			wantGranted, submitted, gotGranted)
	}
	if strings.Join(sorted(gotRevoked), ",") != strings.Join(wantRevoked, ",") {
		t.Errorf("审计的 revoked 应是实际取消的 %v，实际 %v", wantRevoked, gotRevoked)
	}
	// 响应体与审计必须说的是同一件事，否则前端显示与事后追溯会对不上。
	respGranted := strings.Join(sorted(jsonList(resp.body["granted"])), ",")
	if respGranted != strings.Join(wantGranted, ",") {
		t.Errorf("响应里的 granted 应与实际生效一致：期望 %s，实际 %s",
			strings.Join(wantGranted, ","), respGranted)
	}
}

// TestPolicyEndpoint_审计写不进去也不能让请求失败 防的是「制造审计失败来抹掉痕迹」。
//
// 审计是旁路：app/audit.Write 自己吞掉写库错误（只打日志），因为它的判断是
// 「不能因为它挂了就让正在直播的系统做不了操作」。这条用例把同一件事在端点
// 这一层钉住——把落地口换成一个什么都不记的实现，响应必须**一字不变**。
//
// 反过来若审计失败导致请求 500/503，那它就成了一个攻击面：想让自己的痕迹
// 消失的人只要先把 audit_logs 弄坏就行。
func TestPolicyEndpoint_审计写不进去也不能让请求失败(t *testing.T) {
	s := withPolicyStore(t)
	// 模拟「审计完全写不进去」：落地口什么都不做，且**不报错**
	//（auditSink 的签名里根本没有返回值，真实实现只能吞）。
	saved := auditSink
	auditSink = func(contractshttp.Context, models.User, audit.Record) {}
	t.Cleanup(func() { auditSink = saved })

	grant := append(append([]string(nil), rbac.PermissionsOf(models.RoleDirector)...), rbac.PermUserView)
	ctx := newPolicyCtx(t, user(1, models.RoleSuperAdmin),
		map[string]string{"role": "director"},
		map[string]any{"permissions": toAny(grant)})
	resp := replyOf(t, ctx, (NewRbacController()).UpdateRolePermissions(ctx))

	if resp.status != 200 {
		t.Fatalf("审计写不进去时请求仍应成功，实际 %d：%v", resp.status, resp.body)
	}
	// 而且改动**真的**生效了：审计挂掉不该顺带把权限改动也吞掉。
	if !rbac.Can(models.RoleDirector, rbac.PermUserView) {
		t.Error("审计失败不该影响权限改动本身的生效")
	}
	if got := s.granted(models.RoleDirector); !strings.Contains(got, rbac.PermUserView) {
		t.Errorf("审计失败不该让改动没落库，实际 %v", got)
	}
}

// TestPolicyEndpoint_被拒绝的尝试不会被审计失败吞成成功。
//
// 与上一条是同一个不变量的反面：审计是旁路，不影响响应；但**判定**必须仍然
// 决定响应。这里断言 403 不会因为审计口安静而变成 200——否则「审计写不进去」
// 就成了「权限改动被放行」，那比审计丢失严重得多。
func TestPolicyEndpoint_被拒绝的尝试不会被审计失败吞成成功(t *testing.T) {
	withPolicyStore(t)
	saved := auditSink
	auditSink = func(contractshttp.Context, models.User, audit.Record) {}
	t.Cleanup(func() { auditSink = saved })

	ctx := newPolicyCtx(t, user(1, models.RoleSuperAdmin),
		map[string]string{"role": "admin"},
		map[string]any{"permissions": toAny([]string{rbac.PermSystemMaintain})})
	resp := replyOf(t, ctx, (NewRbacController()).UpdateRolePermissions(ctx))

	if resp.status != 403 {
		t.Fatalf("触碰受保护权限仍应返回 403，实际 %d：%v", resp.status, resp.body)
	}
	if rbac.Can(models.RoleAdmin, rbac.PermSystemMaintain) {
		t.Error("管理员拿到了系统维护权限")
	}
}

// diffOf 算「从 before 到 after 的差集」，即审计里该记的 granted/revoked。
func diffOf(before, after []string) (granted, revoked []string) {
	have := map[string]bool{}
	for _, p := range before {
		have[p] = true
	}
	want := map[string]bool{}
	for _, p := range after {
		want[p] = true
	}
	for _, p := range after {
		if !have[p] {
			granted = append(granted, p)
		}
	}
	for _, p := range before {
		if !want[p] {
			revoked = append(revoked, p)
		}
	}
	return granted, revoked
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func mustReject(t *testing.T, role string, body map[string]any) *policyResponse {
	t.Helper()
	withPolicyStore(t)
	ctx := newPolicyCtx(t, user(1, models.RoleSuperAdmin),
		map[string]string{"role": role}, body)
	resp := replyOf(t, ctx, (NewRbacController()).UpdateRolePermissions(ctx))
	if resp.status == 200 {
		t.Fatalf("本该被拒绝，实际 200：%v", resp.body)
	}
	return resp
}

func callShowPolicy(t *testing.T) *policyResponse {
	t.Helper()
	ctx := newPolicyCtx(t, user(1, models.RoleSuperAdmin), nil, nil)
	return replyOf(t, ctx, (NewRbacController()).ShowPolicy(ctx))
}

func toAny(list []string) []any {
	out := make([]any, 0, len(list))
	for _, s := range list {
		out = append(out, s)
	}
	return out
}

// jsonList 把 JSON 往返之后的 []any 还原成 []string，供断言比对。
//
// 响应体在 fake 里已经过一次 marshal/unmarshal（见 fakectx_test.go 的说明），
// 所以数组一定是 []any。直接跟 []string 比会得到一个类型不同的空值，
// 而那种失败信息（"实际 []"）会把人引到完全错误的方向。
func jsonList(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}
