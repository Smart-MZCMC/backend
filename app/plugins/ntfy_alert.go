package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// NtfyAlert ntfy 推送告警插件
type NtfyAlert struct {
	serverURL string
	topic     string
	client    *http.Client
	enabled   bool
	// reason 记录为什么没启用，会原样透出到管理后台。
	reason string
}

// NtfyConfig ntfy 插件的原始配置，来自 config/plugins.go。
type NtfyConfig struct {
	// Enabled 是显式开关（"true" / "false"）。留空时按配置是否齐全自动判断。
	Enabled string
	Server  string
	Topic   string
}

type ntfyPayload struct {
	Title    string `json:"title"`
	Message  string `json:"message"`
	Tags     string `json:"tags,omitempty"`
	Priority int    `json:"priority,omitempty"`
}

func NewNtfyAlert(cfg NtfyConfig) *NtfyAlert {
	n := &NtfyAlert{
		serverURL: strings.TrimRight(cfg.Server, "/"),
		topic:     cfgTopic(cfg.Topic),
		client:    &http.Client{Timeout: 10 * time.Second},
	}

	// 显式开关优先，但配置不全时不允许启用——否则告警会静默丢失，
	// 比直接报「未配置」更难排查。
	switch strings.ToLower(strings.TrimSpace(cfg.Enabled)) {
	case "false", "0", "off", "no":
		n.reason = "已通过 PLUGIN_NTFY_ENABLED=false 关闭"
	case "true", "1", "on", "yes":
		if n.serverURL == "" || n.topic == "" {
			n.reason = "已开启但 NTFY_SERVER / NTFY_TOPIC 未配齐"
		} else {
			n.enabled = true
		}
	default:
		switch {
		case n.serverURL == "" && n.topic == "":
			n.reason = "未配置 NTFY_SERVER / NTFY_TOPIC"
		case n.serverURL == "" || n.topic == "":
			n.reason = "NTFY_SERVER 与 NTFY_TOPIC 只配了一个，配置不完整"
		default:
			n.enabled = true
		}
	}

	if n.enabled {
		log.Printf("[NtfyAlert] 已启用: %s/%s", n.serverURL, n.topic)
	} else {
		log.Printf("[NtfyAlert] 未启用: %s", n.reason)
	}
	return n
}

func cfgTopic(topic string) string { return strings.TrimSpace(topic) }

func (n *NtfyAlert) Name() string    { return "ntfy-alert" }
func (n *NtfyAlert) Version() string { return "1.0.0" }

func (n *NtfyAlert) Describe() Descriptor {
	return Descriptor{
		Name:        n.Name(),
		Version:     n.Version(),
		Description: "把导播掉线、控制权超时、采访点离线等事件推送到 ntfy",
		Enabled:     n.enabled,
		Reason:      n.reason,
		Config: map[string]string{
			"server": n.serverURL,
			"topic":  MaskSecret(n.topic),
		},
	}
}

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
