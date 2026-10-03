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
	want := append([]string{"smart-mzcmc"}, deployDirs...)
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
	found, err := extractArchive(archive, root, []string{"smart-mzcmc", "public"}, nil)
	if err != nil {
		t.Fatalf("应成功，实际 %v", err)
	}
	if !found["public"] {
		t.Fatal("包里确实有 public 下的文件，应算认领")
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
	// 包里只有可执行文件，没有 public
	if err := verifyStagedSites(root, map[string]bool{"smart-mzcmc": true}); err != nil {
		t.Fatalf("没被认领的站点不该报错，实际 %v", err)
	}
}

// 这条规则防的是「上一版产物残渣逐版累积」。
//
// SvelteKit / Vite 的产物文件名带内容哈希，覆盖式更新只会让旧文件一直留着：
// 仓库里曾攒到 140 个没有任何入口引用的旧文件，其中一个 public/admin/build/
// 子目录还被当成第二份后台、以 /admin/build/ 暴露出去。所以替换必须是整份换。
func TestSwapDir_整份换掉不留残渣(t *testing.T) {
	root := t.TempDir()

	// 旧站点：带着上一版的哈希产物，还有一个多出来的 build/ 子目录。
	mustWrite(t, filepath.Join(root, "public", "admin", "index.html"), "OLD")
	mustWrite(t, filepath.Join(root, "public", "admin", "_app", "immutable", "old-AAA.js"), "OLDJS")
	mustWrite(t, filepath.Join(root, "public", "admin", "build", "index.html"), "SECOND ADMIN")

	// 新版本：整个 public 一份
	staged := filepath.Join(t.TempDir(), "public")
	mustWrite(t, filepath.Join(staged, "admin", "index.html"), "NEW")
	mustWrite(t, filepath.Join(staged, "admin", "_app", "immutable", "new-BBB.js"), "NEWJS")

	if err := swapDir(root, staged, "public", oldPathFor(root, "public")); err != nil {
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
		rel     string
		hadOld  bool
		oldBody string
	}{
		{"目录原先存在", "public", true, "OLD"},
		{"目录原先不存在", "resources", false, ""},
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

			if err := (replaced{rel: tc.rel, backup: backup}).restore(root); err != nil {
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
// 旧站点。进程被杀是常态（systemd 超时、断电、运维直接 kill），那一刻的 update/
// 就成了孤儿——不会被用到也不会自己消失，下一轮更新又在同一目录上重新铺一遍。
// 所以清理必须在启动时就做，而不是只在更新成功后做。
func TestCleanupLeftovers_整个暂存目录不留(t *testing.T) {
	root := t.TempDir()
	dir := updateDir(root)

	for _, p := range []string{
		"backend-linux-amd64.tar.gz",
		"backend-linux-amd64.tar.gz.part",
		"stage/smart-mzcmc",
		"stage/public/admin/index.html",
		"old/public/admin/index.html",
		"old/public/docs/guide/index.html",
	} {
		mustWrite(t, filepath.Join(dir, filepath.FromSlash(p)), "x")
	}
	// 部署根里那些真正要留的东西
	mustWrite(t, filepath.Join(root, "smart-mzcmc"), "BIN")
	mustWrite(t, filepath.Join(root, "public", "admin", "index.html"), "LIVE")

	got, err := CleanupLeftovers(root)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if got != dir {
		t.Errorf("应报告清掉 %s，实际 %s", dir, got)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("暂存目录应被整个删掉")
	}
	if mustRead(t, filepath.Join(root, "public", "admin", "index.html")) != "LIVE" {
		t.Error("现场那份站点不该被动过")
	}
	if mustRead(t, filepath.Join(root, "smart-mzcmc")) != "BIN" {
		t.Error("可执行文件不该被动过")
	}
}

// 没更新过的时候没有 update/ 可清，这必须是正常情况而不是错误——
// 全新部署的第一次启动就走这条路。
func TestCleanupLeftovers_没有残留时静默通过(t *testing.T) {
	root := t.TempDir()
	dir, err := CleanupLeftovers(root)
	if err != nil {
		t.Fatalf("不该报错，实际 %v", err)
	}
	if dir != updateDir(root) {
		t.Errorf("应报告 %s，实际 %s", updateDir(root), dir)
	}
}

// 这条规则防的是「传了个空串，清掉的是另一处 update/」。
//
// updateDir 对空串会回退到工作目录，于是删的是 <cwd>/update 而不是 <程序目录>/update——
// 现场那份几十 MB 的残渣永久留着，且全程没有任何报错。宁可不清。
func TestCleanupLeftovers_拒绝空串(t *testing.T) {
	for _, root := range []string{"", "   ", "\t\n"} {
		dir, err := CleanupLeftovers(root)
		if err == nil {
			t.Errorf("root=%q 应拒绝清理，实际返回了 %s", root, dir)
		}
	}
}

// 这条规则防的是「守卫哪天被改成删部署根」。
//
// RemoveAll 没有第二次机会。无论传进来的是什么目录，都只能删它的 update/ 子目录，
// 部署根里的二进制、站点、数据库目录一个都不能少。这条用例把「删不掉」写成断言，
// 而不只是检查函数返回值。
func TestCleanupLeftovers_传任何目录都只删它的update子目录(t *testing.T) {
	// 故意混进一层父目录：守卫要是退化成「删传进来的东西」，
	// 第一个子用例就会把整个 TempDir 连同父目录一起带走。
	outer := t.TempDir()
	for _, rel := range []string{"deploy", "deploy2"} {
		root := filepath.Join(outer, rel)
		mustWrite(t, filepath.Join(root, "smart-mzcmc"), "BIN")
		mustWrite(t, filepath.Join(root, "public", "admin", "index.html"), "LIVE")
		mustWrite(t, filepath.Join(root, "database", "main.db"), "SQLITE")
		mustWrite(t, filepath.Join(updateDir(root), "old.tar.gz"), "x")

		if _, err := CleanupLeftovers(root); err != nil {
			t.Fatalf("%s 清理失败: %v", rel, err)
		}

		for _, keep := range []string{
			filepath.Join(root, "smart-mzcmc"),
			filepath.Join(root, "public", "admin", "index.html"),
			filepath.Join(root, "database", "main.db"),
		} {
			if _, err := os.Stat(keep); err != nil {
				t.Errorf("%s 清理后 %s 消失了", rel, keep)
			}
		}
		if _, err := os.Stat(updateDir(root)); err == nil {
			t.Errorf("%s 的 update/ 应被删掉", rel)
		}
	}
}

// updateDir 的入参是部署根目录。这个回归用例盯的是「别再把可执行文件名传进来」：
// 传文件名会拼出工作目录下的 update/，于是下载的发布包与解出来的 stage 全留在那儿，
// 而启动清理只清程序目录那一份，几十 MB 的残渣永久留下且不报任何错。
func TestUpdateDir_入参是部署根不是可执行文件名(t *testing.T) {
	root := t.TempDir()
	if got, want := updateDir(root), filepath.Join(root, "update"); got != want {
		t.Errorf("应得到 %s，实际 %s", want, got)
	}
	// 与部署根同址：这是启动清理能找到残渣的唯一前提
	if got := oldPathFor(root, "public"); got != filepath.Join(root, "update", "old", "public") {
		t.Errorf("备份路径应与暂存目录同在 update/ 下，实际 %s", got)
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
// 这条用例是给「以后有人往 deployDirs 里加东西」立的规矩。
func TestDeployDirs_不包含运行期数据(t *testing.T) {
	banned := []string{"database", "storage", ".env"}
	for _, rel := range append(append([]string{}, deployDirs...), deploySentinels...) {
		for _, bad := range banned {
			if rel == bad || strings.HasPrefix(rel, bad+"/") {
				t.Errorf("%s 不该出现在部署白名单里", rel)
			}
		}
	}
	// public 是整份换的，所以包里缺任何一个站点的 index.html 都必须整次更新失败，
	// 而不是把现存的站点扬掉、换成一份打开就是 404 的后台。
	//
	// 注意 resources 不要求有 index.html：它是 Go 视图模板。
	for _, want := range []string{
		"public/admin/index.html",
		"public/docs/index.html",
		"public/interviewer/index.html",
	} {
		if !contains(deploySentinels, want) {
			t.Errorf("缺少 %s 的校验规则", want)
		}
	}
	// 反向：校验规则里的路径必须真的会被替换，否则等于配了一条永远走不到的检查。
	for _, s := range deploySentinels {
		covered := false
		for _, dir := range deployDirs {
			if strings.HasPrefix(s, dir+"/") {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("%s 有校验规则，但不在任何被替换的目录下", s)
		}
	}
}

// 这条规则防的是「白名单里的站点名与仓库里实际的目录对不上」。
//
// 上一轮翻车的根因就在这里：更新流程只对着自己造的合成夹具测，夹具里的站点名
// 是照着白名单写的，于是白名单写错、拼错、漏掉一个站点，测试照样全绿。发布包里
// 的 public 就是仓库里的 public（tools/package.go 直接把它打进包），所以拿仓库
// 里的真实文件当基准，才有一条在 CI 里也成立的检查。
//
// 顺带挡住另一个方向的错：改了 deployDirs 却忘了加对应的 sentinel 校验规则。
func TestDeployDirs_与仓库里的真实站点对得上(t *testing.T) {
	repo := repoRoot(t)

	// 白名单里的每个目录都必须是仓库里真实存在的目录
	for _, rel := range deployDirs {
		info, err := os.Stat(filepath.Join(repo, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("白名单里的 %s 在仓库里不存在: %v", rel, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("白名单里的 %s 应该是目录", rel)
		}
	}

	// 每个 sentinel 必须是仓库里真实存在的文件，且落在白名单目录下
	for _, s := range deploySentinels {
		path := filepath.Join(repo, filepath.FromSlash(s))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("校验规则要求的 %s 在仓库里不存在: %v", s, err)
		}
		inside := false
		for _, dir := range deployDirs {
			if strings.HasPrefix(s, dir+"/") {
				inside = true
				break
			}
		}
		if !inside {
			t.Errorf("%s 不在任何会被替换的目录下，校验规则永远走不到", s)
		}
	}

	// 反向：仓库里 public 下每一个子目录都要有 sentinel。
	// 少一个就是那个站点更新完 404，而现象与「更新失败」一模一样。
	publicDir := filepath.Join(repo, "public")
	entries, err := os.ReadDir(publicDir)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", publicDir, err)
	}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		seen++
		want := filepath.ToSlash(filepath.Join("public", e.Name(), "index.html"))
		if !contains(deploySentinels, want) {
			t.Errorf("public/%s/ 存在却没有 index.html 校验规则", e.Name())
		}
	}
	if seen == 0 {
		t.Error("public 下没有任何子目录，测试前提不成立")
	}
}

// repoRoot 定位本仓库根目录：从包目录（app/updater）往上两级。
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// 测试在包目录下运行；先确认这一点，假定不成立就直接报，不静默找错地方
	if filepath.Base(wd) != "updater" {
		t.Fatalf("预期在 app/updater 下运行，实际 %s", wd)
	}
	return filepath.Dir(filepath.Dir(wd))
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
