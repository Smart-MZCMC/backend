// 本文件是策略的**不变量**层：哪些权限/角色不允许被改动，以及改动之前该怎么问一句。
//
// ############################################################################
// # 这是唯一需要被调用来判断「能不能改策略」的地方。
// #
// # 每一次写入都必须先问这里，包括：
// #   - 勾选一项权限   → ValidateGrant(角色, 权限)
// #   - 取消一项权限   → ValidateRevoke(角色, 权限)
// #   - 移除一个角色   → ValidateRemoveRole(角色)
// #   - 重命名一个角色 → 等价于「移除旧的 + 新增新的」，RemoveRole 那条会挡住
// #
// # 「必须问这里」这件事现在靠的是**结构**而不只是约定：app/rbac 不导出
// # *casbin.Enforcer，包外拿不到任何能 AddPolicy 的东西（见
// # rbac/runtime.go 的说明与 bypass_test.go 那条用例）。唯一两个改动内存里
// # 策略的出口是 Reload 与 ApplyRolePermissions，两个都先过下面的校验。
// ############################################################################
//
// 为什么要把「不变量」与「策略内容」分成两个文件：策略内容会变（policy.csv
// 在 code review 里，role_permissions 在线上可编辑），而这几条**永远不变**——
// 它们存在的理由是「除了 SSH 手改数据库，没有任何办法恢复」。所以它们既不在
// 策略文件里，也不在数据库里，只在代码里。
//
// 三层防线，现在分别落在哪：
//
//	1. 纯函数（本文件）  —— 挡住未来的写入路径。
//	2. 测试（protect_test.go + rbac_test.go 的迁移矩阵）—— 挡住 policy.csv 被改坏。
//	   policy.csv 是要过 code review 的纯数据，所以这一层足够，且不需要运行时行为。
//	3. 装载时核对（protectedRuleFindings）—— 从 role_permissions 装载策略时，
//	   违规就**拒绝加载**并保留上一份可用状态（fail-closed）。这里必须 fail-closed
//	   而不只是吵：违规策略一旦生效，所有人就都失去系统维护权限，而且界面改不回来。
//
// 刻意不在装载时「自动修复」：policy.csv 是唯一的事实来源，让内存里的状态与它
// 不一致，会凭空多出第二个事实来源，而 CI 已经拦住了 90% 的情况。

package rbac

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/casbin/casbin/v2"

	"smart-mzcmc/app/models"
)

// protectedPermissions 受保护的权限：不可撤销，且只能由受保护角色持有。
//
// **目前只有 system.maintain。** 加新条目前先回答两个问题：
//   - 失去它会不会让某类人彻底没法工作（且现场无法自助恢复）？
//   - 它是否应该只属于最高角色？
//
// 两个都是「是」才加进来。日志类的、导出类的、可配的都不算——
// 把受保护清单拉长只会让在线编辑界面变成一片灰色。
//
// ⚠️ audit.view **不在**这里。已确认：操作审计保持「管理员及以上」，
//
//	不开放给负责人。理由写在 policy.csv 里。
var protectedPermissions = map[string]string{
	PermSystemMaintain: "系统在线更新会替换服务自身的可执行文件并重启进程，" +
		"任何一次误操作都会影响全系统所有客户端；而且一旦没人持有它，" +
		"现场没有任何界面能把它恢复回来",
}

// protectedRoles 受保护的角色：不可从角色清单里移除。
//
// **目前只有 super_admin。** 它是下面那条权限守卫的有效性前提：
// 一旦这个角色被整体移除，system.maintain 虽然仍然「只允许超级管理员持有」，
// 却没有任何人持有它——所有管理员失去系统维护权限，且无法自助恢复。
// 换句话说，第 2 条守卫（角色受保护）是第 1 条守卫（权限受保护）不空的前提，
// 少掉它整个不变量层就是空的。
var protectedRoles = map[models.Role]string{
	models.RoleSuperAdmin: "它是「系统维护权限只属于最高角色」这条规则的唯一受益者。" +
		"删掉它，那条规则仍然成立但没人满足，于是所有管理员失去系统维护权限，" +
		"而且没有任何界面能改回来",
}

// IsProtected 报告这项权限是否受保护（不可撤销、且只能由受保护角色持有）。
//
// 给界面用的就是它：受保护的权限那一格应当渲染成不可点，而不是点了才报错。
func IsProtected(perm string) bool {
	_, ok := protectedPermissions[perm]
	return ok
}

// IsProtectedRole 报告这个角色是否受保护（不可从角色清单里移除）。
func IsProtectedRole(role models.Role) bool {
	_, ok := protectedRoles[role]
	return ok
}

// ProtectedPermissions 返回全部受保护权限，按声明顺序。
func ProtectedPermissions() []string {
	out := make([]string, 0, len(protectedPermissions))
	// 声明顺序就是 AllPermissions() 的顺序，按它排比按 map 的随机序好读。
	for _, p := range AllPermissions() {
		if IsProtected(p) {
			out = append(out, p)
		}
	}
	return out
}

// ProtectedRoles 返回全部受保护角色，按权限从高到低。
func ProtectedRoles() []models.Role {
	var out []models.Role
	for _, r := range models.AllRoles() {
		if IsProtectedRole(r) {
			out = append(out, r)
		}
	}
	return out
}

// protectedReason 返回「为什么它受保护」，写进错误信息。
//
// 不写清楚的话，界面上的提示就是一句「不允许」，点的人只会以为是系统坏了。
func protectedReason(perm string) string {
	if reason, ok := protectedPermissions[perm]; ok {
		return reason
	}
	return ""
}

func protectedRoleReason(role models.Role) string {
	if reason, ok := protectedRoles[role]; ok {
		return reason
	}
	return ""
}

// PolicyError 是策略校验失败，带一个可判别的类别。
//
// 分成三类是因为在线编辑界面要用：受保护的那一类应当**在渲染时就禁用**
// （用户根本点不到），而不是点下去之后弹一句「不允许」。剩下两类
// （角色/权限不存在）则是「你选的东西已经过期了，刷新一下」。
type PolicyError struct {
	// Code 失败类别。
	Code PolicyErrorCode
	// Message 直接给用户看的中文说明。
	Message string
}

type PolicyErrorCode string

const (
	// ErrCodeProtected 触碰了受保护的权限或角色。
	ErrCodeProtected PolicyErrorCode = "protected"
	// ErrCodeUnknownRole 角色不在合法集合内。
	ErrCodeUnknownRole PolicyErrorCode = "unknown_role"
	// ErrCodeUnknownPermission 权限不在已声明的集合内。
	ErrCodeUnknownPermission PolicyErrorCode = "unknown_permission"
	// ErrCodeConflict 策略写下去了但重载不通过，本次改动已回滚。
	//
	// 与前三类的区别：前三类是「你这样改不被允许」，这一类是「你这样改本来
	// 是被允许的，但落库之后整套策略对不上受保护规则，所以没生效」。
	// 界面必须把它当成一次**没有发生**的改动，并且提示刷新当前策略。
	ErrCodeConflict PolicyErrorCode = "policy_conflict"
	// ErrCodeUnavailable 策略表读不出来，连当前策略都无法确定。
	ErrCodeUnavailable PolicyErrorCode = "policy_unavailable"
)

func (e *PolicyError) Error() string { return e.Message }

func protectedError(msg string) error {
	return &PolicyError{Code: ErrCodeProtected, Message: msg}
}

// IsProtectedError 报告 err 是不是「触碰了受保护的东西」。
//
// 界面据此决定「禁用这一格」还是「提示已过期，请刷新」。
func IsProtectedError(err error) bool {
	var pe *PolicyError
	return errors.As(err, &pe) && pe.Code == ErrCodeProtected
}

// CodeOf 返回 err 的可判别类别；不是 PolicyError 时返回空串。
//
// 端点靠它决定响应体里的 code 字段，前端靠 code 区分「受保护不可改」
// 与「你选的东西过期了，刷新」。没有这个函数的话端点只能自己 errors.As 一遍，
// 而两处各写一遍迟早有一处漏改分类。
func CodeOf(err error) PolicyErrorCode {
	var pe *PolicyError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// ValidateGrant 校验「把这项权限授予这个角色」是否合法。返回 nil 表示允许。
//
// 纯函数，不碰 Enforcer、不碰数据库——它只回答「这条变更该不该被接受」。
// 这样写入路径（无论是 API handler 还是 service）只需要一个「先问这里，
// 再写下去」的形状，而这条规则本身可以脱离运行时单测。
//
// 规则只有一条实质约束：受保护的权限只能授予受保护的角色。
// 除此之外的判断（角色/权限存不存在）是 fail-closed 的兜底——
// 写入路径如果拿到一个不认识的角色，那是 bug，不能当成「那就不受限制了」。
func ValidateGrant(role models.Role, perm string) error {
	if !role.Valid() {
		return &PolicyError{
			Code:    ErrCodeUnknownRole,
			Message: "角色不存在：" + string(role) + "（可用角色见 /api/roles）",
		}
	}
	if !Known(perm) {
		return &PolicyError{
			Code:    ErrCodeUnknownPermission,
			Message: "权限不存在：" + perm,
		}
	}
	if !IsProtected(perm) {
		return nil
	}
	if IsProtectedRole(role) {
		return nil
	}
	return protectedError("权限 " + perm + " 受保护，只允许授予受保护角色（" +
		holderLabel() + "），不能授予" + role.Label() +
		"。理由：" + protectedReason(perm))
}

// ValidateRevoke 校验「取消这个角色的这项权限」是否合法。返回 nil 表示允许。
//
// 受保护权限一律拒，**包括从超级管理员自己身上取消**。这是最容易漏掉的一格：
// 只挡住「别人能不能拿」而允许「持有者自己放手」，锁死还是会发生。
func ValidateRevoke(role models.Role, perm string) error {
	if !role.Valid() {
		return &PolicyError{
			Code:    ErrCodeUnknownRole,
			Message: "角色不存在：" + string(role),
		}
	}
	if !Known(perm) {
		return &PolicyError{
			Code:    ErrCodeUnknownPermission,
			Message: "权限不存在：" + perm,
		}
	}
	if !IsProtected(perm) {
		return nil
	}
	return protectedError("权限 " + perm + " 受保护，不可撤销（哪怕是从 " +
		role.Label() + " 自己身上取消）。理由：" + protectedReason(perm))
}

// ValidateRemoveRole 校验「把这个角色从角色清单里移除」是否合法。
//
// 重命名一个角色等价于「移除旧的 + 新增新的」，所以这条也顺带挡住了重命名。
func ValidateRemoveRole(role models.Role) error {
	if !role.Valid() {
		return &PolicyError{
			Code:    ErrCodeUnknownRole,
			Message: "角色不存在：" + string(role),
		}
	}
	if !IsProtectedRole(role) {
		return nil
	}
	return protectedError("角色 " + role.Label() + " 受保护，不可移除。" +
		"理由：" + protectedRoleReason(role))
}

// holderLabel 列出受保护角色的中文名，用于错误信息。
func holderLabel() string {
	roles := ProtectedRoles()
	labels := make([]string, 0, len(roles))
	for _, r := range roles {
		labels = append(labels, r.Label())
	}
	return strings.Join(labels, "、")
}

// auditProtectedRules 在启动时核对受保护的不变量，只吵不改。
//
// 它只是 protectedRuleFindings 的日志外壳：真正要拿结论（拒绝加载）的是
// runtime.go 里的 Reload。启动这一层刻意保持「只吵不改」——policy.csv 是过
// code review 的纯数据，绝大多数问题由 CI 拦下；真到了线上不一致的时候，
// 把整套系统停掉比让超管多一项系统维护权限更糟。这里把话吵出来，让人去处理。
func auditProtectedRules(e *casbin.Enforcer) {
	if e == nil {
		return
	}
	violations, notes := protectedRuleFindings(e)
	for _, v := range violations {
		log.Printf("[RBAC] 策略违反受保护规则：%s", v)
	}
	for _, n := range notes {
		log.Printf("[RBAC] %s", n)
	}
}

// protectedRuleFindings 核对受保护的不变量，把结论分成「违规」与「须知」。
//
// 分成两类的理由是它们的**后果**不同：
//   - violations：这份策略一旦生效，系统就锁死了（没人能做系统维护，且没有
//     界面能改回来）。从 role_permissions 装载时必须 fail-closed——拒绝加载、
//     保留上一份可用状态。只打日志等于违规策略已经在生效了。
//   - notes：不致命但必须让人知道的话（受保护的权限与角色各是什么、为什么）。
//     它们会一路带到 GET /api/rbac/policy 的 warnings 里由界面原样显示——
//     界面需要这些理由才能把受保护的那几格画成不可点。
//
// 核对两件事：
//  1. 每一条已有授权都过一遍 ValidateGrant——策略里出现「受保护权限授予
//     给了别的角色」，说明有人手改过文件，或直接改过数据库。
//  2. 每个受保护权限都必须有人持有，且必须由受保护角色持有——没人持有的那一刻
//     系统就锁死了，而这正是下面那条守卫要防的结果。
func protectedRuleFindings(e *casbin.Enforcer) (violations, notes []string) {
	if e == nil {
		return nil, []string{"策略未加载，无法核对受保护规则"}
	}
	rules, err := e.GetPolicy()
	if err != nil {
		return []string{fmt.Sprintf("读取策略失败，无法核对受保护规则：%v", err)}, nil
	}
	granted := make(map[string]map[models.Role]bool, len(protectedPermissions))
	for perm := range protectedPermissions {
		granted[perm] = map[models.Role]bool{}
	}
	for _, rule := range rules {
		if len(rule) != 2 {
			violations = append(violations, fmt.Sprintf("策略行字段数不是 2：%v", rule))
			continue
		}
		role, perm := models.Role(rule[0]), rule[1]
		if err := ValidateGrant(role, perm); err != nil {
			violations = append(violations, err.Error())
		}
		if holders, ok := granted[perm]; ok {
			holders[role] = true
		}
	}
	for _, perm := range ProtectedPermissions() {
		holders := granted[perm]
		if len(holders) == 0 {
			violations = append(violations, fmt.Sprintf(
				"受保护权限 %s 没有任何角色持有——系统已经锁死，没有界面能恢复它", perm))
			continue
		}
		for _, role := range ProtectedRoles() {
			if !holders[role] {
				violations = append(violations, fmt.Sprintf(
					"受保护权限 %s 没有授予受保护角色 %s——该权限目前只落在 %s 手里，"+
						"一旦出事将无法恢复",
					perm, role.Label(), strings.Join(roleLabelList(holders), "、")))
			}
		}
	}
	for _, perm := range ProtectedPermissions() {
		notes = append(notes, fmt.Sprintf("受保护权限 %s（%s）：%s",
			perm, Label(perm), protectedReason(perm)))
	}
	for _, role := range ProtectedRoles() {
		notes = append(notes, fmt.Sprintf("受保护角色 %s：%s", role.Label(), protectedRoleReason(role)))
	}
	return violations, notes
}

func roleLabelList(roles map[models.Role]bool) []string {
	out := make([]models.Role, 0, len(roles))
	for r := range roles {
		out = append(out, r)
	}
	// map 的遍历顺序是随机的，日志里角色顺序每次都不一样就没法对照。
	sort.Slice(out, func(i, j int) bool { return out[i].Level() > out[j].Level() })
	labels := make([]string, 0, len(out))
	for _, r := range out {
		labels = append(labels, r.Label())
	}
	return labels
}
