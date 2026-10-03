package tests

import (
	"os"
	"path/filepath"

	"github.com/goravel/framework/testing"

	"smart-mzcmc/app/facades"
	"smart-mzcmc/bootstrap"
)

// 测试用的数据库目录。
//
// 必须在 init 里、Boot 之前就定下来：bootstrap.Boot() 末尾会自动跑一遍全部
// 迁移（见 bootstrap/app.go 的 WithCallback），迁移要真的打开 SQLite 文件。
//
// 指到临时目录有两个原因：
//
//	一、不污染仓库。database/ 没有被 .gitignore 覆盖（只有 storage），
//	   落在这里的 .db 文件会出现在 git status 里。
//	二、那个文件会被测试进程占着，事后 rm 会被 Windows 挡下来，只能进
//	   临时目录由系统清理（见 AGENTS.md「已知陷阱」第 10 条）。
var testDBDir string

func init() {
	dir, err := os.MkdirTemp("", "smart-mzcmc-test-*")
	if err != nil {
		// 造不出临时目录就没法跑测试，但不该在这里 panic：让下面 Boot 失败
		// 并给出它自己的错误，比一个看不懂的 init 崩溃好定位。
		panic("创建测试数据库临时目录失败: " + err.Error())
	}
	testDBDir = dir

	// APP_KEY / JWT_SECRET 用 os.Setenv 就够了：它们由 config.NewApplication()
	// 在**运行时**读取，晚于所有包初始化，所以这里设完还来得及。
	//
	// 长度必须是 32，框架长度不对时直接 os.Exit(0)——在测试里表现为「进程
	// 消失、没有任何输出」，极难定位。
	//
	// 这个值以前写成 29 位。它当下并不会炸：app/setup 在自己的 init 里（早于
	// config 读配置）就把 <包目录>/.env 生成出来了，APP_KEY 是随机 32 位，而
	// .env 文件的优先级高于环境变量，于是那 29 位从来没被真正用上。但它是个埋着
	// 的雷——哪天 app/setup 不再往测试目录写 .env（inTestBinary 现在其实拦不住，
	// 见 app/setup/setup.go），或者有人照着注释里的「32 位」去核对，这行就会变成
	// 静默退出的原因，而症状是「go test 进程凭空消失」。
	setIfAbsent("APP_KEY", "testappkey0123456789abcdefghij12")
	setIfAbsent("JWT_SECRET", "test-jwt-secret-0123456789abcdef")
	setIfAbsent("DB_CONNECTION", "sqlite")

	// DB_DATABASE 则**不能**用 os.Setenv：config/database.go 在自己的包
	// init() 里就把 config.Env("DB_DATABASE") 的结果读进配置了，而包初始化
	// 早于本文件的 init()，Setenv 影响不到它。
	//
	// 改用 Config().Add：它内部就是 viper 的 Set（override 层，优先级最高，
	// 高于环境变量与配置文件），而且这里是运行时调用，能盖住 config 包
	// init() 里写进去的那个值。实际生效路径由 sqlitefacades.Sqlite("sqlite")
	// 在**首次建连**时解析，那时候本行早已执行。
	facades.Config().Add("database.connections.sqlite.database", filepath.Join(dir, "test.db"))

	bootstrap.Boot()
}

// setIfAbsent 只在变量没设过时才写，避免覆盖调用者显式指定的隔离配置。
func setIfAbsent(key, value string) {
	if os.Getenv(key) == "" {
		os.Setenv(key, value)
	}
}

type TestCase struct {
	testing.TestCase
}

// Cleanup 删掉测试用的临时数据库目录。
//
// 刻意不注册 os.Exit 钩子：那会盖掉 Go 自己的退出码与 panic 输出，
// 而测试失败时能看到的堆栈比「目录删没删掉」重要得多。
// 所以由用它的测试包在自己的 TestMain 里调用。
func Cleanup() {
	if testDBDir == "" {
		return
	}
	os.RemoveAll(testDBDir)
	testDBDir = ""
}
