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
//  1. 引导模式：用户表为空时，第一个注册的人自动成为超级管理员，且请求里
//     携带的 role 会被忽略。这是全新部署拿到第一个管理员的唯一途径，
//  2. 常态：已经有用户之后，注册必须由管理员及以上登录态发起，且只能授予
//     不高于自己的角色。管理后台的「新建用户」走的也是这个接口。
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
	var actor models.User
	if bootstrap {
		// 引导模式：第一个账号固定为超级管理员，忽略请求里的 role。
		//
		// 必须是超级管理员而不是管理员：只有超级管理员能授予超管角色
		// （见 guardGrant）。若引导出来的是管理员，就再没有人能创建超管，
		// 系统会停在一个「谁也管不了谁」的状态——系统更新、角色调整全都做不了。
		role = string(models.RoleSuperAdmin)
	} else {
		a, aerr := resolveActor(ctx, "系统已有账号，创建用户")
		if aerr != nil {
			// 鉴权放在参数校验之前：这是个公开路由，先校验参数等于把
			// 密码策略与用户名规则变成匿名可探测的预言机。
			return ctx.Response().Json(aerr.status, map[string]any{"error": aerr.message})
		}
		actor = a
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
			role = string(models.RoleDirector)
		}
		newRole := models.Role(role)
		if !newRole.Valid() {
			return ctx.Response().Json(400, map[string]any{
				"error": "角色非法，可选值：" + roleOptionsText(),
			})
		}
		if gerr := guardGrant(actor, newRole); gerr != nil {
			return ctx.Response().Json(gerr.status, map[string]any{"error": gerr.message})
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
		log.Printf("[AUTH] 引导模式：已创建首个超级管理员账号 %s", username)
	}

	return ctx.Response().Json(201, map[string]any{
		"id":           user.ID,
		"username":     user.Username,
		"display_name": user.DisplayName,
		"role":         user.Role,
	})
}

// roleOptionsText 拼出「a / b / c」形式的角色清单，用于报错提示。
func roleOptionsText() string {
	roles := models.AllRoles()
	parts := make([]string, 0, len(roles))
	for _, r := range roles {
		parts = append(parts, string(r))
	}
	return strings.Join(parts, " / ")
}

// BootstrapStatus 报告系统是否还处于「未初始化」状态。
//
// 全新部署时用户表为空，登录无从谈起。管理后台据此把登录表单换成
// 「创建首个管理员」表单，避免用户对着一个永远登不进去的页面发呆。
//
// 这是公开路由，只回答布尔值、不暴露任何账号信息。它也不构成信息泄露：
// 任何人本来就可以直接尝试调 /api/auth/register 去抢注管理员。
// 真正的防护是首注之后该接口自动收紧为「仅管理员可调用」。
func (c *AuthController) BootstrapStatus(ctx http.Context) http.Response {
	count, err := facades.Orm().Query().Model(&models.User{}).Count()
	if err != nil {
		log.Printf("[AUTH] 查询用户数失败: %v", err)
		return ctx.Response().Json(500, map[string]any{"error": "查询失败"})
	}
	return ctx.Response().Json(200, map[string]any{"needs_bootstrap": count == 0})
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
