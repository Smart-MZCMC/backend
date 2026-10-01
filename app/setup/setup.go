// Package setup 负责「全新部署时的初始化」。
//
// 过去全新部署要敲三条命令：复制 .env、手填 APP_KEY 与 JWT_SECRET、跑迁移、
// 再 curl 注册管理员（见 README）。任何一步漏掉，后端要么直接拒绝启动，
// 要么起来了但登录不上，而报错信息都在服务端日志里，现场的人看不到。
//
// 现在改成：后端启动时如果发现数据库不存在，就进入「初始化模式」——除了
// 初始化接口，其余 API 一律返回 503 并把人引导到 /admin/setup；向导页上填写
// 系统名称、监听地址与管理员账号，后端据此写好 .env、跑完迁移、建好账号。
//
// 本包只依赖标准库与框架 facade，不引入业务包，避免 bootstrap → routes →
// controllers → bootstrap 的循环依赖。
package setup

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"smart-mzcmc/app/facades"
	"smart-mzcmc/app/models"
)

const (
	// EnvFileName 是框架默认读取的环境文件（见 Goravel 的 support.EnvFilePath）。
	EnvFileName = ".env"
	// EnvExampleFileName 是模板文件，发布包里随二进制一起分发。
	EnvExampleFileName = ".env.example"
	// DefaultDatabasePath 与 config/database.go 里的默认值保持一致。
	DefaultDatabasePath = "database/smart-mzcmc.db"
	// DefaultAppName 只在 .env 里还是框架占位名时使用。
	DefaultAppName = "绵中融媒体智汇导播系统"
	// frameworkDefaultAppName 是 config/app.go 的兜底值，看到它就说明
	// 用户还没配过系统名称。
	frameworkDefaultAppName = "Goravel"
	// SecretLength APP_KEY 必须是 32 位，否则框架会拒绝启动。
	SecretLength = 32
)

var (
	// completed 表示本次进程内初始化已经完成。置位后不再查库，直接放行。
	completed atomic.Bool
	// startupChecked / startupDBExisted 记录「进程启动那一刻数据库文件在不在」。
	// 必须在框架打开数据库之前采样：SQLite 一旦被打开，哪怕只是连一下，
	// 也会把空文件建出来，之后再判断就永远是「存在」了。
	startupChecked   atomic.Bool
	startupDBExisted atomic.Bool

	migratorMu sync.RWMutex
	migrator   func() error
)

// SetMigrator 注入迁移执行函数。
//
// 迁移清单定义在 bootstrap 包里，而 bootstrap 依赖 routes，routes 依赖
// controllers；controllers 反过来 import bootstrap 就成环了。所以由 main.go
// 把「跑迁移」这件事作为函数注进来。
func SetMigrator(fn func() error) {
	migratorMu.Lock()
	defer migratorMu.Unlock()
	migrator = fn
}

// RunMigrations 执行初始化需要的全部迁移。
func RunMigrations() error {
	migratorMu.RLock()
	fn := migrator
	migratorMu.RUnlock()

	if fn == nil {
		return fmt.Errorf("迁移函数未注入（应在 main.go 中调用 setup.SetMigrator）")
	}
	return fn()
}

// init 在框架读取 .env 之前完成准备工作。
//
// 为什么是 init 而不是 main：config/*.go 的 init() 会调用 facades.Config()，
// 那一步会实例化框架的配置对象并校验 APP_KEY，缺失时直接 os.Exit(0)——
// main() 根本执行不到。Go 保证被导入包的 init 先于导入包执行，所以
// config 包显式导入了本包（见 config/setup.go），这里的代码先跑。
func init() {
	// 测试进程里绝不动 .env：go test 的工作目录是包目录，
	// 凭空写出 app/setup/.env 会污染仓库，还会被误提交。
	if inTestBinary() {
		return
	}
	ensureEnvFile()
	markStartup()
}

// inTestBinary 判断当前进程是不是 go test 生成的测试二进制。
func inTestBinary() bool {
	return flag.Lookup("test.v") != nil
}

// Prepare 在 main 里调用，只负责把「当前处于初始化模式」这件事说出来。
//
// 真正的准备工作（补 .env、采样数据库文件）已经在 init 里做完了，这里刻意
// 不查库，避免在 bootstrap.Boot() 之前触碰还没注册的 ORM 服务。
func Prepare() {
	if startupChecked.Load() && !startupDBExisted.Load() {
		log.Printf("[初始化] 数据库 %s 不存在，系统进入初始化模式。"+
			"请打开 http://<本机地址>:%s/admin/setup 完成初始化向导。",
			DatabasePath(), currentPort())
	}
}

// ensureEnvFile 补齐 .env 里缺失的必需项。
func ensureEnvFile() {
	path := EnvPath()
	filled, created, err := ensureSecrets(path, EnvExamplePath())
	if err != nil {
		log.Printf("[初始化] 准备 %s 失败: %v", path, err)
		return
	}
	if created {
		log.Printf("[初始化] 已创建 %s", path)
	}
	if len(filled) > 0 {
		log.Printf("[初始化] 已在 %s 中补齐: %s", path, strings.Join(filled, ", "))
	}
}

// ensureSecrets 打开（必要时新建）path，并把缺失的必需项补上，返回补齐的键名。
//
// 只补「空着就起不来」的项，且只补空值，不覆盖用户已经写好的配置：
//   - APP_KEY：缺了框架直接退出
//   - JWT_SECRET：缺了登录接口 500
//   - DB_CONNECTION / DB_DATABASE：本项目只注册了 sqlite 连接，
//     .env.example 历史上写的是 postgres，照抄会连不上库
func ensureSecrets(path, examplePath string) (filled []string, created bool, err error) {
	file, created, err := loadOrCreate(path, examplePath)
	if err != nil {
		return nil, false, err
	}

	if len(file.Get("APP_KEY")) != SecretLength {
		key := RandomString(SecretLength)
		if key == "" {
			return nil, created, fmt.Errorf("系统随机数不可用，无法生成 APP_KEY")
		}
		file.Set("APP_KEY", key)
		filled = append(filled, "APP_KEY")
	}
	if strings.TrimSpace(file.Get("JWT_SECRET")) == "" {
		secret := RandomString(SecretLength)
		if secret == "" {
			return nil, created, fmt.Errorf("系统随机数不可用，无法生成 JWT_SECRET")
		}
		file.Set("JWT_SECRET", secret)
		filled = append(filled, "JWT_SECRET")
	}
	if strings.TrimSpace(file.Get("DB_CONNECTION")) != "sqlite" {
		// 本项目只注册了 sqlite 驱动（config/database.go）。这里改成 sqlite
		// 不是「猜」，而是把一份必然连不上的配置修回唯一可用值。
		file.Set("DB_CONNECTION", "sqlite")
		filled = append(filled, "DB_CONNECTION")
	}
	if strings.TrimSpace(file.Get("DB_DATABASE")) == "" {
		file.Set("DB_DATABASE", DefaultDatabasePath)
		filled = append(filled, "DB_DATABASE")
	}

	if len(filled) == 0 && !created {
		return nil, false, nil
	}
	if err := file.Save(); err != nil {
		return nil, created, err
	}
	return filled, created, nil
}

func markStartup() {
	startupDBExisted.Store(DatabaseFileExists())
	startupChecked.Store(true)
}

// NeedsSetup 报告系统是否还没初始化。
//
// 判据分两层：
//  1. 进程启动时数据库文件不存在 —— 明确的全新部署；
//  2. 数据库文件在，但 users 表不存在或没有账号 —— 上一次初始化中途失败、
//     或数据库被手工清空过。
//
// 一旦确认为「已初始化」，结果会被缓存，后续请求不再查库。
func NeedsSetup() bool {
	if completed.Load() {
		return false
	}
	if startupChecked.Load() && !startupDBExisted.Load() {
		return true
	}
	if !usersExist() {
		return true
	}
	completed.Store(true)
	return false
}

// Complete 标记本次进程的初始化已经完成。
func Complete() {
	completed.Store(true)
}

// usersExist 报告 users 表里是否至少有一个账号。
func usersExist() bool {
	count, err := facades.Orm().Query().Model(&models.User{}).Count()
	return err == nil && count > 0
}

// SuggestAppName 在 .env 里还是框架占位名时给出产品名。
//
// 新建的 .env 里 APP_NAME 要么为空、要么是模板里的 "Goravel"，直接回显给
// 用户会让人以为得先自己去改文件。
func SuggestAppName(current string) string {
	trimmed := strings.TrimSpace(current)
	if trimmed == "" || trimmed == frameworkDefaultAppName {
		return DefaultAppName
	}
	return trimmed
}

// DatabasePath 返回生效的 SQLite 文件路径。
//
// 顺序与框架一致：真实环境变量优先于 .env，最后才是默认值。这里刻意不读
// facades.Config()，因为 Prepare 在框架启动前就要用它。
func DatabasePath() string {
	if v := strings.TrimSpace(os.Getenv("DB_DATABASE")); v != "" {
		return v
	}
	if v := ReadEnvValue("DB_DATABASE"); v != "" {
		return v
	}
	return DefaultDatabasePath
}

// DatabaseFileExists 报告数据库文件当前是否存在。
func DatabaseFileExists() bool {
	_, err := os.Stat(DatabasePath())
	return err == nil
}

// DatabaseAtStartupExisted 暴露启动时的采样结果，供状态接口展示。
func DatabaseAtStartupExisted() bool {
	return startupDBExisted.Load()
}

// EnvPath 返回 .env 的路径（相对进程工作目录）。
func EnvPath() string { return EnvFileName }

// EnvExamplePath 返回模板文件路径。
func EnvExamplePath() string { return EnvExampleFileName }

// EnvAbsolutePath 返回 .env 的绝对路径，用于在界面上告诉用户「写到了哪」。
func EnvAbsolutePath() string {
	abs, err := filepath.Abs(EnvPath())
	if err != nil {
		return EnvPath()
	}
	return abs
}

// EnvExists 报告 .env 是否存在。
func EnvExists() bool {
	_, err := os.Stat(EnvPath())
	return err == nil
}

// EnvWritable 报告 .env 是否可写（目录可写也能新建）。
func EnvWritable() bool {
	if _, err := os.Stat(EnvPath()); err == nil {
		f, err := os.OpenFile(EnvPath(), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return false
		}
		_ = f.Close()
		return true
	}

	dir := filepath.Dir(EnvPath())
	probe, err := os.CreateTemp(dir, ".setup-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return true
}

// ReadEnvValue 读取 .env 里某个键的当前值；读不到返回空串。
func ReadEnvValue(key string) string {
	f, err := LoadEnv(EnvPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(f.Get(key))
}

// SecretConfigured 报告某个密钥是否已经配好（非空）。
func SecretConfigured(key string) bool {
	return strings.TrimSpace(ReadEnvValue(key)) != ""
}

// EnvValue 是待写入 .env 的一个键值对。
type EnvValue struct {
	Key   string
	Value string
}

// WriteAppConfig 把向导提交的配置写进 .env，返回实际写入的键值。
//
// 空值一律跳过：留空代表「不修改」，而不是「清空」。
func WriteAppConfig(values []EnvValue) (map[string]string, error) {
	return writeAppConfig(EnvPath(), EnvExamplePath(), values)
}

func writeAppConfig(path, examplePath string, values []EnvValue) (map[string]string, error) {
	file, _, err := loadOrCreate(path, examplePath)
	if err != nil {
		return nil, err
	}

	written := make(map[string]string)
	for _, kv := range values {
		if strings.TrimSpace(kv.Value) == "" {
			continue
		}
		file.Set(kv.Key, kv.Value)
		written[kv.Key] = kv.Value
	}
	if len(written) == 0 {
		return written, nil
	}
	return written, file.Save()
}

// LANIP 猜一个可供局域网访问的本机 IPv4 地址。
//
// 只用于在向导页上给一个「大概是这个」的默认值；探测不到就返回空串，
// 由用户自己填。优先级：已建立默认路由的网卡 > 其他非回环 IPv4。
func LANIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	var fallback string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || ip.IsLoopback() {
				continue
			}
			// 169.254.x.x 是链路本地地址，校园网里通常连不通。
			if ip[0] == 169 && ip[1] == 254 {
				continue
			}
			if fallback == "" {
				fallback = ip.String()
			}
			// 私网地址更可能是真实的内网出口。
			if isPrivateIPv4(ip) {
				return ip.String()
			}
		}
	}
	return fallback
}

func isPrivateIPv4(ip net.IP) bool {
	switch {
	case ip[0] == 10:
		return true
	case ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31:
		return true
	case ip[0] == 192 && ip[1] == 168:
		return true
	}
	return false
}

// currentPort 读当前监听端口，仅用于日志提示。
func currentPort() string {
	if v := strings.TrimSpace(os.Getenv("APP_PORT")); v != "" {
		return v
	}
	if v := ReadEnvValue("APP_PORT"); v != "" {
		return v
	}
	return "3000"
}
