package tests

import (
	"os"
	"path/filepath"
	"testing"

	"smart-mzcmc/app/facades"
)

// TestDatabaseIsRedirectedToTempDir 锁住「测试库必须落在临时目录」这件事。
//
// 防的是什么：test_case.go 里那行 facades.Config().Add 是个很容易被"顺手改
// 好"掉的写法——换成 os.Setenv("DB_DATABASE", ...) 看起来更自然，而且短期内
// 可能还测不出来，因为 config/database.go 在自己的包 init() 里就把值读走了，
// Setenv 影响不到它。改回去之后测试照样「通过」，但它已经在往版本库里那份
// database/smart-mzcmc.db 写数据了：受版本控制、事后 rm 会被进程占着挡下来、
// 还会把别人的现场数据搅乱，而且没有任何一条断言会响。
//
// 所以这里不只断言路径，还要断言文件真的存在：只断言路径的话，「迁移全部静默
// 跳过、库根本没建」也能通过——而那正是 main.go 里 ensureRuntimeDirs 存在的原因
// （HasTable 静默返回 false，全部迁移跳过，服务起来了但表是空的）。
func TestDatabaseIsRedirectedToTempDir(t *testing.T) {
	if testDBDir == "" {
		t.Fatal("testDBDir 为空：init() 没跑起来，测试脚手架本身坏了")
	}

	got := facades.Config().GetString("database.connections.sqlite.database")
	if got == "" {
		t.Fatal("配置里没有 database.connections.sqlite.database，迁移根本无从下手")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("数据库路径应为绝对路径（否则与测试进程的工作目录耦合），实际是 %q", got)
	}
	if dir := filepath.Dir(got); dir != testDBDir {
		t.Errorf("数据库应落在临时目录 %q，实际落在 %q（dir=%q）——"+
			"检查 test_case.go 里那行 Config().Add 是否被换成了 os.Setenv", testDBDir, got, dir)
	}

	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("自动迁移跑完之后 SQLite 文件应当已经建出来，实际 %v", err)
	}
	if info.Size() == 0 {
		t.Error("SQLite 文件存在却是 0 字节：迁移没有真正建表")
	}
}

// TestBootstrapAlreadyRanMigrations 确认「启动即迁移」这条规则真的生效。
//
// 这条规则是「手工替换二进制升级」唯一的兜底：更新流程（updater.go 第 5 步）
// 与 systemd 重启都走同一条路径，所以这一处断言就等于替另外两个场景背书。
func TestBootstrapAlreadyRanMigrations(t *testing.T) {
	for _, table := range []string{"users", "projects", "role_permissions"} {
		if !facades.Schema().HasTable(table) {
			t.Errorf("表 %q 不存在：bootstrap.Boot() 末尾的自动迁移没有执行，或执行到一半失败了", table)
		}
	}
}
