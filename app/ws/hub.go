package ws

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/goravel/framework/facades"
	"github.com/gorilla/websocket"

	"smart-mzcmc/app/models"
	"smart-mzcmc/app/plugins"
)

type WSMessage struct {
	Type      string `json:"type"`
	ProjectID uint   `json:"project_id"`
	SenderID  uint   `json:"sender_id,omitempty"`
	// SenderName / SenderRole 由服务端在广播前盖上，不是客户端能填的字段。
	//
	// 为什么必须服务端盖：发送者身份要能区分「谁在说话」，而聊天里出现的
	// 人不止导播（解说、包装、采访都能发言）。此前广播里只有 sender_id，
	// 各端拿不到用户名，于是导播端把每条外来消息一律标成「其他」——
	// 多端同场时等于没法沟通。
	//
	// 为什么不能从 payload 里取：payload 是客户端原样上传的内容，
	// 采信它就等于允许任何人自称导播。
	SenderName string          `json:"sender_name,omitempty"`
	SenderRole string          `json:"sender_role,omitempty"`
	Payload    json.RawMessage `json:"payload"`
	Timestamp  int64           `json:"timestamp"`
}

// ShotStatePayload 切台状态载荷。
//
// 导播端每次切台都上报一份完整状态，而不是分两次发「预告」和「已切」：
//   - Current 当前正在播送的机位
//   - Next    本次要切过去的机位（切过去之后即成为新的 Current）
//
// 接收端不再需要自己推断「正在播送」，直接读 Current 即可。
//
// 注意：广播本身只发生在新状态到来的那一刻，所以「后加入的解说端能立刻拿到
// 状态」曾经是**假的**——中途连上来的人会一直停在「等待导播指令」，重连
// 同理。真正让这句话成立的是 project_states 表：握手时的欢迎消息会把
// WelcomePayload 里的 current_shot 一并带上，见 buildWelcome。
type ShotStatePayload struct {
	Current string `json:"current"`
	Next    string `json:"next"`
}

// WelcomePayload 是连接建立后第一条 system 消息的载荷。
//
// 带上当前切台状态，是为了让「中途连接」与「断线重连」这两种情况不用等
// 下一次切台就能显示正确内容。StateAvailable 为 false 表示这个项目还没有
// 过任何切台，客户端应保持「等待导播指令」，而不是显示空机位名。
type WelcomePayload struct {
	Message        string `json:"message"`
	OnlineCount    int    `json:"online_count"`
	CurrentShot    string `json:"current_shot"`
	NextShot       string `json:"next_shot"`
	StateAvailable bool   `json:"state_available"`
}

// IsHeartbeat 判断消息是否只是保活心跳。
//
// 心跳由客户端每 10 秒发一次，属于「不算数」的消息：既不写入 messages 表，
// 也不广播给同项目的其他端，因此不会污染日志、不会计入项目消息统计。
//
// 心跳只用来刷新客户端的 LastSeen（见 Client.touch），后台扫描据此判断
// 采访端是否已经掉线。
func IsHeartbeat(msg WSMessage) bool {
	if msg.Type != "chat" {
		return false
	}
	var payload map[string]any
	if json.Unmarshal(msg.Payload, &payload) != nil {
		return false
	}
	return payload["message"] == "heartbeat"
}

// sendSystemError 回一条只发给当前客户端的 system 消息。
func sendSystemError(c *Client, text string) {
	errMsg := WSMessage{
		Type:      "system",
		ProjectID: c.ProjectID,
		Payload:   json.RawMessage(`{"error":` + strconv.Quote(text) + `}`),
		Timestamp: time.Now().UnixMilli(),
	}
	if data, err := json.Marshal(errMsg); err == nil {
		select {
		case c.Send <- data:
		default:
		}
	}
}

type Client struct {
	ID        string
	Conn      *websocket.Conn
	ProjectID uint
	UserID    uint
	Role      string
	// DisplayName 是握手时从令牌解析出来的账号名，广播时用作 sender_name。
	// 匿名连接（未带令牌且成员校验关闭）时为空，接收端会退回按角色显示。
	DisplayName string
	// PointCode 只在采访端连接上非空（来自查询参数）。
	// 没有它就无法回答「掉线的是哪个采访点」，后台扫描也就无从下手。
	PointCode string
	Hub       *Hub
	Send      chan []byte
	mu        sync.Mutex
	// LastSeen 是最后一次收到该客户端任何消息的时刻。
	//
	// 采访端走出 WiFi 覆盖范围时 TCP 要等很久才报错，readPump 期间连接
	// 仍然是「已注册」状态。靠这个字段做超时判定，才能在不依赖 TCP 超时的
	// 前提下把项目里的采访点标成离线。
	LastSeen time.Time
}

// touch 刷新 LastSeen。读消息与扫描在两个 goroutine 上，必须加锁。
func (c *Client) touch() {
	c.mu.Lock()
	c.LastSeen = time.Now()
	c.mu.Unlock()
}

// senderDisplayName 决定广播里显示的发送者名。
//
// 优先级：账号昵称 → 账号用户名 → 采访点名 → 角色名。
// 落到角色名是必要的退路：关闭项目成员校验时采访端是匿名连接，
// 昵称与用户名都取不到，若此时也返回空串，聊天里就会出现一排无名消息，
// 现场根本分不清哪条是本机发的、哪条是别的采访点发的。
func senderDisplayName(account models.User, hasAccount bool, role, pointCode string) string {
	if hasAccount {
		if account.DisplayName != "" {
			return account.DisplayName
		}
		if account.Username != "" {
			return account.Username
		}
	}
	if role == "interviewer" && pointCode != "" {
		return pointCode
	}
	return roleLabel(role)
}

// roleLabel 把 WS 角色标识翻成人能读的名字。
//
// 显示层不做这层翻译：它得在每个客户端各写一份，而角色词表是后端的概念，
// 客户端写死一份迟早会漏掉新增的角色。
func roleLabel(role string) string {
	switch role {
	case "director":
		return "导播"
	case "commentator":
		return "解说"
	case "packaging":
		return "包装"
	case "interviewer":
		return "采访"
	case "admin":
		return "管理员"
	case "super_admin":
		return "超级管理员"
	default:
		return "未知"
	}
}

// lastSeen 取 LastSeen 的快照。
func (c *Client) lastSeen() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.LastSeen
}

type Hub struct {
	clients    map[*Client]bool
	byProject  map[uint]map[*Client]bool
	register   chan *Client
	unregister chan *Client
	broadcast  chan []byte
	mu         sync.RWMutex
}

var DefaultHub *Hub

func init() {
	DefaultHub = NewHub()
	go DefaultHub.Run()
}

func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		byProject:  make(map[uint]map[*Client]bool),
		register:   make(chan *Client, 256),
		unregister: make(chan *Client, 256),
		broadcast:  make(chan []byte, 256),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			if h.byProject[client.ProjectID] == nil {
				h.byProject[client.ProjectID] = make(map[*Client]bool)
			}
			h.byProject[client.ProjectID][client] = true
			h.mu.Unlock()
			log.Printf("[WS] 客户端注册: project=%d role=%s user=%d online_in_project=%d", client.ProjectID, client.Role, client.UserID, h.GetProjectOnlineCount(client.ProjectID))

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				if projectClients, exists := h.byProject[client.ProjectID]; exists {
					delete(projectClients, client)
				}
				close(client.Send)
			}
			h.mu.Unlock()
			log.Printf("[WS] 客户端断开: %s", client.ID)

			// 导播断线时释放控制权
			if client.Role == "director" {
				releaseLockOnDisconnect(client.ProjectID, client.UserID, client)
				plugins.Emit(plugins.Event{
					Type:      "director_disconnect",
					ProjectID: client.ProjectID,
					UserID:    client.UserID,
				})
			}

			// 采访端断开时立刻标离线。
			//
			// 这是最快的一条路径：客户端掉线的一瞬间就能让导播端变红，而
			// 不必等 60 秒的后台扫描。扫描仍然保留，用于「TCP 还没断但人已经
			// 走出覆盖范围」那种连接看似健在的情况。
			if client.Role == "interviewer" && client.PointCode != "" {
				markInterviewPointOffline(client.ProjectID, client.PointCode, "disconnect")
			}

		case message := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.Send <- message:
				default:
				}
			}
			h.mu.RUnlock()
		}
	}
}

// 连接速率限制：同一标识 1 秒内不允许重复连接。
//
// 之前这是个裸的包级 map，每个 HTTP 处理 goroutine 都在直接读写它。
// 多端同时重连（现场断一次电就是全部客户端一起冲）会让两个 goroutine
// 并发写同一个 map，而 Go 的并发写 map 是 **fatal error**：不是 panic，
// recover 拦不住，整个进程直接消失。现场表现是播到一半所有端同时断线、
// 服务端进程没了，且日志里只有一行 maps.fatal，没有任何业务上下文。
//
// 键是 (project_id, role, point_code) 组合，所以还得顺手回收：项目一多、
// 采访点一多，这个常驻 map 只会单调增长，而它没有任何人负责清理。
var connectionRateLimit = struct {
	mu   sync.Mutex
	seen map[string]time.Time
}{
	seen: make(map[string]time.Time),
}

// allowConnection 决定这次连接是否放行，放行时记下当前时刻。
//
// 检查与写入必须在同一把锁里完成：分开锁的话两个并发重连会同时通过检查，
// 限流等于没写。
func allowConnection(key string) bool {
	now := time.Now()

	connectionRateLimit.mu.Lock()
	defer connectionRateLimit.mu.Unlock()

	// 顺带清掉过期项，保证 map 里只留最近一秒出现过的标识。
	for k, at := range connectionRateLimit.seen {
		if now.Sub(at) >= time.Second {
			delete(connectionRateLimit.seen, k)
		}
	}

	if last, ok := connectionRateLimit.seen[key]; ok && now.Sub(last) < time.Second {
		return false
	}
	connectionRateLimit.seen[key] = now
	return true
}

// isPrivilegedRole 判断连接角色是否属于必须持令牌的那几个。
func isPrivilegedRole(role string) bool {
	return role == "director" || role == "admin" || role == "super_admin"
}

// authenticateWS 解析并校验 WebSocket 携带的令牌。
//
// 与 HTTP 侧同一套规则：验签算法必须是 HMAC、必须查库确认用户还在、
// 必须比对 token_version（改密码后旧令牌立即失效）。任何一条漏掉，
// 「改密码即失效」在直播链路上就是漏的。
func authenticateWS(token string) (models.User, error) {
	if token == "" {
		return models.User{}, fmt.Errorf("需要认证")
	}
	secret := facades.Config().GetString("jwt.secret")
	if secret == "" {
		return models.User{}, fmt.Errorf("JWT密钥未配置")
	}
	parsedToken, err := jwt.Parse(token, func(t *jwt.Token) (interface{}, error) {
		// 必须检查算法。缺这一步的话，把 alg 改成 none 或非 HMAC 的令牌
		// 可能被接受——另外两处验签点（中间件、resolveActor）都有这个检查。
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil || !parsedToken.Valid {
		return models.User{}, fmt.Errorf("令牌无效")
	}
	claims, ok := parsedToken.Claims.(jwt.MapClaims)
	if !ok {
		return models.User{}, fmt.Errorf("令牌解析失败")
	}
	key, _ := claims["key"].(string)
	uid, _ := strconv.ParseUint(key, 10, 64)
	if uid == 0 {
		return models.User{}, fmt.Errorf("无效的用户ID")
	}

	// ID == 0 的判断不能省：First 查不到时不报错，只把结构体留成零值。
	var user models.User
	if err := facades.Orm().Query().Select("id", "role", "token_version").
		Where("id = ?", uid).First(&user); err != nil || user.ID == 0 {
		return models.User{}, fmt.Errorf("用户不存在或已被删除")
	}
	claimVersion, _ := claims["ver"].(float64)
	if int(claimVersion) != user.TokenVersion {
		return models.User{}, fmt.Errorf("登录状态已失效，请重新登录")
	}
	return user, nil
}

// RequireProjectMembership 报告是否开启了项目成员校验。
//
// 默认关闭：用户表与项目授权关系在存量部署里可能还没配好，一上线就强制
// 校验会让现场直接连不上。关闭时只记录不拦截（见 logMembershipMiss）。
func RequireProjectMembership() bool {
	return facades.Config().GetBool("authz.require_project_membership", false)
}

// IsProjectMember 判断用户是否被授权访问某个项目。
//
// 实现在 models 里，HTTP 侧的中间件用同一份——两处各写一遍 Count 查询迟早
// 会出现「REST 放行、WS 拦下」这种只在一半路径上生效的漏洞。
func IsProjectMember(userID, projectID uint) bool {
	return models.IsProjectMember(userID, projectID)
}

func HandleWebSocket(hub *Hub, w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.Atoi(r.URL.Query().Get("project_id"))
	role := r.URL.Query().Get("role")
	token := r.URL.Query().Get("token")
	userIDStr := r.URL.Query().Get("user_id")
	pointCode := r.URL.Query().Get("point_code")

	// 速率限制：同一(project_id, role, point_code) 1秒内不重复
	rateKey := strconv.Itoa(projectID) + ":" + role + ":" + pointCode
	if !allowConnection(rateKey) {
		http.Error(w, `{"error":"连接过于频繁"}`, http.StatusTooManyRequests)
		return
	}

	var userID uint
	if userIDStr != "" {
		uid, _ := strconv.Atoi(userIDStr)
		userID = uint(uid)
	}

	privileged := isPrivilegedRole(role)
	requireMembership := RequireProjectMembership()

	var account models.User
	hasAccount := false

	// 令牌校验。导播/管理员一直要令牌；开启成员校验后所有角色都要。
	// 二者都不满足但客户端仍然带了令牌时也验一次——验过就能识别身份，
	// 对不上则只记日志放行，保持「关闭开关时不打断现有部署」的语义。
	if privileged || requireMembership || token != "" {
		user, err := authenticateWS(token)
		if err != nil {
			if privileged || requireMembership {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusUnauthorized)
				return
			}
			log.Printf("[WS] role=%s 的令牌校验失败，项目成员校验未开启，按匿名连接放行: %v", role, err)
		} else {
			account = user
			hasAccount = true
			userID = user.ID
		}
	}

	if projectID == 0 {
		http.Error(w, `{"error":"需要 project_id 参数"}`, http.StatusBadRequest)
		return
	}

	// 项目成员校验。
	//
	// 管理员及以上绕过：他们本来就要管理所有项目。其余角色（含导播）在严格
	// 模式下必须是该项目的成员，否则 WS 就是一个绕过 REST 鉴权的后门——
	// 只要知道 project_id 就能收到该项目的全部实时消息。
	if requireMembership {
		if !hasAccount {
			http.Error(w, `{"error":"该项目需要登录后访问"}`, http.StatusUnauthorized)
			return
		}
		if !models.Role(account.Role).AtLeast(models.RoleAdmin) &&
			!IsProjectMember(userID, uint(projectID)) {
			log.Printf("[WS] 用户 %d(%s) 不是项目 %d 的成员，拒绝连接", userID, account.Role, projectID)
			http.Error(w, `{"error":"无权访问该项目"}`, http.StatusForbidden)
			return
		}
	} else if !hasAccount && role != "interviewer" {
		log.Printf("[WS] role=%s 未识别身份即连接 project=%d（项目成员校验未开启，仅记录）", role, projectID)
	}

	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS] 升级连接失败: %v", err)
		return
	}

	client := &Client{
		ID:          fmt.Sprintf("client-%d", time.Now().UnixNano()),
		Conn:        conn,
		ProjectID:   uint(projectID),
		UserID:      userID,
		Role:        role,
		DisplayName: senderDisplayName(account, hasAccount, role, pointCode),
		PointCode:   pointCode,
		Hub:         hub,
		Send:        make(chan []byte, 256),
		LastSeen:    time.Now(),
	}

	// 收到 pong 就刷新 LastSeen，掉线判定因此不再依赖应用层心跳。
	//
	// 采访端跑在浏览器里，后台标签页的 Timer.periodic 会被节流到分钟级，
	// 于是 10 秒一次的应用层心跳实际上几分钟才发一条，而服务端 90 秒就判超时——
	// 表现是导播端看到采访端「自己掉线了」，人其实一直好好开着页面。
	//
	// 协议层的 ping/pong 由浏览器网络栈应答，不经过 JS，也不受定时器节流影响，
	// 所以它才是这类场景下唯一可靠的存活信号。writePump 每 30 秒发一次 ping。
	conn.SetPongHandler(func(string) error {
		client.touch()
		return nil
	})

	hub.register <- client

	data, _ := json.Marshal(buildWelcome(hub, uint(projectID)))
	client.Send <- data

	// 导播连接时：如果没有锁，自动获取控制权
	if role == "director" {
		autoAcquireLock(uint(projectID), userID, client)
	}

	go client.writePump()
	go client.readPump()
}

// buildWelcome 拼出连接建立后的第一条 system 消息。
//
// 带上当前切台状态是这次改动的重点：客户端中途连上或断线重连时，
// 不必等下一次切台就能显示正确的「正在播送 / 即将播送」，
// 而不是停在「等待导播指令」。
func buildWelcome(hub *Hub, projectID uint) WSMessage {
	payload := WelcomePayload{
		Message:     "连接成功",
		OnlineCount: hub.GetProjectOnlineCount(projectID),
	}

	var state models.ProjectState
	if err := facades.Orm().Query().Where("project_id = ?", projectID).
		First(&state); err == nil && state.ProjectID != 0 {
		payload.CurrentShot = state.CurrentShot
		payload.NextShot = state.NextShot
		// 只有真的有过切台才置位，否则客户端会把空机位名当成有效状态渲染。
		payload.StateAvailable = state.CurrentShot != "" || state.NextShot != ""
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		// 序列化不可能失败（全是基本类型），真失败也不能拦下整条连接。
		raw = json.RawMessage(`{"message":"连接成功"}`)
	}

	return WSMessage{
		Type:      "system",
		ProjectID: projectID,
		Payload:   raw,
		Timestamp: time.Now().UnixMilli(),
	}
}

// loadProjectState 读取项目当前切台状态，没有记录时返回零值。
func loadProjectState(projectID uint) models.ProjectState {
	var state models.ProjectState
	// ID == 0（这里是 ProjectID == 0）的判断不能省：First 查不到时不报错。
	if err := facades.Orm().Query().Where("project_id = ?", projectID).
		First(&state); err != nil || state.ProjectID == 0 {
		return models.ProjectState{}
	}
	return state
}

// saveProjectState 落库项目当前切台状态。
//
// project_id 是主键，所以要自己判断「插入还是更新」——用 Count 明确判断，
// 不能靠 First 的 error（SQLite 下查不到不报错，见 models/project.go 的说明）。
func saveProjectState(projectID uint, current, next string) {
	state := models.ProjectState{
		ProjectID:   projectID,
		CurrentShot: current,
		NextShot:    next,
		UpdatedAt:   time.Now(),
	}

	existing, err := facades.Orm().Query().Model(&models.ProjectState{}).
		Where("project_id = ?", projectID).Count()
	if err != nil {
		log.Printf("[WS] 查询项目状态失败 project=%d: %v", projectID, err)
		return
	}
	if existing == 0 {
		if err := facades.Orm().Query().Create(&state); err != nil {
			log.Printf("[WS] 写入项目状态失败 project=%d: %v", projectID, err)
		}
		return
	}
	if _, err := facades.Orm().Query().Model(&models.ProjectState{}).
		Where("project_id = ?", projectID).Update(map[string]any{
		"current_shot": current,
		"next_shot":    next,
		"updated_at":   state.UpdatedAt,
	}); err != nil {
		log.Printf("[WS] 更新项目状态失败 project=%d: %v", projectID, err)
	}
}

// recordShotCut 记录一次真实的机位切换。
//
// 判定依据是「当前播送的机位发生了变化」，而不是「收到一条 shot_state」：
// 导播先发预告（current 不变、next 有值）再发确认（current 变成 next），
// 只有后者才是一次切台。把预告也记进去会把平均停留时长算成一半。
func recordShotCut(projectID uint, from, to string, directorID uint) {
	if to == "" || to == from {
		return
	}

	mode := models.ProjectModeLive
	var project models.Project
	if err := facades.Orm().Query().Select("id", "mode").
		Where("id = ?", projectID).First(&project); err == nil && project.ID != 0 {
		if project.Mode != "" {
			mode = project.Mode
		}
	}

	cut := models.ShotCut{
		ProjectID:  projectID,
		FromShot:   from,
		ToShot:     to,
		DirectorID: directorID,
		Mode:       mode,
		CutAt:      time.Now(),
	}
	if err := facades.Orm().Query().Create(&cut); err != nil {
		log.Printf("[WS] 记录切台失败 project=%d %s→%s: %v", projectID, from, to, err)
	}
}

// autoAcquireLock 导播上线时自动获取控制权（如果当前无人持有）
func autoAcquireLock(projectID, userID uint, client *Client) {
	var existing models.ProjectLock
	err := facades.Orm().Query().Where("project_id = ?", projectID).First(&existing)
	// ID == 0 的判断不能省：First 查不到时不报错，见 jwt.go 里的说明。
	if err == nil && existing.ID != 0 {
		// 锁存在
		if time.Now().Before(existing.ExpireAt) {
			if existing.UserID == userID {
				// 自己持有，续期
				existing.ExpireAt = time.Now().Add(90 * time.Second)
				facades.Orm().Query().Where("id = ?", existing.ID).Update(&existing)
				return
			}
			// 别人持有，不抢
			return
		}
		// 已过期，清理
		facades.Orm().Query().Where("id = ?", existing.ID).Delete(&models.ProjectLock{})
	}

	// 无人持有，自动获取
	lock := models.ProjectLock{
		ProjectID: projectID,
		UserID:    userID,
		LockedAt:  time.Now(),
		ExpireAt:  time.Now().Add(90 * time.Second),
	}
	if err := facades.Orm().Query().Create(&lock); err == nil {
		log.Printf("[WS] 导播 %d 自动获取项目 %d 控制权", userID, projectID)
		plugins.Emit(plugins.Event{
			Type:      "lock_acquire",
			ProjectID: projectID,
			UserID:    userID,
		})
		// 广播锁更新
		lockMsg := WSMessage{
			Type:      "lock_update",
			ProjectID: projectID,
			SenderID:  userID,
			Payload:   json.RawMessage(`{"action":"acquire","user_id":` + strconv.Itoa(int(userID)) + `}`),
			Timestamp: time.Now().UnixMilli(),
		}
		client.Hub.SendToProject(projectID, lockMsg, nil)
	}
}

// checkLockHolder 检查用户是否持有项目控制权
func checkLockHolder(projectID, userID uint) bool {
	var lock models.ProjectLock
	err := facades.Orm().Query().Where("project_id = ?", projectID).First(&lock)
	if err != nil || lock.ID == 0 {
		return false
	}
	if time.Now().After(lock.ExpireAt) {
		facades.Orm().Query().Where("id = ?", lock.ID).Delete(&models.ProjectLock{})
		return false
	}
	return lock.UserID == userID
}

// releaseLockOnDisconnect 导播断线时释放控制权
func releaseLockOnDisconnect(projectID, userID uint, client *Client) {
	var lock models.ProjectLock
	err := facades.Orm().Query().Where("project_id = ? AND user_id = ?", projectID, userID).First(&lock)
	if err != nil || lock.ID == 0 {
		return
	}
	facades.Orm().Query().Where("id = ?", lock.ID).Delete(&models.ProjectLock{})
	log.Printf("[WS] 导播 %d 断线，释放项目 %d 控制权", userID, projectID)
	plugins.Emit(plugins.Event{
		Type:      "lock_release",
		ProjectID: projectID,
		UserID:    userID,
		Data:      map[string]any{"reason": "disconnect"},
	})

	lockMsg := WSMessage{
		Type:      "lock_update",
		ProjectID: projectID,
		SenderID:  userID,
		Payload:   json.RawMessage(`{"action":"release","user_id":` + strconv.Itoa(int(userID)) + `,"reason":"disconnect"}`),
		Timestamp: time.Now().UnixMilli(),
	}
	client.Hub.SendToProject(projectID, lockMsg, nil)
}

func (c *Client) readPump() {
	defer func() {
		c.Hub.unregister <- c
		c.Conn.Close()
	}()

	c.Conn.SetReadLimit(4096)
	c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		c.touch()
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			break
		}

		// 任何一条消息都算「还活着」。心跳是最高频的那条，所以掉线判定
		// 只要比心跳间隔大一点就够；同时它也覆盖了「采访端在不发心跳的
		// 间隙里传了状态」这种情况。
		c.touch()

		var msg WSMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		msg.ProjectID = c.ProjectID
		msg.SenderID = c.UserID
		// 发送者身份在这里盖章，而不是让客户端自己在 payload 里声明。
		msg.SenderName = c.DisplayName
		msg.SenderRole = c.Role
		msg.Timestamp = time.Now().UnixMilli()

		// --- 入库前先把「不算数」的消息挑掉 ---
		//
		// 规则：结构非法、已废弃、内容为空的消息不是业务事件，不写 messages 表。
		// 而「格式正确但被拒绝」的切台（例如未持控制权）会照常入库，
		// 因为那是一次真实的越权尝试，属于审计线索。

		// 心跳只是保活信号：必须在这里就丢弃，否则 messages 表会被刷满。
		if IsHeartbeat(msg) {
			continue
		}

		var state ShotStatePayload
		switch msg.Type {
		case "shot_state":
			if err := json.Unmarshal(msg.Payload, &state); err != nil {
				log.Printf("[WS] shot_state 载荷非法 (project=%d): %v", c.ProjectID, err)
				sendSystemError(c, "切台指令格式错误")
				continue
			}
			if state.Current == "" && state.Next == "" {
				log.Printf("[WS] shot_state 内容为空，已忽略 (project=%d)", c.ProjectID)
				continue
			}
		case "next_shot", "confirm_switch":
			// 旧协议已并入 shot_state。这里显式吞掉，否则会掉进 default 分支
			// 被无差别广播给项目内所有客户端。
			log.Printf("[WS] 收到已废弃的 %s (project=%d)，请将客户端升级到 shot_state", msg.Type, c.ProjectID)
			sendSystemError(c, "协议已升级为 shot_state，请刷新客户端")
			continue
		}

		dbMsg := models.Message{
			ProjectID: msg.ProjectID,
			SenderID:  msg.SenderID,
			Type:      msg.Type,
			Content:   string(msg.Payload),
		}
		facades.Orm().Query().Create(&dbMsg)

		switch msg.Type {
		case "shot_state":
			// 切台状态只能由持有控制权的导播上报。
			// 之前这里写的是 `c.Role == "director" && !checkLockHolder(...)`，
			// 条件对非导播角色不成立，等于解说端/包装端/采访端都能无锁注入切台指令。
			if c.Role != "director" {
				log.Printf("[WS] 非导播角色 %s 试图上报 shot_state (project=%d)，已拒绝", c.Role, c.ProjectID)
				sendSystemError(c, "只有导播才能上报切台状态")
				continue
			}
			if !checkLockHolder(c.ProjectID, c.UserID) {
				log.Printf("[WS] 导播 %d 未持控制权却上报 shot_state (project=%d)，已拒绝", c.UserID, c.ProjectID)
				sendSystemError(c, "你未持有控制权，无法切台")
				continue
			}

			// 走到这里说明这次上报已经通过校验，是可以写进状态表与切台流水
			// 的「真事件」。被上面两道守卫拒绝的上报只留在 messages 里当审计线索。
			previous := loadProjectState(c.ProjectID)
			saveProjectState(c.ProjectID, state.Current, state.Next)
			recordShotCut(c.ProjectID, previous.CurrentShot, state.Current, c.UserID)

			// 解说端与包装端各自维护「当前播送 / 即将切台」，直接吃这份状态。
			//
			// 采访端也在这份名单里：它在首页大字显示「正在采访 / 准备切台」，
			// 名单里漏掉 interviewer 时它只能靠本地自己点状态按钮切页，
			// 表现为「切台了但采访端没反应」。
			c.Hub.SendToProjectRoles(c.ProjectID, []string{"commentator", "packaging", "interviewer"}, msg)

		case "chat":
			c.Hub.SendToProject(c.ProjectID, msg, nil)
		case "interview_status":
			// 采访状态必须先落库再广播。
			//
			// 之前这条分支只广播、不写 interview_status 表，于是导播端切回
			// 某个项目时用 GET /api/interview/:id 拿到的是空列表，只能靠
			// 后续 WS 广播慢慢补——「数据库仍是 ready」的说法其实是「数据库
			// 里根本没有这条记录」。后台的掉线扫描也需要这行数据才能改状态。
			var payload struct {
				PointCode string `json:"point_code"`
				PointName string `json:"point_name"`
				Status    string `json:"status"`
			}
			if err := json.Unmarshal(msg.Payload, &payload); err != nil || payload.PointCode == "" {
				log.Printf("[WS] interview_status 载荷非法 (project=%d): %v", c.ProjectID, err)
				sendSystemError(c, "采访状态格式错误")
				continue
			}
			PersistInterviewStatus(c.ProjectID, payload.PointCode, payload.PointName, payload.Status)
			log.Printf("[WS] 采访状态变更: project=%d roles=director,packaging", c.ProjectID)
			c.Hub.SendToProjectRoles(c.ProjectID, []string{"director", "packaging"}, msg)
		default:
			c.Hub.SendToProject(c.ProjectID, msg, nil)
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			w, err := c.Conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)
			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (h *Hub) SendToProject(projectID uint, msg WSMessage, exclude *Client) {
	msg.Timestamp = time.Now().UnixMilli()
	data, _ := json.Marshal(msg)

	h.mu.RLock()
	defer h.mu.RUnlock()

	if projectClients, ok := h.byProject[projectID]; ok {
		for client := range projectClients {
			if client == exclude {
				continue
			}
			select {
			case client.Send <- data:
			default:
			}
		}
	}
}

func (h *Hub) SendToProjectRoles(projectID uint, roles []string, msg WSMessage) {
	msg.Timestamp = time.Now().UnixMilli()
	data, _ := json.Marshal(msg)

	h.mu.RLock()
	defer h.mu.RUnlock()

	if projectClients, ok := h.byProject[projectID]; ok {
		sent := 0
		for client := range projectClients {
			for _, role := range roles {
				if client.Role == role {
					select {
					case client.Send <- data:
						sent++
					default:
					}
					break
				}
			}
		}
		log.Printf("[WS] SendToProjectRoles: project=%d, online=%d, sent=%d", projectID, len(projectClients), sent)
	} else {
		log.Printf("[WS] SendToProjectRoles: project=%d 无在线客户端", projectID)
	}
}

func (h *Hub) GetOnlineCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *Hub) GetProjectOnlineCount(projectID uint) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if projectClients, ok := h.byProject[projectID]; ok {
		return len(projectClients)
	}
	return 0
}
