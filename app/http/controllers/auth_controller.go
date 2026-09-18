package controllers

import (
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
)

type AuthController struct{}

func NewAuthController() *AuthController {
	return &AuthController{}
}

// 手动签发 JWT，绕过 Guard（Guard 内部可能因 cache 未初始化而 panic）
func generateToken(userID uint) (string, error) {
	secret := facades.Config().GetString("jwt.secret")
	if secret == "" {
		return "", fmt.Errorf("jwt.secret 未配置")
	}

	ttl := facades.Config().GetInt("jwt.ttl")
	if ttl == 0 {
		ttl = 60
	}

	now := time.Now()
	claims := jwt.MapClaims{
		"key": strconv.FormatUint(uint64(userID), 10),
		"exp": now.Add(time.Duration(ttl) * time.Minute).Unix(),
		"iat": now.Unix(),
		"sub": "user",
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

func (c *AuthController) Login(ctx http.Context) http.Response {
	username := ctx.Request().Input("username", "")
	password := ctx.Request().Input("password", "")

	if username == "" || password == "" {
		return ctx.Response().Json(400, map[string]any{"error": "用户名和密码不能为空"})
	}

	var user models.User
	if err := facades.Orm().Query().Where("username = ?", username).First(&user); err != nil {
		log.Printf("[AUTH] 用户不存在: %s", username)
		return ctx.Response().Json(401, map[string]any{"error": "用户名或密码错误"})
	}

	if !facades.Hash().Check(password, user.Password) {
		log.Printf("[AUTH] 密码错误: %s", username)
		return ctx.Response().Json(401, map[string]any{"error": "用户名或密码错误"})
	}

	token, err := generateToken(user.ID)
	if err != nil {
		log.Printf("[AUTH] 生成令牌失败: %v", err)
		return ctx.Response().Json(500, map[string]any{"error": "生成令牌失败: " + err.Error()})
	}

	return ctx.Response().Json(200, map[string]any{
		"token": token,
		"user": map[string]any{
			"id":           user.ID,
			"username":     user.Username,
			"display_name": user.DisplayName,
			"role":         user.Role,
		},
	})
}

func (c *AuthController) Register(ctx http.Context) http.Response {
	username := ctx.Request().Input("username", "")
	password := ctx.Request().Input("password", "")
	displayName := ctx.Request().Input("display_name", "")
	role := ctx.Request().Input("role", "director")

	if username == "" || password == "" {
		return ctx.Response().Json(400, map[string]any{"error": "用户名和密码不能为空"})
	}

	hashedPassword, err := facades.Hash().Make(password)
	if err != nil {
		log.Printf("[AUTH] 密码加密失败: %v", err)
		return ctx.Response().Json(500, map[string]any{"error": "密码加密失败"})
	}

	user := models.User{
		Username:    username,
		Password:    hashedPassword,
		DisplayName: displayName,
		Role:        role,
	}

	if err := facades.Orm().Query().Create(&user); err != nil {
		log.Printf("[AUTH] 创建用户失败: %v", err)
		return ctx.Response().Json(409, map[string]any{"error": "用户名已存在"})
	}

	return ctx.Response().Json(201, map[string]any{
		"id":           user.ID,
		"username":     user.Username,
		"display_name": user.DisplayName,
		"role":         user.Role,
	})
}

func (c *AuthController) Profile(ctx http.Context) http.Response {
	userID := ctx.Value("user_id")

	var user models.User
	if err := facades.Orm().Query().Where("id = ?", userID).First(&user); err != nil {
		return ctx.Response().Json(404, map[string]any{"error": "用户不存在"})
	}

	return ctx.Response().Json(200, map[string]any{
		"id":           user.ID,
		"username":     user.Username,
		"display_name": user.DisplayName,
		"role":         user.Role,
	})
}
