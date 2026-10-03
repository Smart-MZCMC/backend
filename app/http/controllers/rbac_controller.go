package controllers

import (
	"log"

	"github.com/goravel/framework/contracts/http"

	"smart-mzcmc/app/audit"
	"smart-mzcmc/app/models"
	"smart-mzcmc/app/rbac"
)

// RbacController 提供在线权限编辑接口。
//
// 守卫（system.maintain）挂在路由上，见 routes.RegisterRbacRoutes。
// 本文件只做三件事：把请求翻译成 rbac 包的调用、把 rbac 包的错误翻译成
// 响应、写审计。**所有策略判定都在 app/rbac 里**——控制器一旦自己写一遍
// 「这一项能不能改」的判断，那份判断就与 protect.go 的那一份迟早分叉，
// 而分叉之后生效的是控制器里那份没人 review 的。
type RbacController struct{}

func NewRbacController() *RbacController {
	return &RbacController{}
}

// ShowPolicy 返回当前生效的权限矩阵与保护状态。
//
// 响应结构（字段名即前端契约，改动前先想清楚谁在读）：
//
//	{
//	  "source": "database",              // 或 "embedded"
//	  "warnings": ["受保护权限 system.maintain（…）：理由…"],
//	  "permissions": [
//	    {"name":"log.view","label":"查看协调日志",
//	     "protected":false,"holders":["super_admin","admin",…]}
//	  ],
//	  "roles": [
//	    {"value":"super_admin","label":"超级管理员","level":60,
//	     "protected":true,"grants":["log.view", …]}
//	  ]
//	}
//
// holders 与 grants 是**从生效中的策略现算**的，不是从 role_permissions 表
// 读出来再推的——两者不一致时，界面会让人以为某项权限没生效（于是反复
// 勾选），而真相是它生效了、只是被脏行挡住。那是完全相反的排查方向。
//
// source 让界面能说出「你改的这条到底存不存在」：embedded 状态下改动会写进
// 数据库，但**要等下次重载才生效**，而当前生效的仍然是文件里的那一版。
// 不显示它的话，管理员会在「保存成功」之后继续按旧策略排查问题。
func (c *RbacController) ShowPolicy(ctx http.Context) http.Response {
	view := rbac.View()
	return ctx.Response().Json(200, view)
}

// UpdateRolePermissions 把某个角色的权限集合整体替换成请求里给的那一组。
//
// 请求：PUT /api/rbac/roles/:role/permissions
// body：{"permissions":["log.view","project.view"]}
//
// 语义是「改成这样」：没出现在列表里的权限一律变成不授予。界面渲染的是
// 一整张勾选表，提交的就是全量。
//
// 响应（成功 200）：
//
//	{
//	  "role":"admin",
//	  "granted":["project.view"],   // 本次新授予的
//	  "revoked":["log.export"],     // 本次被取消的
//	  "source":"database",
//	  "warnings":[...]
//	}
//
// 失败时返回 {"error": "<中文说明>", "code": "<类别>"}，类别见
// rbac.PolicyErrorCode：
//
//	"protected"        受保护不可改（403）——界面把那一格画成不可点，不要让用户点
//	"unknown_role"     角色不存在（400）——通常是刷新之后角色没了
//	"unknown_permission" 权限不存在（400）——同上，前端用的还是旧清单
//	"policy_conflict"  写下去了但重载不通过，本次已回滚（409）——提示刷新
//	"policy_unavailable" 策略表读不出来（503）——稍后重试
//
// 前端靠 code 区分「受保护不可改」与「你选的东西过期了，刷新」这两类：
// 它们的处置完全不同，合成一句话的话，用户会刷新一百次也刷新不出来。
func (c *RbacController) UpdateRolePermissions(ctx http.Context) http.Response {
	actor, aerr := actorFrom(ctx)
	if aerr != nil {
		return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
	}

	role := models.Role(ctx.Request().Route("role"))
	perms, ok := requestedPermissions(ctx)
	if !ok {
		// 审计：请求体本身就不合法也是一种尝试，只是它还没走到策略判定。
		// 不记的话，「有人拿一个畸形请求反复敲这道门」在审计里完全看不见。
		const malformed = "请求体必须是 {\"permissions\":[\"log.view\", ...]}"
		auditPermissionDenied(ctx, actor, role, nil,
			rbac.ErrCodeUnknownPermission, malformed)
		return ctx.Response().Json(400, map[string]any{
			"code":  string(rbac.ErrCodeUnknownPermission),
			"error": malformed,
		})
	}

	change, err := rbac.ApplyRolePermissions(role, perms)
	if err != nil {
		status, code := policyErrorResponse(err)
		auditPermissionDenied(ctx, actor, role, perms, code, err.Error())
		log.Printf("[RBAC] 拒绝权限改动：%s(#%d) 试图把 %s 的权限改为 %v —— %v",
			actor.Username, actor.ID, role, perms, err)
		return ctx.Response().Json(status, map[string]any{
			"error": err.Error(),
			"code":  string(code),
		})
	}

	// 审计：谁、对哪个角色、增删了哪些。增删必须分开列——
	// 「谁多了一项能力」与「谁少了一项能力」是两种完全不同的事故。
	auditPermissionChange(ctx, actor, change)

	view := rbac.View()
	return ctx.Response().Json(200, map[string]any{
		"role":     string(change.Role),
		"granted":  change.Granted,
		"revoked":  change.Revoked,
		"source":   view.Source,
		"warnings": view.Warnings,
	})
}

// requestedPermissions 从请求体里取出权限列表。
//
// 第二个返回值区分「没有这个字段」与「有但是空的」：前者是畸形请求（400），
// 后者是一次合法的「把权限全部收走」。两者混在一起的话，「某人被清空了权限」
// 这件事会被报成一句看不懂的格式错误，而现场看不出来是谁被清空的。
func requestedPermissions(ctx http.Context) ([]string, bool) {
	input := ctx.Request().All()
	raw, present := input["permissions"]
	if !present {
		return nil, false
	}
	list, isList := raw.([]any)
	if !isList {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		name, isString := item.(string)
		if !isString {
			return nil, false
		}
		out = append(out, name)
	}
	return out, true
}

// policyErrorResponse 把 rbac 的错误类别翻成 HTTP 状态码与响应里的 code。
//
// 状态码的取法有一个刻意的选择：受保护的那类给 **403 而不是 400**。
// 403 的语义是「你不被允许做这件事」，400 是「你请求写错了」——
// 前者说的是规则，后者说的是格式，而这一格恰恰是格式完全正确、
// 只是规则不允许。给 400 会让前端的错误提示误导人去检查自己的代码。
//
// 冲突那一类给 409：请求合法、也写进库了，但重载不通过所以已回滚。
// 它必须与前两类区分开，因为处置完全不同——前两类是「改不了」，
// 这一类是「这次没生效，刷新看看」。
func policyErrorResponse(err error) (int, rbac.PolicyErrorCode) {
	switch rbac.CodeOf(err) {
	case rbac.ErrCodeProtected:
		return 403, rbac.ErrCodeProtected
	case rbac.ErrCodeUnknownRole, rbac.ErrCodeUnknownPermission:
		return 400, rbac.CodeOf(err)
	case rbac.ErrCodeConflict:
		return 409, rbac.ErrCodeConflict
	default:
		// 拿不到类别就按「服务端状态不对」处理，而不是按 400：
		// 说成 400 会让运维去查前端，而真正的原因是数据库或磁盘。
		return 503, rbac.ErrCodeUnavailable
	}
}

// auditPermissionChange 记录一次成功的权限改动。
//
// Detail 里 granted 与 revoked **必须分开**：出事故时要回答的问题通常是
// 「谁多了一项能力」或「谁失去了哪一项」，把两者混在一个数组里就得靠
// 人工比对才知道方向。
func auditPermissionChange(ctx http.Context, actor models.User, change *rbac.Change) {
	recordAudit(ctx, actor, audit.Record{
		Action:     "rbac.role_permissions",
		Summary:    "调整 " + change.Role.Label() + " 的权限",
		TargetType: "role",
		TargetID:   string(change.Role),
		Detail: map[string]any{
			"role":    string(change.Role),
			"granted": change.Granted,
			"revoked": change.Revoked,
			"source":  rbac.Source(),
		},
	})
}

// auditPermissionDenied 记录一次被拒绝的权限改动尝试。
//
// **被拒绝的尝试必须落审计**，而且它比成功的改动更值得被看见：有人试图削掉
// 超管的 system.maintain，是这套系统里最该被人知道的一个时刻——
// 而它恰好是**什么都没发生**的那种时刻，只看「改了什么」的审计永远看不到它。
//
// Action 用单独的一个值而不是复用 rbac.role_permissions：审计页按 action
// 筛选，把两者混在一起的话，「谁试图削掉系统维护权限」这条线索就得靠
// 逐条翻 detail 才找得到。
func auditPermissionDenied(ctx http.Context, actor models.User, role models.Role,
	requested []string, code rbac.PolicyErrorCode, reason string) {
	// requested 在「请求体畸形」那条路径上是 nil（压根没解析出权限列表），
	// 而 nil 切片会被 audit.Write 序列化成 "requested":null。
	//
	// 归一成空数组而不是省略这个键：省略之后，「他提交了空的」与「我们没
	// 记下来」在审计里再也分不开，而这两件事要查的地方完全不同（一次是他
	// 在试探，一次是审计管线坏了）。给 [] 两者才分得开，形状也才和
	// granted/revoked 一致——同一个 audit_logs.detail 里不该有两种空值写法。
	if requested == nil {
		requested = []string{}
	}
	recordAudit(ctx, actor, audit.Record{
		Action:     "rbac.role_permissions_denied",
		Summary:    "试图调整 " + role.Label() + " 的权限，被拒绝：" + reason,
		TargetType: "role",
		TargetID:   string(role),
		Detail: map[string]any{
			"role":   string(role),
			"reason": reason,
			"code":   string(code),
			// 请求里被提交的那一组也要留着：只知道「他被拒了」不知道
			// 「他想干什么」，这条审计就答不了「他是不是在反复尝试」。
			"requested": requested,
		},
	})
}
