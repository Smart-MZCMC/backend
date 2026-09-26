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
	userID, _ := strconv.ParseUint(key, 10, 64)
	if userID == 0 {
		ctx.Response().Json(401, map[string]any{"error": "无效的用户ID"}).Abort()
		return
	}

	ctx.WithValue("user_id", uint(userID))
	ctx.Request().Next()
}

func Jwt() contractshttp.Middleware {
	return &JwtMiddleware{}
}

// RoleMiddleware 校验调用者的角色。
//
// 为什么必须有：Jwt 中间件只验证令牌「是否有效」，不关心持有者是谁。
// 少了这一层，任何登录用户（包括最低权限的导播）都能调管理接口。
//
// 注意：本文件所有中断响应都用 `Response().Json(...).Abort()` 链式写法。
// 分成两行写（先 Json 再 ctx.Request().Abort()）会被 gin 重置成
// 400 + 空 body，客户端拿不到任何错误信息。
type RoleMiddleware struct {
	roles []string
}

func (m *RoleMiddleware) Signature() string {
	return "role:" + strings.Join(m.roles, ",")
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
	if err := facades.Orm().Query().Where("id = ?", userID).First(&user); err != nil {
		ctx.Response().Json(401, map[string]any{"error": "用户不存在或已被删除"}).Abort()
		return
	}

	for _, allowed := range m.roles {
		if user.Role == allowed {
			ctx.Request().Next()
			return
		}
	}

	ctx.Response().Json(403, map[string]any{
		"error": "权限不足：该操作需要 " + strings.Join(m.roles, "/") + " 角色，当前为 " + user.Role,
	}).Abort()
}

// RequireRole 返回一个校验调用者角色的中间件。
func RequireRole(roles ...string) contractshttp.Middleware {
	return &RoleMiddleware{roles: roles}
}
