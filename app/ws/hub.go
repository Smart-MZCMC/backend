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
	Type      string          `json:"type"`
	ProjectID uint            `json:"project_id"`
	SenderID  uint            `json:"sender_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	Timestamp int64           `json:"timestamp"`
}

// ShotStatePayload 切台状态载荷。
//
// 导播端每次切台都上报一份完整状态，而不是分两次发「预告」和「已切」：
//   - Current 当前正在播送的机位
//   - Next    本次要切过去的机位（切过去之后即成为新的 Current）
//
// 接收端不再需要自己推断「正在播送」，直接读 Current 即可；
// 这也让后加入项目的解说端/包装端能立刻拿到状态，而不必等下一次切台。
type ShotStatePayload struct {
	Current string `json:"current"`
	Next    string `json:"next"`
}

// IsHeartbeat 判断消息是否只是保活心跳。
//
// 心跳由客户端每 10 秒发一次，属于「不算数」的消息：既不写入 messages 表，
// 也不广播给同项目的其他端，因此不会污染日志、不会计入项目消息统计。
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
	Hub       *Hub
	Send      chan []byte
	mu        sync.Mutex
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

// 连接速率限制：同一标识 1 秒内不允许重复连接
var connectionRateLimit = make(map[string]time.Time)

func HandleWebSocket(hub *Hub, w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.Atoi(r.URL.Query().Get("project_id"))
	role := r.URL.Query().Get("role")
	token := r.URL.Query().Get("token")
	userIDStr := r.URL.Query().Get("user_id")
	pointCode := r.URL.Query().Get("point_code")

	// 速率限制：同一(project_id, role, point_code) 1秒内不重复
	rateKey := strconv.Itoa(projectID) + ":" + role + ":" + pointCode
	if last, ok := connectionRateLimit[rateKey]; ok && time.Since(last) < time.Second {
		http.Error(w, `{"error":"连接过于频繁"}`, http.StatusTooManyRequests)
		return
	}
	connectionRateLimit[rateKey] = time.Now()

	var userID uint
	if userIDStr != "" {
		uid, _ := strconv.Atoi(userIDStr)
		userID = uint(uid)
	}

	if role == "director" || role == "admin" {
		if token == "" {
			http.Error(w, `{"error":"需要认证"}`, http.StatusUnauthorized)
			return
		}
		// 手动解析 JWT，避免 Guard panic
		secret := facades.Config().GetString("jwt.secret")
		if secret == "" {
			http.Error(w, `{"error":"JWT密钥未配置"}`, http.StatusInternalServerError)
			return
		}
		parsedToken, err := jwt.Parse(token, func(t *jwt.Token) (interface{}, error) {
			return []byte(secret), nil
		})
		if err != nil || !parsedToken.Valid {
			http.Error(w, `{"error":"令牌无效"}`, http.StatusUnauthorized)
			return
		}
		claims, ok := parsedToken.Claims.(jwt.MapClaims)
		if !ok {
			http.Error(w, `{"error":"令牌解析失败"}`, http.StatusUnauthorized)
			return
		}
		key, _ := claims["key"].(string)
		uid, _ := strconv.ParseUint(key, 10, 64)
		userID = uint(uid)
	}

	if projectID == 0 {
		http.Error(w, `{"error":"需要 project_id 参数"}`, http.StatusBadRequest)
		return
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
		ID:        fmt.Sprintf("client-%d", time.Now().UnixNano()),
		Conn:      conn,
		ProjectID: uint(projectID),
		UserID:    userID,
		Role:      role,
		Hub:       hub,
		Send:      make(chan []byte, 256),
	}

	hub.register <- client

	welcome := WSMessage{
		Type:      "system",
		ProjectID: uint(projectID),
		Payload:   json.RawMessage(`{"message":"连接成功","online_count":` + strconv.Itoa(hub.GetProjectOnlineCount(uint(projectID))) + `}`),
		Timestamp: time.Now().UnixMilli(),
	}
	data, _ := json.Marshal(welcome)
	client.Send <- data

	// 导播连接时：如果没有锁，自动获取控制权
	if role == "director" {
		autoAcquireLock(uint(projectID), userID, client)
	}

	go client.writePump()
	go client.readPump()
}

// autoAcquireLock 导播上线时自动获取控制权（如果当前无人持有）
func autoAcquireLock(projectID, userID uint, client *Client) {
	var existing models.ProjectLock
	err := facades.Orm().Query().Where("project_id = ?", projectID).First(&existing)
	if err == nil {
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
	if err != nil {
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
	if err != nil {
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
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			break
		}

		var msg WSMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		msg.ProjectID = c.ProjectID
		msg.SenderID = c.UserID
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
			// 解说端与包装端各自维护「当前播送 / 即将切台」，直接吃这份状态。
			c.Hub.SendToProjectRoles(c.ProjectID, []string{"commentator", "packaging"}, msg)

		case "chat":
			c.Hub.SendToProject(c.ProjectID, msg, nil)
		case "interview_status":
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
