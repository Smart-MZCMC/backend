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
	// 源码根目录。仅在本地源码运行时可用于定位 interviewer 工程，
	// 编译后的二进制里这个路径是构建机的路径，部署机上通常不存在。
	_, filename, _, _ := runtime.Caller(0)
	rootDir := filepath.Dir(filepath.Dir(filename))

	// 发布包根目录：二进制所在目录。
	//   发布包结构是「二进制 + public/ + resources/」，解压即可运行，
	//   所以 public 必须相对可执行文件定位，而不是相对源码路径或工作目录。
	exeDir := executableDir()

	// 采访端 Flutter Web 产物路径，按优先级：
	//   1. <二进制目录>/public/interviewer：CI 构建产物，随发布包分发，
	//      部署机上无需采访端源码。
	//   2. <源码根>/public/interviewer：本地在后端仓库内直接 go run 时命中。
	//   3. <源码根>/interviewer/build/web：本地开发直接 flutter build web 的产物。
	publicWebDir := filepath.Join(exeDir, "public", "interviewer")
	localWebDir := filepath.Join(rootDir, "public", "interviewer")
	sourceWebDir := filepath.Join(rootDir, "interviewer", "build", "web")

	// 初始化插件系统
	initPlugins()

	// 启动独立服务器：WebSocket (3002) + 采访端静态文件
	go ws.StartServer(":3002", publicWebDir, localWebDir, sourceWebDir)

	app := bootstrap.Boot()
	app.Start()
}

// executableDir 返回当前可执行文件所在目录。
// 解析失败时回退到当前工作目录，让 public/ 至少还有机会被找到。
func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		if wd, wdErr := os.Getwd(); wdErr == nil {
			return wd
		}
		return "."
	}
	// Linux 上 /proc/self/exe 是符号链接，解析后再取目录。
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
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
