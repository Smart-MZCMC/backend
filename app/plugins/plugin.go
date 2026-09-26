package plugins

import (
	"log"
	"strings"
	"sync"
)

// Event 事件结构
type Event struct {
	// Type 事件类型。目前实际会发出的只有三个：
	//   - director_disconnect：导播断开连接
	//   - lock_acquire：导播获取到项目控制权
	//   - lock_release：控制权被释放（Data["reason"] 说明原因）
	// 其余名字（NtfyAlert 里处理的 lock_timeout / interview_offline /
	// system_error）目前没有任何 Emit 站点，插件可以先按这些类型写好分支，
	// 等后端补上触发点后无需改动。
	Type      string
	ProjectID uint
	UserID    uint
	Data      map[string]any // 事件附加数据
}

// Plugin 插件接口
type Plugin interface {
	Name() string
	Version() string
	OnEvent(event Event)
	Stop()
}

// Descriptor 插件自描述信息。
// 管理后台「插件与统计」页直接渲染它，让「配置成什么样、到底有没有生效」
// 在界面上可见，而不是只能翻日志。
type Descriptor struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	// Reason 说明插件为什么没启用；启用时为空。
	Reason string `json:"reason,omitempty"`
	// Config 是脱敏后的生效配置，键值对形式，方便后台直接列出来。
	Config map[string]string `json:"config"`
}

// Describable 可选接口。
// 实现了它的插件会把自己的配置与真实开关状态透出到后台；
// 没实现的插件由 List() 合成一份最小描述（视为启用、配置为空）。
type Describable interface {
	Describe() Descriptor
}

// describe 把任意插件转成 Descriptor，Describable 优先。
func describe(p Plugin) Descriptor {
	if d, ok := p.(Describable); ok {
		return d.Describe()
	}
	return Descriptor{
		Name:    p.Name(),
		Version: p.Version(),
		Enabled: true,
		Config:  map[string]string{},
	}
}

// Registry 插件注册中心
type Registry struct {
	plugins []Plugin
	mu      sync.RWMutex
}

var defaultRegistry = &Registry{}

// Register 注册插件
func Register(p Plugin) {
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	defaultRegistry.plugins = append(defaultRegistry.plugins, p)
	log.Printf("[Plugin] 注册插件: %s v%s", p.Name(), p.Version())
}

// Emit 触发事件给所有插件（异步）
func Emit(event Event) {
	defaultRegistry.mu.RLock()
	defer defaultRegistry.mu.RUnlock()

	for _, p := range defaultRegistry.plugins {
		go func(plugin Plugin) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[Plugin] %s 处理事件 panic: %v", plugin.Name(), r)
				}
			}()
			plugin.OnEvent(event)
		}(p)
	}
}

// StopAll 停止所有插件
func StopAll() {
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()

	for _, p := range defaultRegistry.plugins {
		p.Stop()
		log.Printf("[Plugin] 已停止: %s", p.Name())
	}
}

// List 列出所有已注册插件及其生效配置
func List() []Descriptor {
	defaultRegistry.mu.RLock()
	defer defaultRegistry.mu.RUnlock()

	result := make([]Descriptor, 0, len(defaultRegistry.plugins))
	for _, p := range defaultRegistry.plugins {
		result = append(result, describe(p))
	}
	return result
}

// MaskSecret 脱敏展示配置里的机密值。
// 只露出首尾各一两位，既能让人确认「配的是哪个」，又不会把凭据贴到界面上。
func MaskSecret(value string) string {
	runes := []rune(value)
	switch {
	case value == "":
		return ""
	case len(runes) <= 2:
		return strings.Repeat("*", len(runes))
	case len(runes) <= 6:
		return string(runes[:1]) + strings.Repeat("*", len(runes)-2) + string(runes[len(runes)-1:])
	default:
		return string(runes[:2]) + strings.Repeat("*", 4) + string(runes[len(runes)-2:])
	}
}
