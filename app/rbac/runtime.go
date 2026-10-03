package rbac

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/casbin/casbin/v2"

	"smart-mzcmc/app/models"
)

// 本文件是策略的**运行时装载与写入**层：谁在什么时候把哪一份策略放进内存、
// 谁可以改它、改的时候必须过哪几关。
//
// ############################################################################
// # 为什么本包不导出 *casbin.Enforcer
// #
// # 之前 Default() 直接返回进程内那个 *casbin.Enforcer。那是个旁路：
// # 任何人都能 `rbac.Default().AddPolicy("admin", rbac.PermSystemMaintain)`，
// # protect.go 里的三条 Validate* 一道都不会跑，受保护规则当场失效——
// # 而且失效得毫无痕迹：没有请求被拒，没有日志，只有权限多了一项。
// #
// # 现在本包对外只暴露纯函数与几组只读视图（Can / Holders / PermissionsOf /
// # Snapshot / Source / Warnings）。Casbin 的写方法从包外不可达，于是
// # 「改动生效中的策略」这件事只有两个出口：
// #
// #   - Reload()               —— 从 role_permissions 重新装载
// #   - ApplyRolePermissions() —— 校验后写入再重载
// #
// # 两个都先过 protect.go 的 Validate*。bypass_test.go 会盯住这一点：
// # 它断言 rbac 包的导出符号里没有任何一个的签名提到 casbin。
// ############################################################################
//
// 三层装载路径，行为各不相同——**这个差异是刻意的**：
//
//	进程启动（init）  embedded policy.csv。加载失败 → Enforcer 为 nil →
//	                  Can 对一切返回 false（全部拒绝）。宁可当场不可用。
//	启动时（Bootstrap） 先试 role_permissions；表为空则播种一次；
//	                  表读不出来或装载后违反受保护规则 → **退回 embedded**
//	                  并在 warnings 里说清原因。理由：策略表坏掉不该让整套
//	                  系统起不来——直播不会因为一张策略表停摆。
//	运行中（Reload）   只从 role_permissions 装载；任何一步不通过都**不动**
//	                  内存里的策略，保留上一份可用的，并返回错误。
//	                  理由：运行中退回 embedded 是一次静默的全系统降权，
//	                  它比「这次改动没生效」严重得多。

// policyWriter 串行化**策略的写入**：ApplyRolePermissions 与 Reload 都走它。
//
// 为什么需要：ApplyRolePermissions 是「算差集 → 写库 → 重载 → 必要时回滚」
// 这一整串，中间有至少两次读策略表。没有它，两个管理员同时改权限会这样：
//
//	A 读到起点 S，算出自己相对 S 的增删
//	B 也读到起点 S，算出自己相对 S 的增删
//	A 写库、重载；B 写库、重载
//
// 于是**两次审计都说自己相对 S 改了某几项**，而实际发生的是 B 覆盖掉了 A。
// 出事故时要回答的第一个问题恰恰是「谁多了一项能力」，而审计在这条路径上
// 会答错。反过来，B 若能读到 A 刚写下的那份，他报的 revoked 就必然包含
// A 的那一项——那才是实情。
//
// 顺带解决的两件事：
//
//   - 写库与重载之间的窗口不再有两个写者交叉，SQLite 上「读库撞上另一个
//     写者的提交」而拿到 SQLITE_BUSY 的机会随之下降。
//   - 「改动生效了吗」变成一个**原子步骤**：要么整串做完且内存与库一致，
//     要么整串回滚。读路径（Can/View）不碰这把锁，所以受守卫的请求不会被
//     写路径卡住——锁只串行化本来就已经很稀有的写操作。
//
// 它**不是**用来防越权的：三条 Validate* 在拿锁之前就跑完了，那才是防越权的
// 那一层。这把锁只保证「写与写之间不打架」。
var policyWriter sync.Mutex

const (
	// SourceEmbedded 表示当前生效的策略来自 go:embed 的 policy.csv。
	//
	// 它同时覆盖两种情况：还没播种（首次启动、迁移没跑）以及数据库读不出来。
	// 界面必须把「当前策略来自 embedded」当成醒目状态显示——此时改权限会
	// 写进数据库但要等下次重载才生效，而下次重载之前策略还是文件里的。
	SourceEmbedded = "embedded"
	// SourceDatabase 表示当前生效的策略来自 role_permissions 表。
	SourceDatabase = "database"
)

// Bootstrap 在应用启动时装载策略。**它不会让启动失败。**
//
// 之所以不让启动失败：策略表读不出来（迁移没跑、磁盘只读、文件被锁）时，
// 正确的反应是退回内嵌 policy.csv 并把这件事吵到日志与界面上，而不是让一个
// 正在直播的系统起不来。内嵌 policy.csv 是过 code review 的纯数据，它本身就是
// 一份可用策略——启动失败换不来任何安全性，只换来停播。
//
// 装配点在本包之外（bootstrap/app.go 的 WithCallback）：只有那里知道
// 「框架已经起好、数据库连接已经就绪」这个时刻。
//
// 里面那个 recover 不是偷懒：facades.Orm() 在框架没完全装配好时会 panic，
// 而让一个权限模块的装配问题把整个服务带走是本末倒置。
func Bootstrap() {
	defer func() {
		if r := recover(); r != nil {
			installEmbedded(fmt.Sprintf("装载策略时发生意外（%v），已退回内嵌 policy.csv", r))
		}
	}()

	if store == nil {
		// 没有装配存储（单测直接跑、或某个精简部署）：就用文件策略。
		installEmbedded("未装配策略存储，当前使用内嵌 policy.csv")
		return
	}
	policyWriter.Lock()
	err := reloadLocked()
	policyWriter.Unlock()
	if err != nil {
		// 刻意把**原始错误**原样说出来，而不是笼统地说「策略表不可用」：
		// Reload 也会因为「库里的策略违反受保护规则」而失败（ErrCodeConflict），
		// 那不是表坏了，是有人绕过本项目改过数据。两种原因要查的地方完全不同，
		// 而启动时正是最不该含糊的时刻。
		installEmbedded("未能从 role_permissions 装载策略（" + err.Error() +
			"），已退回内嵌 policy.csv")
		return
	}
	log.Printf("[RBAC] 策略已从 %s 装载：%d 项权限 / %d 个角色",
		Source(), len(AllPermissions()), len(models.AllRoles()))
}

// Reload 从 role_permissions 重新装载策略并替换内存里生效的那一份。
//
// **fail-closed**：装载失败或装载后违反受保护规则时，内存里仍然是上一份可用
// 策略，绝不换成一份「能让所有人失去系统维护权限」的策略。只打日志是不够的——
// 违规策略已经在生效了，而 system.maintain 被削掉之后所有人都改不回来。
//
// 表为空时会先用 policy.csv 播种一次（见 seedIfEmpty）。这条让「初始化向导
// 跑完迁移但进程没重启」这种情况不需要人来管：下一个请求就会把表填上。
func Reload() error {
	policyWriter.Lock()
	defer policyWriter.Unlock()
	return reloadLocked()
}

// reloadLocked 是 Reload 的本体，**调用方必须已经持有 policyWriter**。
//
// 拆成两个是因为 ApplyRolePermissions 在自己的临界区里要重载：让 Reload
// 再去拿一次同一把锁就是自死锁。
func reloadLocked() error {
	s := currentStore()
	if s == nil {
		return &PolicyError{
			Code:    ErrCodeUnavailable,
			Message: "策略存储未装配，无法从数据库装载策略",
		}
	}

	matrix, err := s.Load()
	if err != nil {
		return &PolicyError{
			Code:    ErrCodeUnavailable,
			Message: "读取 role_permissions 失败：" + err.Error(),
		}
	}
	matrix, err = seedIfEmpty(s, matrix)
	if err != nil {
		return &PolicyError{Code: ErrCodeUnavailable, Message: err.Error()}
	}

	candidate, err := enforcerFromMatrix(matrix)
	if err != nil {
		return &PolicyError{Code: ErrCodeUnavailable, Message: err.Error()}
	}

	warnings := matrixWarnings(matrix)
	violations, notes := protectedRuleFindings(candidate)
	if len(violations) > 0 {
		for _, v := range violations {
			log.Printf("[RBAC] 拒绝装载：策略违反受保护规则 —— %s", v)
		}
		return &PolicyError{
			Code: ErrCodeConflict,
			Message: "策略违反受保护规则，本次装载已被拒绝，内存中仍是上一份可用策略：" +
				strings.Join(violations, "；"),
		}
	}

	active.Lock()
	active.enforcer = candidate
	active.source = SourceDatabase
	active.warnings = append(notes, warnings...)
	active.Unlock()
	auditPolicy(candidate)
	return nil
}

// installEmbedded 装上内嵌 policy.csv，并记下原因。
//
// 它是「策略表坏了也不要停播」这条设计里唯一的退路，所以必须把自己降级这件事
// 说出来：日志里一行、GET /api/rbac/policy 的 warnings 里一条。悄悄退回是
// 最坏的一种——它会让管理员以为自己在改数据库里的策略，而其实下一次重载就
// 会把它们全部覆盖掉。
//
// ⚠️ 它**真的会把内嵌策略装进内存**，而不是只把 source 标成 embedded。
// 只标不装是一句谎：Bootstrap 在表读不出来时调用它，而那一刻内存里若是上一次
// 从库里装来的策略，那么报出来的 source（embedded）与实际生效的那份会对不上。
// 那种谎的代价是管理员按错误的依据做判断，而这里只要多构造一个 Enforcer
// （内嵌那份在任何路径下都不会超过 41 行）就能让它变成真的。
//
// 内嵌策略自己加载失败时**保留原来那一份**：fail-closed 要保留「上一份可用的」，
// 而不是把自己清空成 nil（那会让 Can 对一切返回 false，把一次退避变成停播）。
func installEmbedded(reason string) {
	log.Printf("[RBAC] %s", reason)
	e, err := load(modelConf, policyCSV)
	if err != nil {
		log.Printf("[RBAC] 内嵌策略加载失败，保留当前生效的策略不动：%v", err)
	}
	notes := embeddedWarnings(e)
	active.Lock()
	if err == nil {
		active.enforcer = e
	}
	active.source = SourceEmbedded
	active.warnings = append([]string{reason}, notes...)
	active.Unlock()
}

// embeddedWarnings 返回内嵌策略的静态告警（受保护规则的说明）。
func embeddedWarnings(e *casbin.Enforcer) []string {
	_, notes := protectedRuleFindings(e)
	return notes
}

// Source 返回当前生效的策略来自哪里：SourceDatabase 或 SourceEmbedded。
func Source() string {
	active.RLock()
	defer active.RUnlock()
	return active.source
}

// Warnings 返回当前策略上人需要知道的话，原样交给界面显示。
//
// 里面装的是：受保护规则为什么受保护（界面靠它把那些格子画成不可点）、
// 策略表里的脏数据、以及「已经退回内嵌 policy.csv」这件事。刻意做成原文透传
// 而不是结构化字段——这些句子本来就只有人能读，拆成字段反而会有人在界面上
// 拼一句更短但信息更少的版本。
func Warnings() []string {
	active.RLock()
	defer active.RUnlock()
	if len(active.warnings) == 0 {
		return []string{}
	}
	return append([]string(nil), active.warnings...)
}

// PermissionView 是一项权限在界面上的样子。
type PermissionView struct {
	Name      string   `json:"name"`
	Label     string   `json:"label"`
	Protected bool     `json:"protected"`
	Holders   []string `json:"holders"`
}

// RoleView 是一个角色在界面上的样子。
type RoleView struct {
	Value     string   `json:"value"`
	Label     string   `json:"label"`
	Level     int      `json:"level"`
	Protected bool     `json:"protected"`
	Grants    []string `json:"grants"`
}

// PolicyView 是当前生效策略的完整快照，供权限编辑界面一屏渲染。
//
// 刻意把「界面要的东西」整成一个结构体，而不是让端点去分别问
// Source / Warnings / Can / PermissionsOf 再自己拼 map：那样拼出来的字段
// 组合没有任何一处被断言过，而这份 JSON 就是前后端之间唯一的契约。
// 放在 app/rbac 里还顺带保证了 holders 与 grants 是**从内存里生效的那一份**
// 现算的——从策略表读出来再猜的话，两者一旦不一致，界面就会让人以为某项
// 权限没生效（于是反复勾选），而真相是它生效了、只是被脏行挡住，
// 那是完全相反的排查方向。
type PolicyView struct {
	Source      string           `json:"source"`
	Warnings    []string         `json:"warnings"`
	Permissions []PermissionView `json:"permissions"`
	Roles       []RoleView       `json:"roles"`
}

// View 返回当前生效策略的完整快照。
//
// ⚠️ 这里**一次性**取快照（take），然后所有字段都从那一份上算。写成
// 「Source() + Warnings() + 96 次 Can + 8 次 PermissionsOf」在功能上也对，
// 但那是一次字段一次取锁：一次 Reload 落在中间就会拼出一份半份快照——
// source 是旧策略的、holders 是新策略的。它不是数据竞争（race detector 看不见），
// 而后果是界面渲染出来的矩阵与实际放行对不上，管理员会照着它反复勾选。
//
// concurrency_test.go 的 TestView_快照不掺半份策略 钉住这一条。
//
// ⚠️ **刻意不去优化成「按 permissionsOf 推 holders」**：那确实快一个数量级
// （8 次 PermissionsOf ≈ 14µs，而 96 次 Enforce ≈ 1.6ms，见 bench_test.go），
// 但它把「从生效中的策略现算」换成了「在 Go 侧复现一遍 model.conf 的语义」。
// 本包的 model.conf 是全等匹配，所以两者今天完全等价；哪天它变成 keyMatch，
// permissionsOf 与 Enforce 就会分叉，而界面照旧显示正常——那正是这段代码
// 存在的理由要防的事。1.6ms 是一次页面打开的成本，不是热路径，热路径是
// Can（20µs/次），而 Can 不需要现算整张表。
func View() PolicyView {
	snap := take()
	view := PolicyView{
		Source:      snap.source,
		Warnings:    snap.warnings,
		Permissions: make([]PermissionView, 0, len(permissionNames)),
		Roles:       make([]RoleView, 0, len(models.AllRoles())),
	}
	if view.Warnings == nil {
		// nil 会被 JSON 编成 null，前端 for...of 遇到 null 直接报错。
		view.Warnings = []string{}
	}

	for _, perm := range AllPermissions() {
		holders := make([]string, 0, len(models.AllRoles()))
		for _, role := range models.AllRoles() {
			if snap.enforce(role, perm) {
				holders = append(holders, string(role))
			}
		}
		view.Permissions = append(view.Permissions, PermissionView{
			Name:      perm,
			Label:     Label(perm),
			Protected: IsProtected(perm),
			Holders:   holders,
		})
	}

	for _, role := range models.AllRoles() {
		grants := snap.permissionsOf(role)
		if grants == nil {
			// 同上：权限为空是合法状态（虽然不健康），而「读不出来」由
			// warnings 去说。
			grants = []string{}
		}
		view.Roles = append(view.Roles, RoleView{
			Value:     string(role),
			Label:     role.Label(),
			Level:     role.Level(),
			Protected: IsProtectedRole(role),
			Grants:    grants,
		})
	}
	return view
}

// matrixWarnings 报告矩阵里那些「不致命但必须有人知道」的问题。
//
// 只吵不改：这些都不是越权，擅自替人修一份策略比留着它更危险（谁能改、
// 改成什么样，属于审计范围，不该由一次启动悄悄决定）。真正会锁死系统的
// 违规由 protectedRuleFindings 判，并导致拒绝装载。
func matrixWarnings(matrix Matrix) []string {
	var out []string
	for _, role := range models.AllRoles() {
		cells, ok := matrix[role]
		if !ok {
			out = append(out, fmt.Sprintf("角色 %s 在策略表里没有任何记录，它当前没有任何权限", role.Label()))
			continue
		}
		for _, perm := range AllPermissions() {
			if _, ok := cells[perm]; !ok {
				out = append(out, fmt.Sprintf("角色 %s 缺少权限 %s 那一行，按未授予处理", role.Label(), perm))
			}
		}
		granted := 0
		for _, v := range cells {
			if v {
				granted++
			}
		}
		if granted == 0 {
			out = append(out, fmt.Sprintf("角色 %s 当前一项权限都没有——多半是误操作，"+
				"这个角色登录后什么都点不了", role.Label()))
		}
	}
	for role, cells := range matrix {
		if !role.Valid() {
			out = append(out, fmt.Sprintf("策略表里出现了未知角色 %q，它对任何人都不会生效", role))
			continue
		}
		for perm := range cells {
			if !Known(perm) {
				out = append(out, fmt.Sprintf("策略表里出现了未知权限 %q（角色 %s），没有路由会要求它",
					perm, role.Label()))
			}
		}
	}
	return out
}

// embeddedMatrix 从**内嵌 policy.csv 装载出来的 Enforcer** 反推出完整矩阵。
//
// 刻意不另写一个 CSV 解析器：那样就等于有了两处对同一份文件的理解，
// 而它们一旦分叉（少认一行、多认一行），播种出来的数据库就会与仓库里的
// 策略静悄悄地不同——而 policy.csv 正是 code review 时被人看的那一份。
// 从 Enforcer 反推则保证「播下去的」与「跑着的」逐字一致。
func embeddedMatrix() (Matrix, error) {
	// 刻意**重新 load 一次**而不是读 currentEnforcer()：那个是「当前生效的
	// 策略」，可能来自数据库，也可能已经被在线编辑改过。而播种用的模板必须
	// 永远等于仓库里那一份 policy.csv。
	//
	// 这两者混起来的后果很具体：现场改过权限之后，某次播种会把「现场改过的
	// 那一版」再写一遍，于是「恢复出厂设置」这件事变成了「把上一次编辑固化下来」。
	e, err := load(modelConf, policyCSV)
	if err != nil {
		return nil, fmt.Errorf("加载内嵌策略：%w", err)
	}
	rules, err := e.GetPolicy()
	if err != nil {
		return nil, fmt.Errorf("读取内嵌策略：%w", err)
	}
	return matrixFromRules(rules), nil
}

// matrixFromRules 把 Casbin 的策略行翻成完整矩阵：已授予的格为 true，
// 其余每一格都显式写成 false。
func matrixFromRules(rules [][]string) Matrix {
	matrix := Matrix{}
	for _, role := range models.AllRoles() {
		cells := make(map[string]bool, len(permissionNames))
		for _, perm := range AllPermissions() {
			cells[perm] = false
		}
		matrix[role] = cells
	}
	for _, rule := range rules {
		if len(rule) != 2 {
			continue
		}
		role := models.Role(rule[0])
		cells, ok := matrix[role]
		if !ok {
			// 未知角色：照样建一格出来，让 matrixWarnings 去报它，
			// 而不是在这里悄悄丢掉——丢掉之后策略表里就再没有它的痕迹，
			// 那条「未知角色」告警也跟着消失了。
			cells = map[string]bool{}
			matrix[role] = cells
		}
		cells[rule[1]] = true
	}
	return matrix
}

// matrixToPolicyText 把矩阵翻回 Casbin 认得的策略文本。
//
// 顺序固定（角色从高到低、权限按声明顺序）而不是跟着 map 的随机序走：
// 日志与错误信息里出现的行序每次都一样，才能拿两段日志做对照。
func matrixToPolicyText(matrix Matrix) string {
	var sb strings.Builder
	for _, role := range models.AllRoles() {
		cells := matrix[role]
		for _, perm := range AllPermissions() {
			if cells[perm] {
				sb.WriteString("p, " + string(role) + ", " + perm + "\n")
			}
		}
	}
	return sb.String()
}

func enforcerFromMatrix(matrix Matrix) (*casbin.Enforcer, error) {
	return load(modelConf, matrixToPolicyText(matrix))
}

// Change 是一次权限变更的结果。
//
// ⚠️ Granted 与 Revoked **必须**始终是非 nil 切片，即使本次什么都没变。
//
// 这不是洁癖：encoding/json 把 nil 切片编成 null，而它们会同时出现在
// PUT /api/rbac/roles/:role/permissions 的**响应体**与 audit_logs 的 detail 里。
// null 在 JS 里既没有 .length 也没有 .map，于是前端一句 `res.granted.length`
// 会在**保存成功的那一瞬**抛 TypeError——后端返回的是 200、策略已经改了，
// 而界面停在编辑态说「报错了」。那种症状会把排查方向彻底带偏：看起来像
// 后端把系统改崩了，真正的原因只是「这次没有新增」。
//
// 空集合是**合法且有意义的**状态（这次确实什么都没变），所以它必须以 []
// 出现，而不是以「没有值」出现。构造点只有 ApplyRolePermissions 一处，
// 那里的字面量必须带 make(..., 0)；runtime_test.go 的
// TestApplyRolePermissions_没有变化时也返回非nil切片 钉住这一点。
type Change struct {
	// Role 被改动的角色。
	Role models.Role
	// Granted 本次新授予的权限。永不为 nil。
	Granted []string
	// Revoked 本次被取消的权限。永不为 nil。
	Revoked []string
}

// Empty 报告这次变更什么都没动。
func (c *Change) Empty() bool {
	return c == nil || (len(c.Granted) == 0 && len(c.Revoked) == 0)
}

// ApplyRolePermissions 把 role 的权限集合整体改成 granted 列出的那些。
//
// 语义是「改成这样」而不是「追加」——前端渲染的是一整张勾选表，
// 提交的就是全量；用追加语义的话，取消一项权限永远传不上去。
//
// 三条硬约束，逐条对应 protect.go 里的判定：
//  1. 角色本身先过 ValidateRemoveRole。不是因为这里会改角色，而是因为
//     受保护角色（super_admin）的任何改动都不该通过这个入口——
//     将来这个函数被扩展成「同时改角色」时，这里已经是闸门。
//  2. 每一项新授予过 ValidateGrant（顺带挡住未知权限与「把受保护权限给
//     非受保护角色」）。
//  3. 每一项被取消过 ValidateRevoke（受保护权限一律不可撤销，哪怕是从
//     持有者自己身上取消）。
//
// **任一条不过就整次拒绝，一个字节都不写**：这是「线上没有半成品策略」的
// 唯一保证。逐条写、逐条报错看起来更友好，但会出现「改了 3 项成功、第 4 项
// 被拒」的半成品，而受保护规则一旦被半成品破坏，系统就锁死了。
//
// 三条 Validate* 在**拿写锁之前**就跑完了（那是纯函数，快且不会阻塞别人）；
// 拿锁之后那段是「重载 → 算差集 → 写库 → 重载」，中间不 sleep、不做 IO
// 之外的事，所以它对别的写者的阻塞时间就是一次库读加一次 Enforcer 构造。
//
// 写库之后还会 Reload 一次并再核对受保护规则：万一库里的其他角色本来就是坏的
// （手改过），装载会失败，这时把本次写入回滚并返回错误——宁可这次改动没生效，
// 也不能让内存里的策略与库里的策略对不上。
func ApplyRolePermissions(role models.Role, granted []string) (*Change, error) {
	// 角色本身。这一步挡住了「对 super_admin 的任何改动」。
	if err := ValidateRemoveRole(role); err != nil {
		return nil, err
	}

	// 逐条校验**原始请求里的每一项**，在去重之前。
	//
	// 顺序很重要：先去重再校验的话，一个不认识的权限名会被「按声明顺序归一」
	// 那一步悄悄丢掉——请求看起来成功了，而那一项从来没被写进任何地方。
	// 现场表现是「我明明勾了它，保存后没有」，且日志里一句异常都没有。
	for _, perm := range granted {
		if err := ValidateGrant(role, perm); err != nil {
			return nil, err
		}
	}

	// 去重并按声明顺序归一。不归一的话，同一项权限传三次就会在
	// 变更结果里出现三次，审计记录看起来像是改了三次。
	want := map[string]bool{}
	for _, perm := range granted {
		want[perm] = true
	}
	normalized := make([]string, 0, len(want))
	for _, perm := range AllPermissions() {
		if want[perm] {
			normalized = append(normalized, perm)
		}
	}

	// 到这里所有校验都过了。剩下的事在 policyWriter 的临界区里做完：
	// 重载（拿到一个与库完全一致的基线）→ 算差集 → 写库 → 再重载。
	//
	// **先重载再算差集**，而不是「来源不是 database 才重载」：差集必须相对
	// 这一次**真正替换掉的那一份**来算，否则审计里的 granted/revoked 是在
	// 一份可能已经过期的基线上算出来的。两次并发改动时，后写的那一次就会
	// 漏报前一次加的那一项——而「谁多了一项能力」正是出事故时要回答的第一个
	// 问题。顺带它也把「来源是 database 但存储其实是 nil」那种组合彻底消掉：
	// 原来那种组合会在 currentStore().ReplaceRole 上 panic（nil 接口调用）。
	policyWriter.Lock()
	defer policyWriter.Unlock()

	// 重载失败（表读不出来、库里那份违反受保护规则）就什么都不做：往一张
	// 读不出来、或装不回来的表里写权限，写进去的是不是生效的策略谁也说不准。
	if err := reloadLocked(); err != nil {
		return nil, err
	}

	// 这一行必须在 policyWriter 临界区里取：它就是本次改动**真正替换掉的那一份**
	// ——上面那次重载保证它与 role_permissions 表逐格一致。
	current := currentEnforcerSnapshot().permissionsOf(role)
	have := map[string]bool{}
	for _, perm := range current {
		have[perm] = true
	}
	// 两个字段都显式给非 nil 的空切片：本次「什么都没变」是完全正常的
	// 结果，而 nil 会被 JSON 编成 null，前端读它的 .length 就崩
	// （见 Change 的说明）。
	change := &Change{Role: role, Granted: []string{}, Revoked: []string{}}
	for _, perm := range current {
		if !want[perm] {
			change.Revoked = append(change.Revoked, perm)
		}
	}
	for _, perm := range normalized {
		if !have[perm] {
			change.Granted = append(change.Granted, perm)
		}
	}
	for _, perm := range change.Revoked {
		if err := ValidateRevoke(role, perm); err != nil {
			return nil, err
		}
	}

	// 到这里所有校验都过了，剩下的事只有一件：把集合整体写下去。
	// 刻意不做「逐项增删」——那是两次写、中间有一个可观测的半成品状态，
	// 而一次 ReplaceRole 在事务里要么全成要么全不成。
	s := currentStore()
	if s == nil {
		return nil, &PolicyError{
			Code:    ErrCodeUnavailable,
			Message: "策略存储未装配，无法写入权限",
		}
	}
	if err := s.ReplaceRole(role, normalized); err != nil {
		return nil, &PolicyError{
			Code:    ErrCodeUnavailable,
			Message: "写入 role_permissions 失败：" + err.Error(),
		}
	}
	if err := reloadLocked(); err != nil {
		rollbackRolePermissions(role, current)
		return nil, err
	}
	return change, nil
}

// rollbackRolePermissions 把某角色的权限集合写回旧值并重新装载。
//
// **调用方必须已经持有 policyWriter**。
//
// 回滚本身也可能失败（磁盘满了、表被锁了）。那时候只记日志：
// 内存里仍然是上一份可用策略（重载失败时不动内存），所以系统的行为还是
// 对的，错的只是库里那份数据——而它已经被 warnings 与这条错误日志指出来了。
// 再往上抛一个错误只会让调用方以为「什么都没发生」，反而更危险。
//
// ⚠️ 回滚失败**不构成「重启即中招」的后门**：它意味着库里留着的那份改动
// 违反受保护规则，而下次进程启动时 Bootstrap → reloadLocked 用的是同一个
// protectedRuleFindings，一样会拒绝装载它，于是启动路径照样 fail-closed。
// runtime_test.go 的 TestApplyRolePermissions_回滚失败后重启不会装上违规策略
// 钉住这一条。
func rollbackRolePermissions(role models.Role, previous []string) {
	log.Printf("[RBAC] 正在回滚 %s 的权限：本次写入导致重载失败", role.Label())
	s := currentStore()
	if s == nil {
		log.Printf("[RBAC] 回滚失败：策略存储未装配")
		return
	}
	if err := s.ReplaceRole(role, previous); err != nil {
		log.Printf("[RBAC] 回滚 %s 的权限失败，库里的数据与生效中的策略已不一致，"+
			"请按 README「权限策略锁死时的离线恢复」处理：%v", role.Label(), err)
		return
	}
	if err := reloadLocked(); err != nil {
		log.Printf("[RBAC] 回滚后重新装载仍然失败，生效中的仍是上一份策略：%v", err)
		return
	}
	log.Printf("[RBAC] 已回滚 %s 的权限", role.Label())
}
