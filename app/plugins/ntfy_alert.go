package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// NtfyAlert ntfy 推送告警插件
type NtfyAlert struct {
	serverURL string
	topic     string
	client    *http.Client
	enabled   bool
}

type ntfyPayload struct {
	Title   string `json:"title"`
	Message string `json:"message"`
	Tags    string `json:"tags,omitempty"`
	Priority int   `json:"priority,omitempty"`
}

func NewNtfyAlert(serverURL, topic string) *NtfyAlert {
	if serverURL == "" || topic == "" {
		log.Printf("[NtfyAlert] 未配置 server/topic，插件禁用")
		return &NtfyAlert{enabled: false}
	}
	return &NtfyAlert{
		serverURL: serverURL,
		topic:     topic,
		client:    &http.Client{Timeout: 10 * time.Second},
		enabled:   true,
	}
}

func (n *NtfyAlert) Name() string    { return "ntfy-alert" }
func (n *NtfyAlert) Version() string { return "1.0.0" }

func (n *NtfyAlert) OnEvent(event Event) {
	if !n.enabled {
		return
	}

	var title, message, tags string
	var priority int

	switch event.Type {
	case "director_disconnect":
		title = "导播掉线"
		message = fmt.Sprintf("项目 %d 的导播 (用户 %d) 已断开连接，控制权已释放", event.ProjectID, event.UserID)
		tags = "warning"
		priority = 4
	case "lock_timeout":
		title = "控制权超时"
		message = fmt.Sprintf("项目 %d 的控制权锁已超时", event.ProjectID)
		tags = "clock3"
		priority = 3
	case "interview_offline":
		title = "采访点离线"
		message = fmt.Sprintf("项目 %d 的采访点已离线", event.ProjectID)
		tags = "rotating_light"
		priority = 3
	case "system_error":
		title = "系统错误"
		message = fmt.Sprintf("项目 %d 发生系统错误: %v", event.ProjectID, event.Data["error"])
		tags = "x"
		priority = 5
	default:
		return // 不关心的事件类型
	}

	payload := ntfyPayload{
		Title:    title,
		Message:  message,
		Tags:     tags,
		Priority: priority,
	}

	data, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/%s", n.serverURL, n.topic)

	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		log.Printf("[NtfyAlert] 创建请求失败: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		log.Printf("[NtfyAlert] 推送失败: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		log.Printf("[NtfyAlert] 推送返回 %d", resp.StatusCode)
	} else {
		log.Printf("[NtfyAlert] 推送成功: %s", title)
	}
}

func (n *NtfyAlert) Stop() {}
