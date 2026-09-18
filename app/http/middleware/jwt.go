package middleware

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	contractshttp "github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"
)

type JwtMiddleware struct{}

func (m *JwtMiddleware) Signature() string {
	return "jwt"
}

func (m *JwtMiddleware) Handle(ctx contractshttp.Context) {
	tokenStr := ctx.Request().Header("Authorization", "")
	tokenStr = strings.TrimPrefix(tokenStr, "Bearer ")

	if tokenStr == "" {
		ctx.Response().Json(401, map[string]any{"error": "未提供认证令牌"})
		ctx.Request().Abort()
		return
	}

	secret := facades.Config().GetString("jwt.secret")
	if secret == "" {
		ctx.Response().Json(500, map[string]any{"error": "JWT密钥未配置"})
		ctx.Request().Abort()
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
		ctx.Response().Json(401, map[string]any{"error": "令牌无效或已过期"})
		ctx.Request().Abort()
		return
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		ctx.Response().Json(401, map[string]any{"error": "令牌解析失败"})
		ctx.Request().Abort()
		return
	}

	key, _ := claims["key"].(string)
	userID, _ := strconv.ParseUint(key, 10, 64)
	if userID == 0 {
		ctx.Response().Json(401, map[string]any{"error": "无效的用户ID"})
		ctx.Request().Abort()
		return
	}

	ctx.WithValue("user_id", uint(userID))
	ctx.Request().Next()
}

func Jwt() contractshttp.Middleware {
	return &JwtMiddleware{}
}
