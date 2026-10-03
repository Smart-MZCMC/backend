package middleware

import (
	"log"
	"strconv"

	contractshttp "github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
)

// ProjectMemberMiddleware 校验调用者是不是目标项目的成员。
//
// 为什么必须有：user_projects 这张表从第一天就在，但此前只被管理接口
// 增删查，从未参与任何鉴权判断。结果是
//   - GET /api/admin/projects 是裸 Find，任何管理员都能看到全部项目；
//   - POST /api/locks/:projectId/acquire 直接信 URL 里的 projectId；
//   - WebSocket 只要知道 project_id 就能监听整个项目的实时消息。
//
// 管理员及以上绕过：他们本来就要管理所有项目，逐个配授权没有意义。
//
// 开关：REQUIRE_PROJECT_MEMBERSHIP，默认 false。关闭时只把「本来会被拦下
// 的请求」写进日志而不拦截，让现场可以先把授权配齐、观察一段时间再打开——
// 直接默认强校验会让存量部署当场连不上。
type ProjectMemberMiddleware struct{}

func (m *ProjectMemberMiddleware) Signature() string {
	return "project-member"
}

func (m *ProjectMemberMiddleware) Handle(ctx contractshttp.Context) {
	userID, ok := ctx.Value("user_id").(uint)
	if !ok || userID == 0 {
		ctx.Response().Json(401, map[string]any{"error": "未提供认证令牌"}).Abort()
		return
	}

	// 这里必须自己查库拿角色，不能依赖 RequirePermission / RequireRole 写进
	// ctx 的 "user"：成员校验挂的那几条路由上，这两者压根没有跑过
	// （成员校验就是它们的前一道门，不是一道更粗的门）。
	var user models.User
	if err := facades.Orm().Query().Select("id", "username", "role").
		Where("id = ?", userID).First(&user); err != nil || user.ID == 0 {
		ctx.Response().Json(401, map[string]any{"error": "用户不存在或已被删除"}).Abort()
		return
	}

	role := models.Role(user.Role)
	if role.AtLeast(models.RoleAdmin) {
		ctx.Request().Next()
		return
	}

	projectID := resolveProjectID(ctx)
	if projectID == 0 {
		ctx.Response().Json(400, map[string]any{
			"error": "无法确定目标项目：需要 project_id 参数或 /:projectId 路径段",
		}).Abort()
		return
	}

	allowed := models.IsProjectMember(userID, projectID)

	if !facades.Config().GetBool("authz.require_project_membership", false) {
		if !allowed {
			// 只记录不拦截。这条日志就是打开开关前的依据：谁会被拦、被哪个
			// 项目拦，先把名单看清楚再决定什么时候开。
			log.Printf("[Authz] 用户 %s(#%d, %s) 访问了未授权的项目 %d"+
				"（REQUIRE_PROJECT_MEMBERSHIP=false，仅记录，未拦截）",
				user.Username, user.ID, user.Role, projectID)
		}
		ctx.Request().Next()
		return
	}

	if !allowed {
		log.Printf("[Authz] 拒绝：用户 %s(#%d) 不是项目 %d 的成员", user.Username, user.ID, projectID)
		ctx.Response().Json(403, map[string]any{
			"error": "无权访问该项目：请联系管理员分配项目权限",
		}).Abort()
		return
	}

	ctx.Request().Next()
}

// resolveProjectID 按路径参数、再按表单/查询参数取项目 ID。
//
// 三种写法都要认：锁接口用 :projectId，管理接口用 :id，而按项目统计、
// 消息列表这类既可能出现在路径里也可能出现在查询串里。
func resolveProjectID(ctx contractshttp.Context) uint {
	for _, key := range []string{"projectId", "project_id", "id"} {
		if raw := ctx.Request().Route(key); raw != "" {
			if id, err := strconv.ParseUint(raw, 10, 64); err == nil {
				return uint(id)
			}
		}
	}
	if raw := ctx.Request().Input("project_id", ""); raw != "" {
		if id, err := strconv.ParseUint(raw, 10, 64); err == nil {
			return uint(id)
		}
	}
	return 0
}

// RequireProjectMember 返回项目成员校验中间件。
func RequireProjectMember() contractshttp.Middleware {
	return &ProjectMemberMiddleware{}
}
