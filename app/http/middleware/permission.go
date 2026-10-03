package middleware

import (
	"log"

	contractshttp "github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
	"smart-mzcmc/app/rbac"
)

// PermissionMiddleware 校验调用者是否持有某个**具名权限**。
//
// 为什么换掉 RequireRole（等级门槛）：
//
//	等级只能表达高低，表达不了「负责人能看、不能改」。负责人 leader(40)
//	比导播 director(30) 还高，所以任何 min <= 40 的门槛他都过得去——
//	「负责人只读 + 管采访点 + 授权成员」这条业务需求在等级制里无解，
//	而人肉往路由上补角色名只会得到一堆互相矛盾的例外。
//	现在路由声明「我需要 project.member」，策略文件声明「谁能拿到它」
//	（见 app/rbac）。等级仍然保留，但只管「能不能操作某个人」，
//	不再管「能不能进某个接口」——那两件事本来就不该由同一个机制管。
//
// 与 RoleMiddleware 一样，每次都查库而不是信任令牌里的角色：管理员可以在
// 后台改角色，而令牌在过期前可能长达 60 分钟。
//
// 注意：本文件所有中断响应都用 `Response().Json(...).Abort()` 链式写法。
// 分成两行写会被 gin 重置成 400 + 空 body，客户端拿不到任何错误信息。
type PermissionMiddleware struct {
	perm string
}

func (m *PermissionMiddleware) Signature() string {
	return "perm=" + m.perm
}

func (m *PermissionMiddleware) Handle(ctx contractshttp.Context) {
	userID, ok := ctx.Value("user_id").(uint)
	if !ok || userID == 0 {
		ctx.Response().Json(401, map[string]any{"error": "未提供认证令牌"}).Abort()
		return
	}

	// 必须用 models.User 承载结果：Goravel 的 ORM 依赖模型元数据解析字段
	// 映射，查进匿名 struct 会直接失败（表现为「用户不存在」）。
	//
	// **不能只看 err**：SQLite 驱动下 First 查不到记录时不返回错误，只是把
	// 结构体留成零值。只判 err 的写法会让「用户已被删除」这道守卫完全失效
	// ——本项目踩过，已删除账号的令牌在有效期内畅通无阻（见 jwt.go 的说明）。
	var user models.User
	if err := facades.Orm().Query().Where("id = ?", userID).First(&user); err != nil || user.ID == 0 {
		ctx.Response().Json(401, map[string]any{"error": "用户不存在或已被删除"}).Abort()
		return
	}

	role := models.Role(user.Role)
	if !role.Valid() {
		// users.role 是 varchar(20) 且没有 CHECK 约束。出现非法值说明数据
		// 被绕过本项目的路径改过，不能报成「权限不足」糊弄过去——那会让人
		// 一直去改权限，而真正的原因是数据坏了。
		ctx.Response().Json(403, map[string]any{
			"error": "账号角色异常（" + user.Role + "），请联系超级管理员修复",
		}).Abort()
		return
	}

	// 放进 ctx 供控制器做「不能操作同级或更高」这类判断，省掉重复查库。
	//
	// 这两行**不是样板代码，删不得**：controllers/authz.go 的 actorFrom 与
	// app/audit 都从 ctx 里取 "user"（连 actor.Username 都要），少了任何一行
	// 下面所有控制器都会拿到 401。
	ctx.WithValue("role", role)
	ctx.WithValue("user", user)

	if rbac.Can(role, m.perm) {
		ctx.Request().Next()
		return
	}

	log.Printf("[Authz] 拒绝：用户 %s(#%d, %s) 缺少权限 %s",
		user.Username, user.ID, user.Role, m.perm)
	// 错误信息只讲角色与权限，不讲项目授权：这是两件不同的事，「不是成员」
	// 由 RequireProjectMember 单独报。混在一起会让人去查错的方向
	//（重新配项目授权，而真正的原因是自己的角色没有这项能力）。
	ctx.Response().Json(403, map[string]any{
		"error": rbac.DeniedMessage(m.perm, role),
	}).Abort()
}

// RequirePermission 返回校验「持有某项具名权限」的中间件。
//
// 用法：r.Prefix("/api/admin").Middleware(RequirePermission(rbac.PermUserView))…
//
// 注意**不要**在这里传等级（RequireRole）。两个机制是分工的：
//   - RequirePermission 回答「能不能进这个接口」，看 app/rbac/policy.csv；
//   - Role.AtLeast 回答「能不能操作这个人」，留在 controllers/authz.go。
//     混着来就会出现「后勤能删掉导播账号」这种坑（等级高于管理员的后勤
//     通过了门槛，却在控制器里本该被拦住的那条规则上没被拦住）。
func RequirePermission(perm string) contractshttp.Middleware {
	return &PermissionMiddleware{perm: perm}
}
