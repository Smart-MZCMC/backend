package middleware

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	contractshttp "github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
)

type JwtMiddleware struct{}

func (m *JwtMiddleware) Signature() string {
	return "jwt"
}

func (m *JwtMiddleware) Handle(ctx contractshttp.Context) {
	tokenStr := ctx.Request().Header("Authorization", "")
	tokenStr = strings.TrimPrefix(tokenStr, "Bearer ")

	if tokenStr == "" {
		ctx.Response().Json(401, map[string]any{"error": "未提供认证令牌"}).Abort()
		return
	}

	secret := facades.Config().GetString("jwt.secret")
	if secret == "" {
		ctx.Response().Json(500, map[string]any{"error": "JWT密钥未配置"}).Abort()
		return
	}

	// 手动解析 JWT
	token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	})

	if err != nil || !token.Valid {
		ctx.Response().Json(401, map[string]any{"error": "令牌无效或已过期"}).Abort()
		return
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		ctx.Response().Json(401, map[string]any{"error": "令牌解析失败"}).Abort()
		return
	}

	key, _ := claims["key"].(string)
	userID, err := strconv.ParseUint(key, 10, 64)
	if err != nil || userID == 0 {
		ctx.Response().Json(401, map[string]any{"error": "无效的用户ID"}).Abort()
		return
	}

	// 校验令牌版本。
	//
	// 用户改密码时 token_version 会递增，因此这里能立刻判定令牌已失效，而不必
	// 等 JWT_TTL（默认 60 分钟）自然过期。只取两列：这是每个请求都会走到的
	// 路径，不值得把密码哈希也读出来。
	//
	// 顺带修掉一个既有漏洞：Jwt 中间件此前完全不查库，所以**被删除的用户的
	// 令牌在有效期内依然畅通无阻**。既然现在要查，顺手把「用户不存在」一并
	// 拦下来。
	//
	// 令牌里没有 ver 字段的（本次改动前签发的）按 0 处理，与库里的默认值一致，
	// 升级后老令牌不会被误杀。
	// 注意 err != nil || X.ID == 0 这个判断：**不能只看 error**。
	// SQLite 驱动下 First 查不到记录时不返回错误，只是把目标结构体留成零值；
	// 只判 err 的写法会让「用户/记录不存在」这道守卫完全失效——实测已删除账号
	// 的令牌在有效期内照样畅通无阻（profile / 改资料 / 读日志全部 200）。
	// 所有表都是自增主键、ID 从 1 起，所以 ID == 0 即代表没查到。
	var tokenUser models.User
	if err := facades.Orm().Query().Select("id", "token_version").
		Where("id = ?", userID).First(&tokenUser); err != nil || tokenUser.ID == 0 {
		ctx.Response().Json(401, map[string]any{"error": "用户不存在或已被删除"}).Abort()
		return
	}

	claimVersion, _ := claims["ver"].(float64)
	if int(claimVersion) != tokenUser.TokenVersion {
		ctx.Response().Json(401, map[string]any{
			"error": "登录状态已失效，请重新登录（密码可能已变更）",
		}).Abort()
		return
	}

	ctx.WithValue("user_id", uint(userID))
	ctx.Request().Next()
}

func Jwt() contractshttp.Middleware {
	return &JwtMiddleware{}
}

// RoleMiddleware 校验调用者是否达到最低角色等级。
//
// 为什么必须有：Jwt 中间件只验证令牌「是否有效」，不关心持有者是谁。
// 少了这一层，任何登录用户（包括最低权限的导播）都能调管理接口。
//
// 语义是「等级 >= min」，不是「角色等于 min」。加超级管理员时这一点很关键：
// 早期写法是对允许的角色名做等值比较，于是 RequireRole("admin") 并不放行
// super_admin，每加一个更高角色都得回到每个调用点把名字补一遍——漏一处就是
// 一个越权或误拒的入口。改成等级后，高角色自动继承低角色的接口访问权。
//
// 注意：本文件所有中断响应都用 `Response().Json(...).Abort()` 链式写法。
// 分成两行写（先 Json 再 ctx.Request().Abort()）会被 gin 重置成
// 400 + 空 body，客户端拿不到任何错误信息。
type RoleMiddleware struct {
	min models.Role
}

func (m *RoleMiddleware) Signature() string {
	return "role>=" + string(m.min)
}

func (m *RoleMiddleware) Handle(ctx contractshttp.Context) {
	userID, ok := ctx.Value("user_id").(uint)
	if !ok || userID == 0 {
		ctx.Response().Json(401, map[string]any{"error": "未提供认证令牌"}).Abort()
		return
	}

	// 每次都查库而不是信任令牌里的角色：管理员可以在后台改角色，
	// 令牌在过期前可能长达 60 分钟，写进 claims 会让降权延迟生效。
	//
	// 必须用 models.User 承载结果：Goravel 的 ORM 依赖模型元数据解析
	// 字段映射，查进匿名 struct 会直接失败（表现为「用户不存在」）。
	var user models.User
	if err := facades.Orm().Query().Where("id = ?", userID).First(&user); err != nil || user.ID == 0 {
		ctx.Response().Json(401, map[string]any{"error": "用户不存在或已被删除"}).Abort()
		return
	}

	role := models.Role(user.Role)
	if !role.Valid() {
		// users.role 是 varchar(20) 且没有 CHECK 约束。出现非法值说明数据
		// 被绕过本项目的路径改过，不能当成「权限不足」糊弄过去——那会让人
		// 一直找不到真正的原因。
		ctx.Response().Json(403, map[string]any{
			"error": "账号角色异常（" + user.Role + "），请联系超级管理员修复",
		}).Abort()
		return
	}

	// 放进 ctx 供控制器做「不能操作同级或更高」这类判断，省掉重复查库。
	ctx.WithValue("role", role)
	ctx.WithValue("user", user)

	if role.AtLeast(m.min) {
		ctx.Request().Next()
		return
	}

	ctx.Response().Json(403, map[string]any{
		"error": "权限不足：该操作需要 " + m.min.Label() + " 及以上角色，当前为 " + role.Label(),
	}).Abort()
}

// RequireRole 返回一个校验调用者角色等级的中间件。
//
// 传入多个角色时取其中权限最低的一个作为门槛，例如
// RequireRole(RoleAdmin, RoleLeader) 等价于 RequireRole(RoleLeader)。
func RequireRole(min ...models.Role) contractshttp.Middleware {
	floor := models.RoleSuperAdmin
	for _, r := range min {
		if r.Level() < floor.Level() {
			floor = r
		}
	}
	return &RoleMiddleware{min: floor}
}
