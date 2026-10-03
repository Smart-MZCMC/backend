package rbac

import (
	"errors"
	"log"
	"sort"
	"strings"

	"github.com/casbin/casbin/v2"

	"smart-mzcmc/app/models"
)

// 本文件是策略的**不变量**层：哪些权限/角色不允许被改动，以及改动之前该怎么问一句。
//
// ############################################################################
// # 这是唯一需要被调用来判断「能不能改策略」的地方。
// #
// # 将来接入在线权限编辑（把 load() 里的 stringadapter 换成 gorm 的 fileadapter）
// # 之后，**每一次写入都必须先问这里**，包括：
// #   - 勾选一项权限   → ValidateGrant(角色, 权限)
// #   - 取消一项权限   → ValidateRevoke(角色, 权限)
// #   - 移除一个角色   → ValidateRemoveRole(角色)
// #   - 重命名一个角色 → 等价于「移除旧的 + 新增新的」，RemoveRole 那条会挡住
// #
// # 绕过这里直接调 e.AddPolicy / e.RemovePolicy 的代码路径，等于打开了下面
// # 两条守卫上的锁：系统维护权限会消失，而且没有界面能改回来。
// #
// # 已知的一个现成绕过点：**Default() 返回的就是 *casbin.Enforcer**，它带着
// # AddPolicy，谁都能写。也就是说「必须问这里」这件事今天靠约定、而不靠结构。
// # 接入在线编辑时第一件事是把这个出口收掉——返回一个只读接口，或把写方法
// # 封进本包让裸 AddPolicy 从包外不可达（见 rbac.go 里 Default 的注释）。
// ############################################################################
//
// 为什么现在就要有这套东西，尽管策略还是只读的 go:embed 文件：
//
// 只读意味着「今天改不动」，不意味着「规则可以等」。在线编辑一开，第一个勾掉
// 超级管理员 system.maintain 的人会在当场把系统锁死：所有管理员失去系统维护
// 权限（只有超管能做在线更新与运行指标），而**没有任何界面能改回来**，只能 SSH
// 手改数据库。守卫必须在写入能力被打开**之前**存在——写进去的时候人已经不在
// 现场了，那一行代码将是唯一的防线。
//
// 三层防线，现在分别落在哪：
//
//	1. 纯函数（本文件）  —— 挡住未来的写入路径。
//	2. 测试（protect_test.go + rbac_test.go 的迁移矩阵）—— 挡住 policy.csv 被改坏。
//	   policy.csv 是要过 code review 的纯数据，所以这一层足够，且不需要运行时行为。
//	3. 启动自检（auditProtectedRules）—— 只吵不改：告诉正在跑的实例「策略不对劲」，
//	   但不改内存里的策略，也不拦启动。策略文件被人手改过、或者镜像里的文件与
//	   仓库不一致时，这一行日志是唯一的线索。
//
// 刻意不在启动时「自动修复」：策略文件是唯一的事实来源，让内存里的状态与它
// 不一致，会凭空多出第二个事实来源，而 CI 已经拦住了 90% 的情况。

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

// ValidateGrant 校验「把这项权限授予这个角色」是否合法。返回 nil 表示允许。
//
// 纯函数，不碰 Enforcer、不碰数据库——它只回答「这条变更该不该被接受」。
// 这样将来在线编辑那一层（无论是 API handler 还是 service）只需要一个
// 「先问这里，再写下去」的形状，而这条规则本身可以脱离运行时单测。
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
// 核对两件事：
//  1. 每一条已有授权都过一遍 ValidateGrant——策略文件里出现「受保护权限授予
//     了别的角色」，说明有人手改过文件，或镜像里的文件与仓库不一致。
//  2. 每个受保护权限都必须有人持有，且必须由受保护角色持有——没人持有的那一刻
//     系统就锁死了，而这正是下面那条守卫要防的结果。
//
// 刻意不 fail-closed 到「拒绝启动」：策略文件是过 code review 的纯数据，
// 绝大多数改动由 CI 拦下；真到了线上不一致的时候，把整套系统停掉比让超管
// 多一项系统维护权限更糟。这里把话吵出来，让人去处理。
func auditProtectedRules(e *casbin.Enforcer) {
	if e == nil {
		return
	}
	rules, err := e.GetPolicy()
	if err != nil {
		log.Printf("[RBAC] 读取策略失败，无法核对受保护规则：%v", err)
		return
	}
	granted := make(map[string]map[models.Role]bool, len(protectedPermissions))
	for perm := range protectedPermissions {
		granted[perm] = map[models.Role]bool{}
	}
	for _, rule := range rules {
		if len(rule) != 2 {
			continue
		}
		role, perm := models.Role(rule[0]), rule[1]
		if err := ValidateGrant(role, perm); err != nil {
			log.Printf("[RBAC] 策略违反受保护规则：%v", err)
		}
		if holders, ok := granted[perm]; ok {
			holders[role] = true
		}
	}
	for _, perm := range ProtectedPermissions() {
		holders := granted[perm]
		if len(holders) == 0 {
			log.Printf("[RBAC] 受保护权限 %s 没有任何角色持有——"+
				"系统已经锁死，没有界面能恢复它，请立即核对 policy.csv", perm)
			continue
		}
		for _, role := range ProtectedRoles() {
			if !holders[role] {
				log.Printf("[RBAC] 受保护权限 %s 没有授予受保护角色 %s——"+
					"该权限目前只落在 %s 手里，一旦出事将无法恢复",
					perm, role.Label(), strings.Join(roleLabelList(holders), "、"))
			}
		}
	}
	for _, role := range ProtectedRoles() {
		log.Printf("[RBAC] 受保护角色 %s：%s", role.Label(), protectedRoleReason(role))
	}
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
