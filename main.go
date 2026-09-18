package main

import (
	"os"
	"path/filepath"
	"runtime"

	"smart-mzcmc/app/plugins"
	"smart-mzcmc/app/ws"
	"smart-mzcmc/bootstrap"
)

func main() {
	// 获取项目根目录
	_, filename, _, _ := runtime.Caller(0)
	rootDir := filepath.Dir(filepath.Dir(filename))

	// 采访端 Flutter Web 产物路径
	webDir := filepath.Join(rootDir, "interviewer", "build", "web")

	// 初始化插件系统
	initPlugins()

	// 启动独立服务器：WebSocket (3002) + 采访端静态文件
	go ws.StartServer(":3002", webDir)

	app := bootstrap.Boot()
	app.Start()
}

func initPlugins() {
	// ntfy-alert 插件
	ntfyServer := os.Getenv("NTFY_SERVER")
	ntfyTopic := os.Getenv("NTFY_TOPIC")
	plugins.Register(plugins.NewNtfyAlert(ntfyServer, ntfyTopic))

	// log-archive 插件（保留30天）
	la := plugins.NewLogArchive(30)
	plugins.Register(la)
	la.Start() // 启动定时清理

	// csv-export 插件
	plugins.Register(plugins.NewCSVExport())
}
