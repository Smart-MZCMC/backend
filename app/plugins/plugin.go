package plugins

import (
	"log"
	"sync"
)

// Event 事件结构
type Event struct {
	Type      string         // director_disconnect, lock_timeout, interview_offline, system_error, lock_acquire, lock_release
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

// List 列出所有已注册插件
func List() []map[string]string {
	defaultRegistry.mu.RLock()
	defer defaultRegistry.mu.RUnlock()

	var result []map[string]string
	for _, p := range defaultRegistry.plugins {
		result = append(result, map[string]string{
			"name":    p.Name(),
			"version": p.Version(),
		})
	}
	return result
}
