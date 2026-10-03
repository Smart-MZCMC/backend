// Package rbac 是路由守卫的准入来源：谁（角色）能做什么（具名权限）。
//
// 之前 routes/web.go 用的是「等级门槛」——RequireRole(models.RoleAdmin) 的
// 语义是 role.AtLeast(admin)。等级只能表达高低，表达不了「负责人能看、
// 不能改」：负责人 leader(40) 比导播 director(30) 还高，任何 min <= 40 的
// 门槛他都过得去，而业务上负责人只读 + 管采访点 + 授权成员，不该能删账号、
// 删项目。等级制在这条需求上无解，这也是它被换掉的原因。
//
// 现在路由声明「我需要哪个具名权限」，策略文件声明「哪个角色有哪些权限」。
// 两者解耦之后，**等级高低与权限大小不再是同一件事**——等级仍然保留，但它
// 只管「能不能操作某个人」（controllers/authz.go 的 decideRoleChange 等），
// 不再管「能不能进某个接口」。
//
// 三条设计约束。改动这个包之前先读，改动时不要绕过：
//
//  1. 策略存在 role_permissions 表里（在线可编辑），首次启动时用 policy.csv
//     播种一次。**表一旦有数据，它就是唯一事实来源**；policy.csv 退化为
//     播种模板与数据库读不出来时的兜底（见 store.go 与 runtime.go）。
//     策略文件仍然是可读、可评审的纯数据——它只是不再是运行时的权威。
//
//  2. 角色 → 权限是显式清单，不走 Casbin 的 g 继承链。g, admin, leader 这种
//     写法一配错就是越权且无提示，而且它天然表达不了「负责人权限比管理员小」。
//     漏补一行的后果是该角色少一项能力（安全侧，出错时立刻有人喊），
//     继承链配错的后果是某角色多了一项能力（危险侧）。
//
//  3. 项目成员校验不在这套里。user_projects 那套由 ProjectMemberMiddleware
//     单独负责，与「角色能做什么」是正交的两件事。这套只看角色，不看项目。
package rbac

import (
	_ "embed"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	stringadapter "github.com/casbin/casbin/v2/persist/string-adapter"

	"smart-mzcmc/app/models"
)

// 具名权限。命名规则 `<对象>.<动作>`，全小写。
//
// 声明顺序即 policy.csv 里分块的顺序，也是 AllPermissions() 的返回顺序，
// 也是错误信息与文档里出现的顺序——照这个顺序往下读是从「看」到「删系统」。
const (
	// PermLogView 看协调日志。
	PermLogView = "log.view"
	// PermProjectView 看项目列表/详情/机位/切台记录/统计。
	PermProjectView = "project.view"
	// PermUserView 看用户列表。与 PermUserManage 是两件事：
	// 负责人能看，不能增删改角色。
	PermUserView = "user.view"
	// PermAuditView 看操作审计。
	PermAuditView = "audit.view"
	// PermLogExport 导出日志。
	PermLogExport = "log.export"
	// PermInterviewManage 管理采访点。
	//
	// 目前没有任何路由挂它（后端尚无采访点增删改接口）。先声明是为了将来
	// 加接口时权限已经存在、且必须被显式挂上。
	PermInterviewManage = "interview.manage"
	// PermProjectMember 授权/回收项目成员。
	PermProjectMember = "project.member"
	// PermSwitchOperate 切台（acquire / release / heartbeat）。
	//
	// 唯一一组不连续的角色：导播(30)、管理员(50)、超管(60)，中间空着包装(25)
	// 与负责人(40)。这道门**不能**用等级表达，见 policy.csv 里的说明。
	PermSwitchOperate = "switch.operate"
	// PermLogCleanup 清理日志。
	PermLogCleanup = "log.cleanup"
	// PermProjectManage 建/改/删项目、机位预设增删改。
	PermProjectManage = "project.manage"
	// PermUserManage 增删账号、改角色。
	//
	// 「能进这个接口」不等于「能操作任何人」：控制器里的 decideRoleChange /
	// decideDeleteUser 仍然管「不能操作同级或更高、不能自降权、不能动最后一个
	// 超管」，两套判断是叠加的，不是一套换另一套。
	PermUserManage = "user.manage"
	// PermSystemMaintain 系统信息/运行指标/在线更新。
	PermSystemMaintain = "system.maintain"
)

// permissionNames 按声明顺序排列的全部权限名。
//
// 单独留一份有序列表而不是 map，是为了让 AllPermissions() 有确定顺序：
// 策略自检的报错信息、以及任何要打印权限清单的地方都靠它稳定可读。
var permissionNames = []string{
	PermLogView,
	PermProjectView,
	PermUserView,
	PermAuditView,
	PermLogExport,
	PermInterviewManage,
	PermProjectMember,
	PermSwitchOperate,
	PermLogCleanup,
	PermProjectManage,
	PermUserManage,
	PermSystemMaintain,
}

// permissionLabels 权限的中文名与说明。
//
// 为什么必须有：403 的文案不能是「权限不足」四个字——收到这句话的人既不知道
// 缺的是哪一项，也不知道该找谁，只能挨个试。于是每个权限都要有一句能直接
// 贴给现场看的话。
var permissionLabels = map[string]string{
	PermLogView:         "查看协调日志",
	PermProjectView:     "查看项目列表、详情、机位、切台记录与统计",
	PermUserView:        "查看用户列表",
	PermAuditView:       "查看操作审计",
	PermLogExport:       "导出协调日志",
	PermInterviewManage: "管理采访点",
	PermProjectMember:   "授权/回收项目成员",
	PermSwitchOperate:   "操作切台（获取/释放/续期控制权）",
	PermLogCleanup:      "清理协调日志",
	PermProjectManage:   "创建/修改/删除项目与机位预设",
	PermUserManage:      "增删账号、调整角色",
	PermSystemMaintain:  "查看系统信息、运行指标与执行在线更新",
}

//go:embed model.conf
var modelConf string

//go:embed policy.csv
var policyCSV string

// active 是内存里生效的那一份策略。
//
// 为什么要一个带锁的容器而不是一个裸 *casbin.Enforcer：在线编辑会**整体替换**
// 内存里的 Enforcer（Reload），而 Can 在每个受守卫的请求上都会调一次。
// 裸指针替换 + 并发读是数据竞争，Go 的 race detector 会报，线上则可能是
// 「一半请求按新策略、一半按旧策略」——权限系统最不能出现的状态。
//
// enforcer 为 nil 表示策略没加载起来：Can 因此对所有请求返回 false，也就是
// **全部拒绝**。宁可让整套系统当场不可用（一眼能看出是策略坏了），也不能让它
// 在没有策略的情况下按「无人有权限」以外的任何方式放行。
var active = struct {
	sync.RWMutex
	enforcer *casbin.Enforcer
	source   string
	warnings []string
}{source: SourceEmbedded}

// snapshot 是一次**原子**读出：策略、来源与告警必须来自同一代。
//
// 为什么需要它：一次 GET /api/rbac/policy 要读 12 项权限 × 8 个角色 =
// 96 次 Can，外加 8 次 PermissionsOf、一次 Source、一次 Warnings。如果每一次
// 各自取一次读锁，那么一次 Reload 落在中间就会拼出一份**半份快照**——
// source 是旧策略的、holders 是新策略的。
//
// 这不是数据竞争，race detector 看不见它，而它的后果恰恰是权限系统最怕的
// 那种：界面照着这份 JSON 渲染并让管理员据此提交，界面与实际放行从此对不上，
// 排查方向会被彻底带偏。
//
// 一次性取出来之后，Enforcer 本身此后不再被写（Reload 每次都新构造一个），
// 所以在锁外读它是安全的——这与 Can 拿到指针后放开读锁再 Enforce 是同一条
// 推理。
type snapshot struct {
	enforcer *casbin.Enforcer
	source   string
	warnings []string
}

// take 取一份原子快照。返回的 warnings 是副本，调用方可以随便改。
//
// 只给「一次响应要读多个字段」的调用点用（View、DeniedMessage）。单字段的
// 读走下面的 currentEnforcer：它不复制 warnings 切片，所以在每个受守卫的
// 请求都调一次的 Can 上不会凭空多出一次分配。
func take() snapshot {
	active.RLock()
	defer active.RUnlock()
	return snapshot{
		enforcer: active.enforcer,
		source:   active.source,
		warnings: append([]string(nil), active.warnings...),
	}
}

// currentEnforcer 取当前生效的 Enforcer 指针。
//
// 放开读锁之后再去 Enforce 是安全的：换下来的那个 Enforcer 此后不再被写
// （Reload 每次都新构造一个再整体换指针）。这条推理是 Can 不必持锁做整个
// 判定的原因，也是并发读写没有数据竞争的原因。
func currentEnforcer() *casbin.Enforcer {
	active.RLock()
	defer active.RUnlock()
	return active.enforcer
}

// currentEnforcerSnapshot 是一条只有 Enforcer 的「快照」。
//
// 存在的意义是让 Can / PermissionsOf / Holders 与 View 共用同一份判定逻辑
// （snapshot.enforce / snapshot.permissionsOf），而不是各写一遍——各写一遍
// 的话，「快照必须自洽」这个不变量就只在 View 成立，别处会在下一次重构里
// 悄悄退回逐字段取锁的写法。
func currentEnforcerSnapshot() snapshot {
	return snapshot{enforcer: currentEnforcer()}
}

// enforce 按这份快照回答「能不能做」。与 Can 同一个判定、同一批 fail-closed
// 规则，但不重新读全局——这是「快照必须自洽」的前提。
func (s snapshot) enforce(role models.Role, perm string) bool {
	if s.enforcer == nil {
		log.Printf("[RBAC] 拒绝：Enforcer 未初始化（策略加载失败），角色 %s 请求权限 %s", role, perm)
		return false
	}
	if !role.Valid() {
		log.Printf("[RBAC] 拒绝：非法角色 %q 请求权限 %s", role, perm)
		return false
	}
	if !Known(perm) {
		log.Printf("[RBAC] 拒绝：未知权限名 %q（角色 %s）——"+
			"路由上打错了权限名，检查 middleware.RequirePermission 的参数", perm, role)
		return false
	}
	allowed, err := s.enforcer.Enforce(string(role), perm)
	if err != nil {
		log.Printf("[RBAC] 拒绝：Enforce 出错（角色 %s，权限 %s）：%v", role, perm, err)
		return false
	}
	return allowed
}

// permissionsOf 按这份快照回答「策略给这个角色记了哪些权限」。与
// PermissionsOf 同一套语义（包括把脏行吵出来）。
func (s snapshot) permissionsOf(role models.Role) []string {
	if s.enforcer == nil || !role.Valid() {
		return nil
	}
	rules, err := s.enforcer.GetPermissionsForUser(string(role))
	if err != nil {
		log.Printf("[RBAC] 读取 %s 的权限失败：%v", role, err)
		return nil
	}
	granted := make(map[string]bool, len(rules))
	for _, rule := range rules {
		if len(rule) != 2 {
			// 字段数不对的行不可能来自本包的写入路径（它们都是拼出来的
			// `p, role, perm`）。留着不吵只会让界面上多出一格说不清的权限。
			log.Printf("[RBAC] 策略行字段数不是 2，已忽略：%v", rule)
			continue
		}
		granted[rule[1]] = true
	}
	out := make([]string, 0, len(granted))
	for _, perm := range AllPermissions() {
		if granted[perm] {
			out = append(out, perm)
		}
	}
	return out
}

func init() {
	e, err := load(modelConf, policyCSV)
	if err != nil {
		log.Printf("[RBAC] 策略加载失败，所有具名权限一律拒绝：%v", err)
		// 刻意把这件事也放进 warnings，而不只是打一行日志。Enforcer 为 nil
		// 时 Can 对一切返回 false，于是整个系统 403——而 GET /api/rbac/policy
		// 此刻会说「策略来自 embedded」且**一条告警都没有**。管理员看到的是
		// 一份空矩阵，找不到任何解释，只会去怀疑自己没配好权限。
		// fail-closed 是对的，但必须让人**看得见**它是 fail-closed。
		active.Lock()
		active.warnings = []string{
			"内嵌策略 policy.csv 加载失败，所有具名权限一律拒绝（fail-closed）：" + err.Error(),
		}
		active.Unlock()
		return
	}
	active.Lock()
	active.enforcer = e
	active.source = SourceEmbedded
	active.warnings = embeddedWarnings(e)
	active.Unlock()
	auditPolicy(e)
	auditProtectedRules(e)
}

// load 用给定的模型与策略文本构造一个 Enforcer。
//
// 单独抽出来是为了能被测试直接调用：迁移验证表要在**真实的 Casbin** 上逐格
// 跑，而不是靠一个自己实现的判断函数——后者只能证明「那个函数对」，证不了
// 「策略文件对」。
//
// 注意策略文本在这里仍然只是一段 CSV：数据库矩阵先被翻译成同样形状的文本
// （runtime.go 的 matrixToPolicyText），再喂给 Casbin。之所以不接 gorm 的
// adapter，是因为 Casbin 的 matcher 与行格式都没变，多引一个持久化适配器
// 只会多一处「adapter 行为与字符串不完全等价」的偏差来源——那正是策略这种
// 不能出错的东西最不该承担的风险。
func load(modelText, policyText string) (*casbin.Enforcer, error) {
	m, err := model.NewModelFromString(modelText)
	if err != nil {
		return nil, fmt.Errorf("解析 model.conf: %w", err)
	}
	e, err := casbin.NewEnforcer(m, stringadapter.NewAdapter(policyText))
	if err != nil {
		return nil, fmt.Errorf("加载策略文本: %w", err)
	}
	if err := e.LoadPolicy(); err != nil {
		return nil, fmt.Errorf("重读策略文本: %w", err)
	}
	return e, nil
}

// Can 报告角色是否持有某项权限。**任何异常都返回 false**。
//
// fail-closed 是这里唯一的原则，一个一个说清为什么不能反过来：
//   - Enforcer 为 nil（策略没加载起来）→ 拒。否则等于「策略坏了就全放行」。
//   - Enforce 返回 error → 拒。matcher 写错、策略写坏都走这条路。
//   - 角色非法 → 拒。users.role 是 varchar(20) 且没有 CHECK 约束，脏数据
//     真的会出现。这里先拦一道，好让中间件能报出「角色异常」而不是
//     「权限不足」——后者会让人一直去改权限，而真正的原因是数据坏了。
//   - 权限名未知 → 拒，并打日志。这通常是路由上打错了一个字，权限没有
//     静悄悄地失效，而是要吵。
func Can(role models.Role, perm string) bool {
	return currentEnforcerSnapshot().enforce(role, perm)
}

// PermissionsOf 返回策略**本身**记给这个角色的权限清单（按声明顺序）。
//
// 刻意不走 Can 逐项问一遍：Can 只回答「能不能做」，而这里要回答的是
// 「策略里到底有没有这一行」。两者不一样——在线编辑界面要展示与提交的是
// 后者，而超管多出来的那些脏行只能靠它才看得见。
func PermissionsOf(role models.Role) []string {
	return currentEnforcerSnapshot().permissionsOf(role)
}

// Known 报告 perm 是已声明的权限名。
func Known(perm string) bool {
	_, ok := permissionLabels[perm]
	return ok
}

// AllPermissions 返回全部已声明的权限名，按声明顺序。
func AllPermissions() []string {
	out := make([]string, len(permissionNames))
	copy(out, permissionNames)
	return out
}

// Label 返回权限的中文名；未知权限原样返回，便于排查路由上的错字。
func Label(perm string) string {
	if label, ok := permissionLabels[perm]; ok {
		return label
	}
	return perm
}

// Holders 返回持有该权限的角色，按权限从高到低（models.AllRoles() 的顺序）。
//
// 从**策略本身**读出来，而不是从一张写死的表里抄：这样错误信息里的
// 「谁能做」不可能与实际放行的集合对不上。
func Holders(perm string) []models.Role {
	return currentEnforcerSnapshot().holders(perm)
}

// holders 是 Holders 的快照版本：同一次回答里的每一项都来自同一代策略。
func (s snapshot) holders(perm string) []models.Role {
	var out []models.Role
	for _, role := range models.AllRoles() {
		if s.enforce(role, perm) {
			out = append(out, role)
		}
	}
	return out
}

// DeniedMessage 拼出 403 的文案：缺哪个权限、这权限是干什么的、谁能做、
// 你现在是什么角色。
//
// 「权限不足」四个字是排查不了任何事的：不知道缺哪一项、也不知道该找谁。
// 这里把四个信息一次给全——现场截图过来就能定位。
//
// 例子：
//
//	权限不足：缺少权限 project.member（授权/回收项目成员；
//	仅 负责人、管理员、超级管理员 可执行），当前角色为 导播
func DeniedMessage(perm string, role models.Role) string {
	// 一次性取快照：holders 里的每一项必须来自同一代策略，否则会拼出
	// 「A 说只有超管能做、B 说超管和负责人都能做」这种自相矛盾的 403 文案。
	holders := currentEnforcerSnapshot().holders(perm)
	var who string
	switch len(holders) {
	case 0:
		// 走到这里说明权限名拼错了（Known 已经挡过一次，但 holders 是从
		// 策略读的，理论上还有别的路径能到这里）。说「没有人可执行」比
		// 列出空名单诚实。
		who = "当前没有任何角色可执行，请联系超级管理员核查策略"
	case 1:
		who = "仅 " + holders[0].Label() + " 可执行"
	default:
		labels := make([]string, 0, len(holders))
		for _, r := range holders {
			labels = append(labels, r.Label())
		}
		who = "仅 " + strings.Join(labels, "、") + " 可执行"
	}

	label := Label(perm)
	desc := ""
	if label != perm {
		desc = "（" + label + "；" + who + "）"
	} else {
		desc = "（" + who + "）"
	}
	return "权限不足：缺少权限 " + perm + desc + "，当前角色为 " + role.Label()
}

// auditPolicy 在启动时把策略文件里的可疑行吵出来。
//
// 刻意**不**因此让启动失败：策略多写了一行无害的角色或权限，行为上只是那行
// 谁都匹配不上——真正危险的是「本该写却漏了」，那是少能力，测试会报。
// 但「策略里出现了系统不认识的角色/权限」几乎总是打错了字，必须吵。
//
// ⚠️ 不要把这一层当成守卫。它是给人看的线索：policy.csv 被人手改过、
//
//	或者镜像里的文件与仓库不一致时，它是唯一的提示。真正拦住坏改动的是
//	两处——app/rbac 的测试（policy.csv 要过 code review，CI 会挡），
//	与 protect.go 的 Validate*（那是留给将来在线编辑写入路径的唯一入口，
//	review 挡不住运行时）。启动自检刻意只吵不改：让内存里的状态与唯一的
//	事实来源不一致，等于凭空多出第二个事实来源。
func auditPolicy(e *casbin.Enforcer) {
	if e == nil {
		return
	}
	rules, err := e.GetPolicy()
	if err != nil {
		log.Printf("[RBAC] 读取策略失败，无法自检：%v", err)
		return
	}
	for _, rule := range rules {
		if len(rule) != 2 {
			log.Printf("[RBAC] 策略行字段数不是 2，已跳过：%v", rule)
			continue
		}
		subject, object := rule[0], rule[1]
		if !models.Role(subject).Valid() {
			log.Printf("[RBAC] 策略里出现了未知角色 %q（权限 %s）——"+
				"这一行对任何人都不会生效，请核对 app/models/role.go 的常量", subject, object)
		}
		if !Known(object) {
			log.Printf("[RBAC] 策略里出现了未知权限 %q（角色 %s）——"+
				"没有路由会要求它，检查是不是打错了字", object, subject)
		}
	}
	log.Printf("[RBAC] 策略已加载：%d 项权限 / %d 条授权规则",
		len(permissionNames), len(rules))
}
