package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrations_每个迁移文件都已注册 这条规则防的是「迁移写了但从没运行过」。
//
// 20261101000006_normalize_audit_created_at 就是这么丢掉的：文件写好了、逻辑也
// 是对的（能把 +08:00 / -05:00 准确换算成 UTC，实测过），但没人把它加进
// Migrations()，于是它一次都没跑过。
//
// 而这类 bug 没有任何症状：RunMigrations 只遍历注册表，未注册的文件连一行日志都
// 不会有；迁移本身又有 HasTable / HasColumn 守卫，跑不跑都返回 nil。所以它可以
// 安静地存在任意久——直到有人恰好撞上「审计页明明有记录却查不出来」才会回头找。
//
// 反向也要查：注册表里写了却在 migrations 目录下找不到文件的，说明 Signature()
// 拼错了，或者文件被删了而注册表没跟着改。
func TestMigrations_每个迁移文件都已注册(t *testing.T) {
	registered := make(map[string]bool, len(Migrations()))
	for _, m := range Migrations() {
		registered[m.Signature()] = true
	}

	files, err := filepath.Glob(filepath.Join("..", "database", "migrations", "*.go"))
	if err != nil {
		t.Fatalf("列迁移目录失败: %v", err)
	}

	seen := make(map[string]bool, len(files))
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasSuffix(base, "_test.go") {
			continue
		}
		// 目录里可能有不带 Up() 的辅助文件，那些不是迁移，跳过。
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("访问 %s 失败: %v", base, err)
		}
		sig := strings.TrimSuffix(base, ".go")
		seen[sig] = true
		if !registered[sig] {
			t.Errorf("迁移文件 %s 没有出现在 bootstrap.Migrations() 里——这条迁移永远不会执行", base)
		}
	}

	for _, m := range Migrations() {
		if !seen[m.Signature()] {
			t.Errorf("注册表里的 %s 在 database/migrations/ 下没有对应文件", m.Signature())
		}
	}
}
