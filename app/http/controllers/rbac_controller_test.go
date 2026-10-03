package controllers

import (
	"fmt"
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

// fromEmbedded 按 policy.csv 铺一份完整矩阵。
func (s *fakeStore) fromEmbedded() error {
	m, err := rbacEmbeddedMatrix()
	if err != nil {
		return err
	}
	s.seed(m)
	return nil
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
		// 还原成内嵌策略：app/rbac 包内其他用例断言的是那一份。
		rbac.SetStore(nil)
		_ = rbac.Reload()
	})
	return s
}

// rbacEmbeddedMatrix 从当前生效的策略反推一份完整矩阵，用来铺初始表。
//
// 走 rbac.View() 而不是直接读 policy.csv：View 是 app/rbac 唯一导出的读出口，
// 测试不引入第二条读策略的路——那种「测试自己解析一遍文件」的做法迟早会与
// 生产解析分叉，而分叉之后测试还在绿。
func rbacEmbeddedMatrix() (map[models.Role]map[string]bool, error) {
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
	return m, nil
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
