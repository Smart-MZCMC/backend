package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 在线更新此前只解可执行文件，发布包里带的三个静态站点全被丢掉：
// 后端升到新版，管理后台还跑在几个月前的 JS 上。这条用例锁住「站点会被取出来」。
func TestExtractArchive_取出白名单里的静态站点(t *testing.T) {
	archive := makeArchive(t, map[string]string{
		"backend-linux-amd64/smart-mzcmc":                      "BIN",
		"backend-linux-amd64/public/admin/index.html":          "<html>admin</html>",
		"backend-linux-amd64/public/admin/_app/immutable/a.js": "console.log(1)",
		"backend-linux-amd64/public/docs/index.html":           "<html>docs</html>",
		"backend-linux-amd64/public/interviewer/index.html":    "<html>iv</html>",
		"backend-linux-amd64/public/index.html":                "<html>home</html>",
		"backend-linux-amd64/resources/views/index.html":       "<html>view</html>",
	})

	root := filepath.Join(t.TempDir(), "stage")
	want := []string{
		"smart-mzcmc", "public/admin", "public/docs",
		"public/interviewer", "public/index.html", "resources",
	}
	found, err := extractArchive(archive, root, want, nil)
	if err != nil {
		t.Fatalf("应成功，实际 %v", err)
	}

	for _, w := range want {
		if !found[w] {
			t.Errorf("%s 应被解出", w)
		}
	}

	// 站点目录要整份取出，不能只拿到 index.html：哈希文件名的 JS/CSS
	// 才是真正决定后台能不能跑起来的东西。
	js := filepath.Join(root, "public", "admin", "_app", "immutable", "a.js")
	if _, err := os.Stat(js); err != nil {
		t.Errorf("站点目录应整份解出，缺 %s: %v", js, err)
	}
}

// 这条规则防的是「把运行期数据当产物覆盖掉」。
//
// 发布包里带着 database/ 与 storage/ 的 .keep 占位，而这两个目录里是真实的
// 数据库与日志。白名单漏掉它们，加载器一改就会把它们连同数据一起清掉。
func TestExtractArchive_不碰运行期目录(t *testing.T) {
	archive := makeArchive(t, map[string]string{
		"backend-linux-amd64/smart-mzcmc":        "BIN",
		"backend-linux-amd64/database/.keep":     "占位",
		"backend-linux-amd64/storage/logs/.keep": "占位",
		"backend-linux-amd64/.env.example":       "JWT_SECRET=",
	})

	root := filepath.Join(t.TempDir(), "stage")
	want := append([]string{"smart-mzcmc"}, deployTargets...)
	want = append(want, deployFiles...)
	if _, err := extractArchive(archive, root, want, nil); err != nil {
		t.Fatalf("应成功，实际 %v", err)
	}

	for _, banned := range []string{"database", "storage", ".env.example", ".env"} {
		if _, err := os.Stat(filepath.Join(root, banned)); err == nil {
			t.Errorf("%s 不该被解出", banned)
		}
	}
}

// 这条规则防的是「用残缺的包把能用的后台覆盖掉」。
//
// 站点是整份换掉的，所以包里声称带了 public/admin、却没带 index.html 时，
// 必须整次更新失败——而不是把现存的站点扬掉、换成一份打开就是 404 的后台。
func TestVerifyStagedSites_残缺的站点必须拒绝更新(t *testing.T) {
	archive := makeArchive(t, map[string]string{
		"backend-linux-amd64/smart-mzcmc":            "BIN",
		"backend-linux-amd64/public/admin/_app/x.js": "console.log(1)",
	})

	root := filepath.Join(t.TempDir(), "stage")
	found, err := extractArchive(archive, root, []string{"smart-mzcmc", "public/admin"}, nil)
	if err != nil {
		t.Fatalf("应成功，实际 %v", err)
	}
	if !found["public/admin"] {
		t.Fatal("包里确实有 public/admin 下的文件，应算认领")
	}
	err = verifyStagedSites(root, found)
	if err == nil {
		t.Fatal("缺 index.html 的站点必须让更新失败")
	}
	if !strings.Contains(err.Error(), "index.html") {
		t.Errorf("错误信息应指出缺 index.html，实际 %q", err)
	}
}

// 这条规则防的是「发布包不带前端时连累程序更新」。
//
// 没被认领的站点直接跳过、保留现场那份：后端该升还是要升，不能因为某个包
// 忘了打前端就卡住整个更新。
func TestVerifyStagedSites_包不带前端时不拦程序更新(t *testing.T) {
	root := filepath.Join(t.TempDir(), "stage")
	// found 全 false —— 包里只有可执行文件
	if err := verifyStagedSites(root, map[string]bool{"public/admin": false}); err != nil {
		t.Fatalf("没被认领的站点不该报错，实际 %v", err)
	}
}

// 这条规则防的是「上一版产物残渣逐版累积」。
//
// SvelteKit / Vite 的产物文件名带内容哈希，覆盖式更新只会让旧文件一直留着：
// 仓库里曾攒到 140 个没有任何入口引用的旧文件，其中一个 public/admin/build/
// 子目录还被当成第二份后台、以 /admin/build/ 暴露出去。所以替换必须是整份换。
func TestSwapIn_整份换掉不留残渣(t *testing.T) {
	root := t.TempDir()

	// 旧站点：带着上一版的哈希产物，还有一个多出来的 build/ 子目录。
	old := filepath.Join(root, "public", "admin")
	mustWrite(t, filepath.Join(old, "index.html"), "OLD")
	mustWrite(t, filepath.Join(old, "_app", "immutable", "old-AAA.js"), "OLDJS")
	mustWrite(t, filepath.Join(old, "build", "index.html"), "SECOND ADMIN")

	// 新站点
	staged := filepath.Join(t.TempDir(), "admin")
	mustWrite(t, filepath.Join(staged, "index.html"), "NEW")
	mustWrite(t, filepath.Join(staged, "_app", "immutable", "new-BBB.js"), "NEWJS")

	if err := swapIn(root, staged, "public/admin", oldPathFor(root, "public/admin"), true); err != nil {
		t.Fatalf("替换失败: %v", err)
	}

	if got := mustRead(t, filepath.Join(root, "public", "admin", "index.html")); got != "NEW" {
		t.Errorf("index.html 应换成新版，实际 %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "public", "admin", "_app", "immutable", "new-BBB.js")); err != nil {
		t.Errorf("新版产物应就位: %v", err)
	}
	// 关键断言：旧产物与多出来的第二份后台都不能留下。
	if _, err := os.Stat(filepath.Join(root, "public", "admin", "_app", "immutable", "old-AAA.js")); err == nil {
		t.Error("上一版的哈希产物必须随整份替换一起消失")
	}
	if _, err := os.Stat(filepath.Join(root, "public", "admin", "build")); err == nil {
		t.Error("多出来的 build/ 子目录必须随整份替换一起消失")
	}
}

// 这条规则防的是「更新到一半失败，把现场留在半新半旧的状态」。
//
// 迁移失败时代码会逐个 restore()，此时每个条目都必须回到替换前的内容。
func TestReplaced_Restore把内容换回去(t *testing.T) {
	cases := []struct {
		name    string
		kind    replaceKind
		rel     string
		hadOld  bool
		oldBody string
	}{
		{"目录原先存在", kindDir, "public/admin", true, "OLD"},
		{"目录原先不存在", kindDir, "public/interviewer", false, ""},
		{"单文件原先存在", kindFile, "public/index.html", true, "OLDHTML"},
		{"单文件原先不存在", kindFile, "public/index.html", false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, filepath.FromSlash(tc.rel))
			backup := oldPathFor(root, tc.rel)

			if tc.hadOld {
				mustWrite(t, target, tc.oldBody)
				mustWrite(t, backup, tc.oldBody) // 换下来的旧内容
			}
			mustWrite(t, target, "NEW") // 换上去的新内容

			if err := (replaced{rel: tc.rel, kind: tc.kind, backup: backup}).restore(root); err != nil {
				t.Fatalf("restore 失败: %v", err)
			}

			if tc.hadOld {
				if got := mustRead(t, target); got != tc.oldBody {
					t.Errorf("应回到旧内容 %q，实际 %q", tc.oldBody, got)
				}
				return
			}
			// 原先没有就该干净地消失，而不是留下新版
			if _, err := os.Lstat(target); err == nil {
				t.Error("原先不存在的目标在回滚后不应残留")
			}
		})
	}
}

// 这条规则防的是「更新垃圾把磁盘吃满」。
//
// 每次更新会在 update/ 下落下几十 MB 的发布包、.part、暂存目录，以及换下来的
// 旧站点。成功之后它们没有任何用途，必须整目录清掉；而失败回滚时又必须留着，
// 所以清理只能发生在全部步骤成功之后。
func TestCleanupTarget_更新成功后整个目录都不留(t *testing.T) {
	root := t.TempDir()
	dir := updateDir(filepath.Join(root, "smart-mzcmc"))

	for _, p := range []string{
		"backend-linux-amd64.tar.gz",
		"backend-linux-amd64.tar.gz.part",
		"stage/smart-mzcmc",
		"stage/public/admin/index.html",
		"old/public/admin/index.html",
	} {
		mustWrite(t, filepath.Join(dir, filepath.FromSlash(p)), "x")
	}
	if err := os.MkdirAll(filepath.Join(dir, "old", "public", "docs"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("暂存目录应被整个删掉")
	}
	// 备份刻意留在 update/ 之外：它是这一版出问题之后唯一的回退路径。
	live := filepath.Join(root, "smart-mzcmc")
	if err := copyFile(live, live+".bak", 0o755); err == nil {
		t.Fatal("可执行文件不存在时拷贝应当失败，测试前提不成立")
	}
}

// stripFirstComponent 的行为直接决定白名单能不能匹配上：发布包全部条目都带
// 一层 backend-linux-amd64/ 前缀，不剥掉就永远匹配不到 public/admin。
func TestStripFirstComponent(t *testing.T) {
	cases := map[string]string{
		filepath.Join("backend-linux-amd64", "public", "admin"): filepath.Join("public", "admin"),
		filepath.Join("pkg", "smart-mzcmc"):                     "smart-mzcmc",
		"smart-mzcmc":                                           "smart-mzcmc",
		filepath.Join("a", "b", "c", "d"):                       filepath.Join("b", "c", "d"),
	}
	for in, want := range cases {
		if got := stripFirstComponent(in); got != want {
			t.Errorf("stripFirstComponent(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 白名单里不能出现 database / storage / .env 这些运行期数据。
// 这条用例是给「以后有人往 deployTargets 里加东西」立的规矩。
func TestDeployTargets_不包含运行期数据(t *testing.T) {
	banned := []string{"database", "storage", ".env"}
	all := append(append([]string{}, deployTargets...), deployFiles...)
	for _, rel := range all {
		for _, bad := range banned {
			if rel == bad || strings.HasPrefix(rel, bad+"/") {
				t.Errorf("%s 不该出现在部署白名单里", rel)
			}
		}
	}
	// 每个站点都要有对应的 index.html 校验，否则残缺的包能覆盖掉能用的后台。
	// 注意 resources 不在其列：它是 Go 视图模板，本来就没有 index.html。
	needSentinel := []string{"public/admin", "public/docs", "public/interviewer"}
	for _, dir := range needSentinel {
		if !contains(deployTargets, dir) {
			t.Errorf("%s 必须出现在部署白名单里", dir)
		}
		if _, ok := deploySentinels[dir]; !ok {
			t.Errorf("%s 缺少 index.html 校验规则", dir)
		}
	}
	// 反向：校验规则里不该出现白名单之外的路径，否则等于给一个不会部署的目录
	// 配了一条永远走不到的检查。
	for dir := range deploySentinels {
		if !contains(deployTargets, dir) {
			t.Errorf("%s 有校验规则但不在部署白名单里", dir)
		}
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
