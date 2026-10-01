package controllers

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/goravel/framework/contracts/http"
	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
	"smart-mzcmc/app/setup"
)

// SetupController 提供「全新部署初始化向导」的两个接口。
//
// 这两个接口是公开的（向导页此时还没有任何账号可登录）。写操作 Apply
// 自带「已初始化就 403」的守卫，读操作 Status 则在已初始化后整体停发部署细节
// （理由见 Status 的注释）——只靠「Apply 会拒绝」来推断「Status 也不该多说」
// 是错的：Status 在初始化之后依然可以被任何人匿名调用。
//
// 这和 POST /api/auth/register 的「引导模式」是同一类风险面，不额外放大：
// 在第一个账号建好之前，能访问到端口的人本来就可以抢先注册管理员。
type SetupController struct{}

func NewSetupController() *SetupController {
	return &SetupController{}
}

// 向导会写入 .env 的键名。
const (
	envAppName      = "APP_NAME"
	envAppURL       = "APP_URL"
	envAppHost      = "APP_HOST"
	envAppPort      = "APP_PORT"
	envDBConnection = "DB_CONNECTION"
)

// Status 返回初始化向导需要的全部上下文。
//
// ⚠️ 这个接口是**公开**的（全新部署时系统里一个账号都没有，向导无从登录），
//
//	所以「公开」不能等于「一直公开同样的内容」。
//
// 下面这些字段——.env 与数据库的**绝对路径**、内网 IP、JWT_SECRET/APP_KEY
// 是否已配置、是不是全新装的——对填表前的运维有用，对已初始化的系统毫无价值，
// 却正好是一份匿名可读的部署情报：谁都能知道这台机器的文件怎么摆、密钥配了没有。
// 因此 users 表里一有账号就整体停发，只保留判断「还需不需要初始化」所需的两个字段。
// 前端拿到 needs_setup=false 就渲染「系统已完成初始化」，不依赖任何细节字段。
func (c *SetupController) Status(ctx http.Context) http.Response {
	if !setup.NeedsSetup() {
		return ctx.Response().Json(200, map[string]any{
			"needs_setup": false,
			"version":     Version,
		})
	}

	cfg := facades.Config()

	port := strings.TrimSpace(cfg.GetString("http.port", "3000"))
	if port == "" {
		port = "3000"
	}
	host := strings.TrimSpace(cfg.GetString("http.host", "127.0.0.1"))
	if host == "" {
		host = "127.0.0.1"
	}
	url := strings.TrimSpace(cfg.GetString("http.url", ""))
	if url == "" || url == "http://localhost" {
		// 局域网部署的默认值应该是内网地址，不是 localhost ——
		// 导播端、解说端、采访端都要从别的机器访问这个地址。
		url = "http://" + setupHostForURL(host) + ":" + port
	}

	return ctx.Response().Json(200, map[string]any{
		"needs_setup": setup.NeedsSetup(),
		"version":     Version,
		"lan_ip":      setup.LANIP(),
		"database": map[string]any{
			"connection": strings.TrimSpace(cfg.GetString("database.default", "sqlite")),
			"path":       setup.DatabasePath(),
			"exists":     setup.DatabaseFileExists(),
			// 进程启动时数据库文件还不存在 —— 这才是「全新部署」的判据。
			// 单独暴露出来是因为 exists 会被框架自身打开连接后变成 true
			// （SQLite 连一下就建出空文件），直接拿它当「是否全新」会看错。
			"missing_at_startup": !setup.DatabaseAtStartupExisted(),
		},
		"env": map[string]any{
			"path":     setup.EnvPath(),
			"abs_path": setup.EnvAbsolutePath(),
			"exists":   setup.EnvExists(),
			"writable": setup.EnvWritable(),
		},
		"secrets": map[string]any{
			"app_key":    setup.SecretConfigured("APP_KEY"),
			"jwt_secret": setup.SecretConfigured("JWT_SECRET"),
		},
		"defaults": map[string]any{
			"app_name":           setup.SuggestAppName(cfg.GetString("app.name", "")),
			"app_url":            url,
			"app_host":           host,
			"app_port":           port,
			"admin_username":     "admin",
			"admin_display_name": "系统管理员",
		},
	})
}

// setupHostForURL 把监听地址换算成 URL 里能用的主机名。
//
// 0.0.0.0 / :: 是「监听全部网卡」，不能直接塞进 URL——浏览器打开
// http://0.0.0.0:3000 在部分系统上会失败。这种情况用探测到的内网 IP 代替。
func setupHostForURL(host string) string {
	switch host {
	case "0.0.0.0", "::", "[::]", "":
		if ip := setup.LANIP(); ip != "" {
			return ip
		}
		return "127.0.0.1"
	default:
		return host
	}
}

// 向导提交的字段。
type setupRequest struct {
	AppName     string
	AppURL      string
	AppHost     string
	AppPort     string
	Username    string
	Password    string
	DisplayName string
	Email       string
}

// Apply 执行初始化：写 .env → 跑迁移 → 建管理员。
//
// 顺序不能换。迁移必须在建账号之前跑完（users 表还不存在），而 .env 必须先
// 写好，否则新生成的 JWT_SECRET 只存在于内存里，进程一重启就换了密钥。
func (c *SetupController) Apply(ctx http.Context) http.Response {
	if !setup.NeedsSetup() {
		return ctx.Response().Json(403, map[string]any{
			"error": "系统已完成初始化。如需重新初始化，请停止服务、备份并删除数据库文件后重启。",
		})
	}

	req, resp := c.parseRequest(ctx)
	if resp != nil {
		return resp
	}

	cfg := facades.Config()
	prevHost := strings.TrimSpace(cfg.GetString("http.host", ""))
	prevPort := strings.TrimSpace(cfg.GetString("http.port", ""))
	prevURL := strings.TrimSpace(cfg.GetString("http.url", ""))

	written, err := setup.WriteAppConfig([]setup.EnvValue{
		{Key: envAppName, Value: req.AppName},
		{Key: envAppURL, Value: req.AppURL},
		{Key: envAppHost, Value: req.AppHost},
		{Key: envAppPort, Value: req.AppPort},
		{Key: envDBConnection, Value: "sqlite"},
	})
	if err != nil {
		log.Printf("[初始化] 写入 .env 失败: %v", err)
		return ctx.Response().Json(500, map[string]any{
			"error": "写入 .env 失败：" + err.Error(),
		})
	}

	// 本次进程不会再读一次 .env，所以把关键项同步进运行时配置。
	//
	// 不做这一步，向导完成后「立刻登录」会失败在 JWT_SECRET 上：新生成的
	// 密钥写进了文件，但当前进程内存里还是空串（generateToken 会直接报
	// 「jwt.secret 未配置」）。
	cfg.Add("app.name", req.AppName)
	cfg.Add("http.url", req.AppURL)
	cfg.Add("http.host", req.AppHost)
	cfg.Add("http.port", req.AppPort)
	if secret := setup.ReadEnvValue("JWT_SECRET"); secret != "" {
		cfg.Add("jwt.secret", secret)
	}
	if key := setup.ReadEnvValue("APP_KEY"); key != "" {
		cfg.Add("app.key", key)
	}

	if err := setup.RunMigrations(); err != nil {
		log.Printf("[初始化] 数据库迁移失败: %v", err)
		return ctx.Response().Json(500, map[string]any{
			"error": "数据库初始化失败：" + err.Error(),
		})
	}

	user, err := createBootstrapAdmin(req.Username, req.Password, req.DisplayName, req.Email)
	if err != nil {
		log.Printf("[初始化] 创建管理员失败: %v", err)
		return ctx.Response().Json(500, map[string]any{
			"error": "创建管理员账号失败：" + err.Error(),
		})
	}

	setup.Complete()

	// .env 只在进程启动时读一次，所以凡是改了启动期配置的都要提示重启。
	// 但提示语要分两种情况，否则「只改了访问地址」也会被说成「监听地址变了」——
	// 一句不准确的提示会让运维去找一个根本不存在的问题。
	listenChanged := prevHost != req.AppHost || prevPort != req.AppPort
	restartRequired := listenChanged || prevURL != req.AppURL

	notes := []string{".env 已更新，管理员账号已创建，可以登录了。"}
	switch {
	case listenChanged:
		notes = append(notes, fmt.Sprintf(
			"监听地址/端口已在 .env 中改为 %s:%s，重启后端后生效（当前进程仍按 %s:%s 监听）。",
			req.AppHost, req.AppPort, prevHost, prevPort))
	case restartRequired:
		notes = append(notes, "系统名称与对外地址已写入 .env，重启后端后在其他地方一并生效。")
	}
	notes = append(notes, "数据库文件："+setup.DatabasePath()+"（SQLite，随发布包目录一起备份即可）")

	log.Printf("[初始化] 完成：系统名称=%q 访问地址=%s 管理员=%s(#%d)",
		req.AppName, req.AppURL, user.Username, user.ID)

	return ctx.Response().Json(201, map[string]any{
		"message":          "初始化完成",
		"admin":            userPayload(user),
		"env_written":      sortedKeys(written),
		"env_path":         setup.EnvPath(),
		"restart_required": restartRequired,
		"app_url":          req.AppURL,
		"ws_url":           setupWsURL(req.AppURL),
		"login_url":        "/admin/login",
		"notes":            notes,
	})
}

// parseRequest 校验向导提交的字段。校验失败时返回 (nil-ish, 响应)。
func (c *SetupController) parseRequest(ctx http.Context) (setupRequest, http.Response) {
	req := setupRequest{
		AppName:     strings.TrimSpace(ctx.Request().Input("app_name", "")),
		AppURL:      strings.TrimSpace(ctx.Request().Input("app_url", "")),
		AppHost:     strings.TrimSpace(ctx.Request().Input("app_host", "")),
		AppPort:     strings.TrimSpace(ctx.Request().Input("app_port", "")),
		Username:    strings.TrimSpace(ctx.Request().Input("admin_username", "")),
		Password:    ctx.Request().Input("admin_password", ""),
		DisplayName: strings.TrimSpace(ctx.Request().Input("admin_display_name", "")),
		Email:       models.NormalizeEmail(ctx.Request().Input("admin_email", "")),
	}

	bad := func(message string) (setupRequest, http.Response) {
		return setupRequest{}, ctx.Response().Json(400, map[string]any{"error": message})
	}

	if req.AppName == "" {
		req.AppName = setup.DefaultAppName
	}
	if len([]rune(req.AppName)) > 60 {
		return bad("系统名称不能超过 60 个字符")
	}

	if !setup.EnvWritable() {
		return bad("当前进程无法写入 " + setup.EnvAbsolutePath() + "，请检查文件权限后重试")
	}

	// 监听地址与端口：留空表示沿用 .env 里的现值。
	cfg := facades.Config()
	if req.AppHost == "" {
		req.AppHost = strings.TrimSpace(cfg.GetString("http.host", "0.0.0.0"))
	}
	if strings.ContainsAny(req.AppHost, "/\\ :\t") {
		return bad("监听地址只能是 IP 或主机名，不能带协议、端口或空格")
	}

	if req.AppPort == "" {
		req.AppPort = strings.TrimSpace(cfg.GetString("http.port", "3000"))
	}
	port, err := strconv.Atoi(req.AppPort)
	if err != nil || port < 1 || port > 65535 {
		return bad("端口必须是 1-65535 之间的整数")
	}
	req.AppPort = strconv.Itoa(port)

	if req.AppURL == "" {
		req.AppURL = "http://" + setupHostForURL(req.AppHost) + ":" + req.AppPort
	}
	if !strings.HasPrefix(req.AppURL, "http://") && !strings.HasPrefix(req.AppURL, "https://") {
		return bad("访问地址必须以 http:// 或 https:// 开头")
	}
	req.AppURL = strings.TrimRight(req.AppURL, "/")

	if req.Username == "" {
		req.Username = "admin"
	}
	if !validUsername(req.Username) {
		return bad("用户名只能包含字母、数字、下划线、点、短横线与中文，且不超过 64 个字符")
	}
	if len(req.Password) < minPasswordLength {
		return bad(fmt.Sprintf("密码至少 %d 位", minPasswordLength))
	}
	if req.DisplayName == "" {
		req.DisplayName = "系统管理员"
	}
	if len([]rune(req.DisplayName)) > 100 {
		return bad("显示名不能超过 100 个字符")
	}
	if req.Email != "" && !models.ValidEmail(req.Email) {
		return bad("邮箱格式不正确")
	}

	return req, nil
}

// createBootstrapAdmin 建立第一个账号，固定为超级管理员。
//
// 必须是超级管理员而不是管理员：只有超管能授予超管角色（见 guardGrant），
// 若这里建成管理员，系统会停在一个没人能管理系统更新的状态。
func createBootstrapAdmin(username, password, displayName, email string) (models.User, error) {
	count, err := facades.Orm().Query().Model(&models.User{}).Count()
	if err != nil {
		return models.User{}, fmt.Errorf("查询用户表失败: %w", err)
	}
	if count > 0 {
		return models.User{}, fmt.Errorf("系统已存在账号，初始化流程已中止")
	}

	hashed, err := facades.Hash().Make(password)
	if err != nil {
		return models.User{}, fmt.Errorf("密码加密失败: %w", err)
	}

	user := models.User{
		Username:    username,
		Password:    hashed,
		DisplayName: displayName,
		Role:        string(models.RoleSuperAdmin),
		Email:       email,
	}
	if err := facades.Orm().Query().Create(&user); err != nil {
		return models.User{}, err
	}
	return user, nil
}

// sortedKeys 把写入结果整理成稳定的键名列表，便于界面展示与日志比对。
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// setupWsURL 由 HTTP 地址推出 WebSocket 地址，给客户端配置提供现成的值。
func setupWsURL(appURL string) string {
	switch {
	case strings.HasPrefix(appURL, "https://"):
		return "wss://" + strings.TrimPrefix(appURL, "https://") + "/ws"
	case strings.HasPrefix(appURL, "http://"):
		return "ws://" + strings.TrimPrefix(appURL, "http://") + "/ws"
	default:
		return ""
	}
}
