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
//  1. 策略在 go:embed 的 policy.csv 里，**不进数据库**。现场误配一个勾就把
//     所有人锁在门外，而数据库策略没法在 code review 里审——它只有一条
//     UPDATE 记录，看不出「谁因此获得了什么」。策略文件是纯数据、可读、可评审，
//     而且没有任何数据迁移：policy.csv 本身就是迁移结果。将来真要运行时可编辑，
//     把下面 load() 里的 stringadapter 换成 gorm 的 fileadapter 即可，
//     路由声明那一层一行都不用动。
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

// defaultEnforcer 是进程内共享的那一个。
//
// 加载失败时保持 nil：Can 会因此对所有请求返回 false，也就是**全部拒绝**。
// 宁可让整套系统当场不可用（一眼能看出是策略坏了），也不能让它在没有策略
// 的情况下按「无人有权限」以外的任何方式放行。
var defaultEnforcer *casbin.Enforcer

func init() {
	e, err := load(modelConf, policyCSV)
	if err != nil {
		log.Printf("[RBAC] 策略加载失败，所有具名权限一律拒绝：%v", err)
		return
	}
	defaultEnforcer = e
	auditPolicy(e)
	auditProtectedRules(e)
}

// load 用给定的模型与策略文本构造一个 Enforcer。
//
// 单独抽出来是为了能被测试直接调用：迁移验证表要在**真实的 Casbin** 上逐格
// 跑，而不是靠一个自己实现的判断函数——后者只能证明「那个函数对」，证不了
// 「策略文件对」。
//
// 将来换成数据库策略，改的就是这个函数里 adapter 那一行。
func load(modelText, policyText string) (*casbin.Enforcer, error) {
	m, err := model.NewModelFromString(modelText)
	if err != nil {
		return nil, fmt.Errorf("解析 model.conf: %w", err)
	}
	e, err := casbin.NewEnforcer(m, stringadapter.NewAdapter(policyText))
	if err != nil {
		return nil, fmt.Errorf("加载 policy.csv: %w", err)
	}
	if err := e.LoadPolicy(); err != nil {
		return nil, fmt.Errorf("重读 policy.csv: %w", err)
	}
	return e, nil
}

// Default 返回进程内共享的 Enforcer。
//
// 暴露它是为了让测试能用 GetPermissionsForUser 直接读策略内容做自检
// （「超管是不是真的拿到了全部权限」这类断言必须问策略本身，不能问 Can）。
// 加载失败时返回 nil，调用方要能处理。
//
// ⚠️ **它带着写方法，这是接入在线编辑之前必须堵上的绕过点。**
//
// 拿它就能 `rbac.Default().AddPolicy("admin", rbac.PermSystemMaintain)`——
// protect.go 里的三条 Validate* 一道都不会跑，受保护规则当场失效。
// 之所以现在还能忍：策略是 go:embed 的只读文件，写入不是业务路径，
// 而 protect_test.go 会盯住「有人真的这么调了」（下面那条用例）。
//
// 接入在线编辑时**第一件事**就是把这个出口收掉：返回一个只读接口，
// 或者干脆把写方法封进本包，让裸的 AddPolicy/RemovePolicy 从包外不可达。
// 「Validate 是唯一入口」只有在这个出口消失之后才成立。
func Default() *casbin.Enforcer {
	return defaultEnforcer
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
	if defaultEnforcer == nil {
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

	allowed, err := defaultEnforcer.Enforce(string(role), perm)
	if err != nil {
		log.Printf("[RBAC] 拒绝：Enforce 出错（角色 %s，权限 %s）：%v", role, perm, err)
		return false
	}
	return allowed
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
	var out []models.Role
	for _, role := range models.AllRoles() {
		if Can(role, perm) {
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
	holders := Holders(perm)
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
