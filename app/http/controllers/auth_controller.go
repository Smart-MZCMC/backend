package controllers

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/audit"
	"smart-mzcmc/app/models"
)

type AuthController struct{}

func NewAuthController() *AuthController {
	return &AuthController{}
}

// 手动签发 JWT，绕过 Guard（Guard 内部可能因 cache 未初始化而 panic）
//
// user 的 token_version 会写进 ver 声明：用户改密码时库里的值递增，三处
// 验签点都比对它，于是「改完密码立刻让所有旧令牌作废」才成立，而不是等
// JWT_TTL（默认 60 分钟）自然过期。
func generateToken(user models.User) (string, error) {
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
		"key": strconv.FormatUint(uint64(user.ID), 10),
		"ver": user.TokenVersion,
		"exp": now.Add(time.Duration(ttl) * time.Minute).Unix(),
		"iat": now.Unix(),
		"sub": "user",
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// AdminMinRole 返回允许登录管理后台网页的最低角色等级。
//
// 配置项写错（比如填了一个不存在的角色名）时退回 logistics 并记日志：
// 退回 0 会变成「谁都能进后台」，比默认拦掉导播危险得多。
func AdminMinRole() models.Role {
	raw := strings.TrimSpace(facades.Config().GetString("authz.admin_min_role", "logistics"))
	role := models.Role(raw)
	if !role.Valid() {
		log.Printf("[AUTH] authz.admin_min_role=%q 不是合法角色，回退为 %s", raw, models.RoleLogistics)
		return models.RoleLogistics
	}
	return role
}

// AdminLogin 是管理后台网页专用的登录入口。
//
// 与 Login 的唯一区别：登录成功后额外校验一次角色等级，够不到门槛的直接拒绝。
// 之所以要单独一个接口而不是在 Login 里拦，是因为导播端/采访端/解说端这些
// 原生应用共用 /api/auth/login——导播账号本就该能登录（它用原生界面），
// 在那里按网页后台的门槛拦会把原生端一起打死。
func (c *AuthController) AdminLogin(ctx http.Context) http.Response {
	username := strings.TrimSpace(ctx.Request().Input("username", ""))
	password := ctx.Request().Input("password", "")

	if username == "" || password == "" {
		return ctx.Response().Json(400, map[string]any{"error": "用户名和密码不能为空"})
	}

	var user models.User
	if err := facades.Orm().Query().Where("username = ?", username).First(&user); err != nil || user.ID == 0 {
		log.Printf("[AUTH] 用户不存在: %s", username)
		return ctx.Response().Json(401, map[string]any{"error": "用户名或密码错误"})
	}

	if !facades.Hash().Check(password, user.Password) {
		log.Printf("[AUTH] 密码错误: %s", username)
		return ctx.Response().Json(401, map[string]any{"error": "用户名或密码错误"})
	}

	// 口令校验之后才判角色：先确认「这个人是谁」，再说「他能不能进后台」。
	// 反过来会让人拿到一个「权限不足」的提示，从而确认该账号存在。
	if min := AdminMinRole(); !models.Role(user.Role).AtLeast(min) {
		log.Printf("[AUTH] %s(%s) 角色低于管理后台门槛 %s，拒绝网页登录", user.Username, user.Role, min)
		return ctx.Response().Json(403, map[string]any{
			"error": "该账号没有管理后台的访问权限（需要" + min.Label() + "及以上）。" +
				"如需调整，可由管理员修改环境变量 ADMIN_MIN_ROLE。",
		})
	}

	token, err := generateToken(user)
	if err != nil {
		log.Printf("[AUTH] 生成令牌失败: %v", err)
		return ctx.Response().Json(500, map[string]any{"error": "生成令牌失败: " + err.Error()})
	}

	return ctx.Response().Json(200, map[string]any{
		"token": token,
		"user":  userPayload(user),
	})
}

func (c *AuthController) Login(ctx http.Context) http.Response {
	username := ctx.Request().Input("username", "")
	password := ctx.Request().Input("password", "")

	if username == "" || password == "" {
		return ctx.Response().Json(400, map[string]any{"error": "用户名和密码不能为空"})
	}

	var user models.User
	if err := facades.Orm().Query().Where("username = ?", username).First(&user); err != nil || user.ID == 0 {
		log.Printf("[AUTH] 用户不存在: %s", username)
		return ctx.Response().Json(401, map[string]any{"error": "用户名或密码错误"})
	}

	if !facades.Hash().Check(password, user.Password) {
		log.Printf("[AUTH] 密码错误: %s", username)
		return ctx.Response().Json(401, map[string]any{"error": "用户名或密码错误"})
	}

	token, err := generateToken(user)
	if err != nil {
		log.Printf("[AUTH] 生成令牌失败: %v", err)
		return ctx.Response().Json(500, map[string]any{"error": "生成令牌失败: " + err.Error()})
	}

	return ctx.Response().Json(200, map[string]any{
		"token": token,
		"user":  userPayload(user),
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

	// 先查用户名再插入：真正的唯一性由索引兜底，但这里查一次能让绝大多数
	// 「重名」得到精确提示，而不是笼统的「用户名或邮箱已被占用」。
	taken, err := facades.Orm().Query().Model(&models.User{}).
		Where("username = ?", username).Count()
	if err != nil {
		log.Printf("[AUTH] 查询用户名失败: %v", err)
		return ctx.Response().Json(500, map[string]any{"error": "创建用户失败"})
	}
	if taken > 0 {
		return ctx.Response().Json(409, map[string]any{"error": "用户名已存在"})
	}

	if err := facades.Orm().Query().Create(&user); err != nil {
		log.Printf("[AUTH] 创建用户失败: %v", err)
		// 不要在此断言是哪个约束冲突：users 上现在有 username 与 email 两个
		// 唯一索引，写死「用户名已存在」在邮箱冲突时会给出完全指错方向的提示
		// （之前加 email 列时就踩过一次，插空邮箱的第二个用户被报成用户名重复）。
		// 用户名是否重复上面已经查过了，所以走到这里更可能是别的约束或底层错误。
		return ctx.Response().Json(409, map[string]any{
			"error": "创建用户失败：用户名或邮箱已被占用",
		})
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

// userPayload 是登录与个人资料接口返回的用户信息。
//
// 集中成一个函数是因为三处（登录、profile、更新）必须返回完全一致的字段，
// 之前是各写各的 map，很容易漏掉一个字段导致前端拿到 undefined。
func userPayload(user models.User) map[string]any {
	return map[string]any{
		"id":           user.ID,
		"username":     user.Username,
		"display_name": user.DisplayName,
		"role":         user.Role,
		"role_label":   models.Role(user.Role).Label(),
		"email":        user.Email,
		// 为空表示未设置邮箱，前端据此回退到首字母圆圈而不是加载图片。
		"avatar_url": user.AvatarURL(),
	}
}

func (c *AuthController) Profile(ctx http.Context) http.Response {
	user, err := selfUser(ctx)
	if err != nil {
		return ctx.Response().Json(404, map[string]any{"error": err.Error()})
	}
	return ctx.Response().Json(200, userPayload(user))
}

// selfUser 取出当前登录用户。
func selfUser(ctx http.Context) (models.User, error) {
	userID := ctx.Value("user_id")

	// ID == 0 的判断不能省：First 查不到时不报错，见 jwt.go 里的说明。
	var user models.User
	if err := facades.Orm().Query().Where("id = ?", userID).First(&user); err != nil || user.ID == 0 {
		return models.User{}, errors.New("用户不存在")
	}
	return user, nil
}

// UpdateProfile 修改自己的显示名与邮箱。
//
// 挂在 Jwt() 组内、不带角色门槛——任何人只能改自己，这是个人资料而不是管理
// 操作。真正的判定是「只操作 ctx 里的那个 id」，不存在越权空间。
func (c *AuthController) UpdateProfile(ctx http.Context) http.Response {
	user, err := selfUser(ctx)
	if err != nil {
		return ctx.Response().Json(404, map[string]any{"error": err.Error()})
	}

	// 语义要区分「传了空串」（= 清空）与「没传」（= 不动），
	// 而 ContextRequest 没有 Has()，只能用 All() 判键是否存在。
	// 前端永远把两个字段一起发上来。
	input := ctx.Request().All()
	updates := map[string]any{}

	if _, hasName := input["display_name"]; hasName {
		name := strings.TrimSpace(ctx.Request().Input("display_name", ""))
		if len(name) > 100 {
			return ctx.Response().Json(400, map[string]any{"error": "显示名不能超过 100 个字符"})
		}
		updates["display_name"] = name
	}

	if _, hasEmail := input["email"]; hasEmail {
		email := models.NormalizeEmail(ctx.Request().Input("email", ""))
		if email != "" && !models.ValidEmail(email) {
			return ctx.Response().Json(400, map[string]any{
				"error": "邮箱格式不正确",
			})
		}

		if email != user.Email {
			// 先查再改，是为了让错误信息可读；唯一索引仍然兜底，
			// 防止两个请求并发时都通过检查。
			taken, err := facades.Orm().Query().Model(&models.User{}).
				Where("email = ?", email).Count()
			if err != nil {
				return ctx.Response().Json(500, map[string]any{"error": "查询邮箱失败"})
			}
			if taken > 0 {
				return ctx.Response().Json(409, map[string]any{
					"error": "该邮箱已被其他账号使用",
				})
			}
		}
		updates["email"] = email
	}

	if len(updates) == 0 {
		return ctx.Response().Json(400, map[string]any{"error": "没有需要更新的内容"})
	}

	if _, err := facades.Orm().Query().Model(&models.User{}).
		Where("id = ?", user.ID).Update(updates); err != nil {
		// 唯一索引冲突也走到这里（并发写入）。
		log.Printf("[AUTH] 更新个人资料失败 user=%d: %v", user.ID, err)
		return ctx.Response().Json(409, map[string]any{"error": "该邮箱已被其他账号使用"})
	}

	// 回读而不是拿内存里的 user 拼：邮箱可能被唯一索引拒绝、或者
	// AvatarURL 依赖的是归一化后的实际值。
	updated, err := selfUser(ctx)
	if err != nil {
		return ctx.Response().Json(500, map[string]any{"error": "更新后读取失败"})
	}

	log.Printf("[AUTH] 用户 %s(#%d) 更新了个人资料", user.Username, user.ID)

	// 改个人资料本身不是管理操作，但它是「谁在什么时候把邮箱换成了什么」
	// 的唯一线索——邮箱又是头像取值依据与账号找回凭据，值得留痕。
	recordAudit(ctx, user, audit.Record{
		Action:     "user.profile_update",
		Summary:    "更新个人资料",
		TargetType: "user",
		TargetID:   strconv.FormatUint(uint64(user.ID), 10),
		// 只记改了哪些字段名，不记具体值：邮箱属于个人信息，审计表会被导出。
		Detail: map[string]any{"fields": changedFields(updates)},
	})

	return ctx.Response().Json(200, userPayload(updated))
}

// changedFields 只取更新字段的键名，用于审计。
func changedFields(updates map[string]any) []string {
	fields := make([]string, 0, len(updates))
	for key := range updates {
		fields = append(fields, key)
	}
	// 排序让同样的改动顺序一致，便于比对两条记录。
	sort.Strings(fields)
	return fields
}

// ChangePassword 修改自己的密码。
//
// 改完立即让所有旧令牌失效：token_version 递增，而令牌里带的是签发时的版本号。
// 三处验签点（Jwt 中间件、resolveActor、WebSocket）都比对它。
//
// 同时返回新令牌，否则当前设备会被自己刚改的密码踢下线。
func (c *AuthController) ChangePassword(ctx http.Context) http.Response {
	user, err := selfUser(ctx)
	if err != nil {
		return ctx.Response().Json(404, map[string]any{"error": err.Error()})
	}

	current := ctx.Request().Input("current_password", "")
	next := ctx.Request().Input("new_password", "")

	// 当前密码错误必须返回 400 而不是 401：前端的统一处理会在收到 401 时
	// 清掉登录态并跳回登录页——用户改密码时手滑输错一次就被登出，体验上
	// 等于「改密码功能把账号锁了」。
	if current == "" || !facades.Hash().Check(current, user.Password) {
		return ctx.Response().Json(400, map[string]any{"error": "当前密码不正确"})
	}
	if len(next) < minPasswordLength {
		return ctx.Response().Json(400, map[string]any{
			"error": fmt.Sprintf("新密码至少 %d 位", minPasswordLength),
		})
	}
	if next == current {
		return ctx.Response().Json(400, map[string]any{"error": "新密码不能与当前密码相同"})
	}

	hashed, err := facades.Hash().Make(next)
	if err != nil {
		log.Printf("[AUTH] 密码加密失败: %v", err)
		return ctx.Response().Json(500, map[string]any{"error": "密码加密失败"})
	}

	// 一次写入同时更新密码哈希与令牌版本：两者必须同时生效，
	// 分两次写的话中间失败会留下「新密码 + 旧版本」的组合。
	result, err := facades.Orm().Query().Model(&models.User{}).
		Where("id = ?", user.ID).Update(map[string]any{
		"password":      hashed,
		"token_version": user.TokenVersion + 1,
	})
	if err != nil {
		log.Printf("[AUTH] 修改密码失败 user=%d: %v", user.ID, err)
		return ctx.Response().Json(500, map[string]any{"error": "修改密码失败"})
	}
	if result.RowsAffected == 0 {
		return ctx.Response().Json(404, map[string]any{"error": "用户不存在"})
	}

	// 用递增后的版本号签发，否则新令牌会立刻因为版本不匹配而失效。
	fresh := user
	fresh.TokenVersion = user.TokenVersion + 1

	token, err := generateToken(fresh)
	if err != nil {
		log.Printf("[AUTH] 生成新令牌失败: %v", err)
		return ctx.Response().Json(500, map[string]any{"error": "生成新令牌失败"})
	}

	log.Printf("[AUTH] 用户 %s(#%d) 修改了密码，所有旧令牌已失效", user.Username, user.ID)

	return ctx.Response().Json(200, map[string]any{
		"token": token,
		"user":  userPayload(fresh),
	})
}
