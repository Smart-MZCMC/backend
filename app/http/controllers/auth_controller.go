package controllers

import (
	"fmt"
	"log"
	"strconv"
	"strings"
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

// 用户名的合法字符：字母、数字、下划线、点、中文。
// 不用正则是因为框架里没有引入 regexp 依赖，且这只是登录名而非 SQL 片段。
func validUsername(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '.', r == '-':
		case r >= 0x4e00 && r <= 0x9fff: // 中文
		default:
			return false
		}
	}
	return true
}

const minPasswordLength = 6

// Register 创建用户。
//
// 这是公开路由（没有挂在 JWT 中间件组里），但分两种模式：
//
//  1. 引导模式：用户表为空时，第一个注册的人自动成为管理员，且请求里
//     携带的 role 会被忽略。这是全新部署拿到第一个管理员的唯一途径，
//     不需要任何预置账号或额外密钥。
//  2. 常态：已经有用户之后，注册必须由管理员登录态发起，且 role 只能是
//     admin / director 之一。管理后台的「新建用户」走的也是这个接口。
//
// 早期版本既不限制引导条件、也不校验 role，导致任何能访问到端口的人
// 都能直接开一个 admin 账号并调用全部管理接口。
func (c *AuthController) Register(ctx http.Context) http.Response {
	username := strings.TrimSpace(ctx.Request().Input("username", ""))
	password := ctx.Request().Input("password", "")
	displayName := strings.TrimSpace(ctx.Request().Input("display_name", ""))
	requestedRole := ctx.Request().Input("role", "")

	// 判断是否处于引导模式：用户表为空。
	// 用 Count 而不是 First —— First 在结果为空时是否返回 ErrRecordNotFound
	// 依赖驱动实现，不可靠；Count 的语义没有歧义。
	userCount, err := facades.Orm().Query().Model(&models.User{}).Count()
	if err != nil {
		log.Printf("[AUTH] 查询用户数失败: %v", err)
		return ctx.Response().Json(500, map[string]any{"error": "查询用户数失败"})
	}
	bootstrap := userCount == 0

	role := requestedRole
	if bootstrap {
		// 引导模式：第一个账号固定为管理员，忽略请求里的 role。
		role = "admin"
	} else if verr := c.requireAdmin(ctx); verr != nil {
		// 鉴权放在参数校验之前：这是个公开路由，先校验参数等于把
		// 密码策略与用户名规则变成匿名可探测的预言机。
		return ctx.Response().Json(verr.status, map[string]any{"error": verr.message})
	}

	if username == "" || password == "" {
		return ctx.Response().Json(400, map[string]any{"error": "用户名和密码不能为空"})
	}
	if !validUsername(username) {
		return ctx.Response().Json(400, map[string]any{
			"error": "用户名只能包含字母、数字、下划线、点、短横线与中文，且不超过 64 个字符",
		})
	}
	if len(password) < minPasswordLength {
		return ctx.Response().Json(400, map[string]any{
			"error": fmt.Sprintf("密码至少 %d 位", minPasswordLength),
		})
	}
	if !bootstrap {
		if role == "" {
			role = "director"
		}
		if role != "admin" && role != "director" {
			return ctx.Response().Json(400, map[string]any{
				"error": "角色只能是 admin 或 director",
			})
		}
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

	if bootstrap {
		log.Printf("[AUTH] 引导模式：已创建首个管理员账号 %s", username)
	}

	return ctx.Response().Json(201, map[string]any{
		"id":           user.ID,
		"username":     user.Username,
		"display_name": user.DisplayName,
		"role":         user.Role,
	})
}

type registerError struct {
	status  int
	message string
}

// requireAdmin 校验调用者是管理员。
//
// Register 挂在公开路由上，拿不到 JWT 中间件写入的上下文，
// 所以这里自己解析一次 Authorization 头。
func (c *AuthController) requireAdmin(ctx http.Context) *registerError {
	raw := strings.TrimPrefix(ctx.Request().Header("Authorization", ""), "Bearer ")
	if raw == "" {
		return &registerError{401, "系统已有账号，创建用户需要管理员登录"}
	}

	secret := facades.Config().GetString("jwt.secret")
	if secret == "" {
		return &registerError{500, "JWT密钥未配置"}
	}
	token, err := jwt.Parse(raw, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil || !token.Valid {
		return &registerError{401, "令牌无效或已过期，请重新登录"}
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return &registerError{401, "令牌解析失败"}
	}
	key, _ := claims["key"].(string)
	userID, _ := strconv.ParseUint(key, 10, 64)
	if userID == 0 {
		return &registerError{401, "无效的用户ID"}
	}

	var user models.User
	if err := facades.Orm().Query().Where("id = ?", userID).First(&user); err != nil {
		return &registerError{401, "用户不存在或已被删除"}
	}
	if user.Role != "admin" {
		return &registerError{403, "权限不足：只有管理员可以创建用户"}
	}
	return nil
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
