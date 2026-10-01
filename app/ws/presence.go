package ws

import (
	"encoding/json"
	"log"
	"strconv"
	"time"

	"github.com/goravel/framework/facades"

	"smart-mzcmc/app/models"
	"smart-mzcmc/app/plugins"
)

// DefaultPresenceTimeout 是判断采访端「已经掉线」的无消息时长上限。
//
// 客户端每 10 秒一条心跳，90 秒等于连丢 8 条才判离线。取这个数量级是因为
// 校园 WiFi 在人多的时候会成片丢包，阈值太紧会让导播端不停在红绿之间闪。
const DefaultPresenceTimeout = 90 * time.Second

// DefaultPresenceScanInterval 是后台扫描的间隔。
//
// 60 秒是「发现掉线的最坏延迟」与「扫描频率」之间的折中：真正断开连接的
// 情况由 Hub.Run 的 unregister 分支立刻处理，扫描只是兜住「TCP 还活着但
// 人已经走出覆盖范围」这一类。
const DefaultPresenceScanInterval = 60 * time.Second

// validInterviewStatus 是采访端允许上报的状态集合。
var validInterviewStatus = map[string]bool{
	"ready": true, "preparing": true, "not_ready": true, "offline": true,
}

// PersistInterviewStatus 把采访点状态写入（或更新）interview_status 表。
//
// 之前在 WebSocket 分支里这条消息只被广播、不落库，于是
//   - 导播端切换项目时调 GET /api/interview/:projectId 拿到的是空列表；
//   - 后台掉线扫描没有可改的记录，offline 永远写不进去。
//
// 两件事都表现为「界面上显示的和数据库里的不是一回事」。
//
// 返回实际生效的状态（非法状态返回空串表示已忽略）。
func PersistInterviewStatus(projectID uint, pointCode, pointName, status string) string {
	if projectID == 0 || pointCode == "" || !validInterviewStatus[status] {
		log.Printf("[WS] 忽略非法的采访状态 project=%d point=%q status=%q", projectID, pointCode, status)
		return ""
	}

	// 判断「是否已有记录」必须用 Count：First 在 SQLite 下查不到时不报错，
	// 只把结构体留成零值，会让原本该走 Create 的分支变成对着 id=0 更新 0 行。
	existing, err := facades.Orm().Query().Model(&models.InterviewStatus{}).
		Where("project_id = ? AND point_code = ?", projectID, pointCode).Count()
	if err != nil {
		log.Printf("[WS] 查询采访状态失败 project=%d point=%s: %v", projectID, pointCode, err)
		return ""
	}

	if existing == 0 {
		record := models.InterviewStatus{
			ProjectID: projectID,
			PointCode: pointCode,
			PointName: pointName,
			Status:    status,
		}
		if pointName == "" {
			record.PointName = pointCode
		}
		if err := facades.Orm().Query().Create(&record); err != nil {
			log.Printf("[WS] 创建采访状态失败 project=%d point=%s: %v", projectID, pointCode, err)
			return ""
		}
		return record.Status
	}

	updates := map[string]any{"status": status, "updated_at": time.Now()}
	if pointName != "" {
		updates["point_name"] = pointName
	}
	if _, err := facades.Orm().Query().Model(&models.InterviewStatus{}).
		Where("project_id = ? AND point_code = ?", projectID, pointCode).
		Update(updates); err != nil {
		log.Printf("[WS] 更新采访状态失败 project=%d point=%s: %v", projectID, pointCode, err)
		return ""
	}
	return status
}

// markInterviewPointOffline 把一个采访点标成离线，并把变更广播出去。
//
// 只在状态真的发生变化时广播与发事件：断线、超时扫描、客户端主动上报
// 三条路径都会调用它，重复触发不该给导播端刷一屏重复提示、也不该给
// ntfy 推一串重复告警。
func markInterviewPointOffline(projectID uint, pointCode, reason string) bool {
	if projectID == 0 || pointCode == "" {
		return false
	}

	var record models.InterviewStatus
	// ID == 0 的判断不能省：First 查不到时不报错，只把结构体留成零值。
	if err := facades.Orm().Query().
		Where("project_id = ? AND point_code = ?", projectID, pointCode).
		First(&record); err != nil || record.ID == 0 {
		// 从未上报过状态的采访点不需要「变成离线」——它本来就是未知。
		return false
	}
	if record.Status == "offline" {
		return false
	}

	if _, err := facades.Orm().Query().Model(&models.InterviewStatus{}).
		Where("id = ?", record.ID).Update(map[string]any{
		"status":     "offline",
		"updated_at": time.Now(),
	}); err != nil {
		log.Printf("[WS] 标记采访点离线失败 project=%d point=%s: %v", projectID, pointCode, err)
		return false
	}
	log.Printf("[WS] 采访点离线: project=%d point=%s reason=%s", projectID, pointCode, reason)

	payload := `{"point_code":` + strconv.Quote(record.PointCode) +
		`,"point_name":` + strconv.Quote(record.PointName) +
		`,"status":"offline"}`

	msg := models.Message{
		ProjectID: projectID,
		Type:      "interview_status",
		Content:   payload,
	}
	facades.Orm().Query().Create(&msg)

	DefaultHub.SendToProjectRoles(projectID, []string{"director", "packaging"}, WSMessage{
		Type:      "interview_status",
		ProjectID: projectID,
		Payload:   json.RawMessage(payload),
		Timestamp: time.Now().UnixMilli(),
	})

	// 这条分支在本次改动前从未被触发过：interview_offline 事件全项目
	// 没有任何 Emit 站点，ntfy 插件的对应分支是死代码。
	plugins.Emit(plugins.Event{
		Type:      "interview_offline",
		ProjectID: projectID,
		Data:      map[string]any{"point_code": pointCode, "reason": reason},
	})
	return true
}

// scanExpiredLocks 清理已过期的控制权锁并发出 lock_timeout 事件。
//
// 之前锁过期只有「有人查询时才顺手删掉」这一条路径（checkLockHolder /
// LockController.Status），没人查就永远不删、也永远不会超时告警，
// ntfy_alert.go 的 lock_timeout 分支因此从未被触发过。
func scanExpiredLocks() {
	var locks []models.ProjectLock
	if err := facades.Orm().Query().Where("expire_at < ?", time.Now()).
		Find(&locks); err != nil {
		log.Printf("[WS] 扫描过期控制权失败: %v", err)
		return
	}

	for _, lock := range locks {
		if _, err := facades.Orm().Query().Where("id = ?", lock.ID).
			Delete(&models.ProjectLock{}); err != nil {
			log.Printf("[WS] 清理过期控制权失败 id=%d: %v", lock.ID, err)
			continue
		}
		log.Printf("[WS] 项目 %d 的控制权已超时（原持有者 %d）", lock.ProjectID, lock.UserID)

		plugins.Emit(plugins.Event{
			Type:      "lock_timeout",
			ProjectID: lock.ProjectID,
			UserID:    lock.UserID,
			Data:      map[string]any{"expire_at": lock.ExpireAt},
		})

		DefaultHub.SendToProject(lock.ProjectID, WSMessage{
			Type:      "lock_update",
			ProjectID: lock.ProjectID,
			SenderID:  lock.UserID,
			Payload: json.RawMessage(`{"action":"release","user_id":` +
				strconv.Itoa(int(lock.UserID)) + `,"reason":"timeout"}`),
			Timestamp: time.Now().UnixMilli(),
		}, nil)
	}
}

// ScanStaleClients 扫描长时间没有消息的采访端并把它们标成离线。
//
// 由 log-archive 插件的后台 ticker 调用（见 app/plugins/log_archive.go），
// 复用那个本来只用来清日志的 goroutine，不再单独起一个只为扫状态的服务。
//
// 注意：判断是否超时要在**收集阶段**做、动作阶段不持有 h.mu，因为
// markInterviewPointOffline 会调 SendToProjectRoles 去拿读锁，而 Go 的
// RWMutex 在写者排队时不允许递归加读锁——那样写会直接死锁。
func (h *Hub) ScanStaleClients(timeout time.Duration) {
	if timeout <= 0 {
		timeout = DefaultPresenceTimeout
	}

	now := time.Now()
	stale := make([]*Client, 0)

	h.mu.RLock()
	for client := range h.clients {
		if client.Role != "interviewer" || client.PointCode == "" {
			continue
		}
		last := client.lastSeen()
		if last.IsZero() {
			continue
		}
		if now.Sub(last) > timeout {
			stale = append(stale, client)
		}
	}
	h.mu.RUnlock()

	if len(stale) == 0 {
		// 没有超时客户端时仍然要扫锁：锁超时与采访掉线共用这一趟扫描。
		scanExpiredLocks()
		return
	}

	for _, client := range stale {
		log.Printf("[WS] 采访端 %s 已 %v 无消息（阈值 %v），判定为离线",
			client.ID, now.Sub(client.lastSeen()).Truncate(time.Second), timeout)
		markInterviewPointOffline(client.ProjectID, client.PointCode, "timeout")
	}

	scanExpiredLocks()
}

// ScanStaleClients 是包级入口，供后台定时器调用。
func ScanStaleClients(timeout time.Duration) {
	DefaultHub.ScanStaleClients(timeout)
}
