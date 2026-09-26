package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"smart-mzcmc/app/facades"
	"smart-mzcmc/app/plugins"
	"smart-mzcmc/app/ws"
	"smart-mzcmc/bootstrap"
)

func main() {
	// 补齐运行期目录。必须放在最前面——`migrate` 子命令也要用：
	// 全新部署时 database/ 还不存在，SQLite 打不开文件，HasTable 会静默返回
	// false，迁移就会「全部跳过」而不建任何表，之后服务起来了但表是空的。
	ensureRuntimeDirs()

	// `migrate` 子命令：只跑数据库迁移，不启动任何服务。
	// Goravel 的 migrate 是 console 命令，而本项目没有接入 console kernel，
	// 所以这里直接遍历 bootstrap.Migrations() 调 Up()。
	// 所有迁移都写成幂等的（建表前判存在、数据清理用 DELETE），重复执行安全。
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		app := bootstrap.Boot()
		app.Boot()
		if err := runMigrations(); err != nil {
			log.Fatalf("迁移失败: %v", err)
		}
		log.Println("迁移完成")
		return
	}

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

// ensureRuntimeDirs 创建运行期需要的目录，失败只告警不中断启动。
//
// 这些路径都相对进程工作目录，见 config/database.go 与 config/logging.go：
//   - database/ ：SQLite 数据文件（DB_DATABASE 默认 database/smart-mzcmc.db）
//   - storage/logs/ ：日志文件目录
//   - storage/framework/sessions/ ：file session 驱动的落盘位置
func ensureRuntimeDirs() {
	for _, dir := range []string{"database", "storage/logs", "storage/framework/sessions"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			// 建不出来通常意味着工作目录不可写，交给后续启动流程报错更有信息量。
			log.Printf("[启动] 创建目录 %s 失败: %v", dir, err)
		}
	}
}

// runMigrations 依次执行所有已注册迁移。
func runMigrations() error {
	for _, m := range bootstrap.Migrations() {
		log.Printf("[Migrate] 执行: %s", m.Signature())
		if err := m.Up(); err != nil {
			return fmt.Errorf("%s: %w", m.Signature(), err)
		}
	}
	return nil
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

// initPlugins 按 config/plugins.go 装配内置插件。
// 配置全部来自环境变量（见 .env.example），停用的插件依然会注册，
// 这样管理后台能显示「已停用」以及停用原因，而不是让插件凭空消失。
func initPlugins() {
	cfg := facades.Config()

	// ntfy-alert：导播掉线 / 控制权超时等事件推送
	plugins.Register(plugins.NewNtfyAlert(plugins.NtfyConfig{
		Enabled: cfg.GetString("plugins.ntfy.enabled", ""),
		Server:  cfg.GetString("plugins.ntfy.server", ""),
		Topic:   cfg.GetString("plugins.ntfy.topic", ""),
	}))

	// log-archive：定期清理过期日志
	archive := plugins.NewLogArchive(plugins.LogArchiveConfig{
		Enabled:       cfg.GetBool("plugins.log_archive.enabled", true),
		RetentionDays: cfg.GetInt("plugins.log_archive.retention_days", 30),
		CheckInterval: cfg.GetString("plugins.log_archive.check_interval", "1h"),
	})
	plugins.Register(archive)
	archive.Start() // 内部会判断是否启用

	// csv-export：日志导出接口
	plugins.Register(plugins.NewCSVExportWith(cfg.GetBool("plugins.csv_export.enabled", true)))
}
