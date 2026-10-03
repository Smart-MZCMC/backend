package updater

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这条用例是上一轮翻车后补的：更新流程此前只对着自己造的合成夹具测，
// 而夹具里的站点名是照白名单写的——白名单写错、漏掉一个站点，测试照样全绿。
//
// 合成夹具证明不了「真实的发布包能被整份换上去」。所以这里直接吃真的那个包：
// go run ./tools 打出来的 backend-linux-amd64.tar.gz。它不是构建产物里随机取的
// 样本，而是 tools/package.go 的唯一输出，与线上发布的字节一致。
//
// 包不在就跳过：_dist/ 是构建产物，不进版本库。
func TestDeploy_真实发布包端到端(t *testing.T) {
	archive := realPackage(t)
	t.Logf("用真实发布包 %s", archive)

	// 先看清包里到底有什么，别让断言只覆盖我们「以为」有的东西
	top := packageTop(t, archive)
	t.Logf("顶层目录 %s/，解出 %d 个顶层条目", top, countTopLevel(t, archive))

	root := t.TempDir()
	// 现场现状：三个站点的上一版
	for _, rel := range deploySentinels {
		writeFile(t, filepath.Join(root, filepath.FromSlash(rel)), "OLD")
	}
	writeFile(t, filepath.Join(root, "database", "smart-mzcmc.db"), "REAL DATABASE")
	writeFile(t, filepath.Join(root, ".env"), "JWT_SECRET=REAL")

	stagingRoot := filepath.Join(updateDir(root), "stage")
	want := append([]string{"smart-mzcmc"}, deployDirs...)
	found, err := extractArchive(archive, stagingRoot, want, nil)
	if err != nil {
		t.Fatalf("解真实发布包失败: %v", err)
	}

	// 包里带的二进制必须叫 smart-mzcmc —— 改名的话线上会解出一个没人能启动的文件
	if !found["smart-mzcmc"] {
		t.Fatalf("包里没有 %s/smart-mzcmc，顶层目录是 %s", top, top)
	}
	for _, rel := range deployDirs {
		if !found[rel] {
			t.Errorf("真实包里没有 %s —— 打包脚本或白名单对不上", rel)
		}
	}
	// 站点必须完整，否则不许继续：宁可更新失败，也不能把能用的后台换成 404
	if err := verifyStagedSites(stagingRoot, found); err != nil {
		t.Fatalf("真实包没通过站点完整性校验: %v", err)
	}

	// 先把包里那份的字节取出来，再做替换。
	//
	// 顺序不能反：swapDir 是 rename，会把 stagingRoot 下的 public/ 整个搬走，
	// 替换之后再回 staging 读就只会读到一个「文件不存在」——曾经就在这里绕了一圈。
	staged := map[string]string{}
	for _, s := range deploySentinels {
		staged[s] = readFile(t, filepath.Join(stagingRoot, filepath.FromSlash(s)))
	}
	if findFirst(t, stagingRoot, "public", "admin", "_app") == "" {
		t.Error("真实包里没有后台的 _app 产物，后台打开会白屏")
	}

	// 整份替换
	for _, rel := range deployDirs {
		backup := oldPathFor(root, rel)
		if err := swapDir(root, filepath.Join(stagingRoot, filepath.FromSlash(rel)), rel, backup); err != nil {
			t.Fatalf("替换 %s 失败: %v", rel, err)
		}
	}

	// 逐个比对：上线后的字节必须与包里那一份逐字节相同
	for _, s := range deploySentinels {
		if got := readFile(t, filepath.Join(root, filepath.FromSlash(s))); got != staged[s] {
			t.Errorf("%s 上线后与包里不一致（上线 %d 字节，包里 %d 字节）",
				s, len(got), len(staged[s]))
		}
	}
	// 后台的哈希产物也必须换掉——那才是真正决定能不能跑起来的文件
	if _, err := os.Stat(filepath.Join(root, "public", "admin", "_app")); err != nil {
		t.Errorf("后台产物没换上去: %v", err)
	}

	// 运行期数据一个字节都不能动
	if got := readFile(t, filepath.Join(root, "database", "smart-mzcmc.db")); got != "REAL DATABASE" {
		t.Errorf("真实数据库被动了，实际 %q", got)
	}
	if got := readFile(t, filepath.Join(root, ".env")); got != "JWT_SECRET=REAL" {
		t.Errorf(".env 被动了，实际 %q", got)
	}
	// 包里带这些不代表该覆盖：部署脚手架由现场维护，database/ 与 storage/ 里
	// 是真实的数据库与日志。
	//
	// 这里查的是包里那个 .keep 占位有没有被解出来，而不是目录本身在不在——上面刚
	// 亲手造过一个 database/smart-mzcmc.db，断言目录不存在会和自己打架。
	for _, banned := range []string{
		"database/.keep", "storage/logs/.keep", "storage/framework/sessions/.keep",
		"start.sh", "smart-mzcmc.service", ".env.example", "DEPLOY.md",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(banned))); err == nil {
			t.Errorf("%s 不该被解到部署目录里", banned)
		}
	}

	// 启动时的清理必须能把这一轮的全部残渣带走
	if _, err := CleanupLeftovers(root); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if _, err := os.Stat(updateDir(root)); err == nil {
		t.Error("update/ 应被清掉，几十 MB 的发布包会永久留下")
	}
	for _, rel := range deploySentinels {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("清理把上线后的 %s 也删了", rel)
		}
	}
}

// realPackage 找到真实发布包。
//
// 先找 MZCMC_DIST_DIR（CI 里 workflow 就是这么指过去的），再按 tools/package.go
// 的输出位置找。路径一律相对**仓库根**拼，不相对测试的工作目录——后者是
// app/updater，相对它拼出来的路径落在包目录里，永远找不到，于是这条用例被
// 静默跳过：看着全绿，其实根本没验过真实包。
func realPackage(t *testing.T) string {
	t.Helper()
	repo := repoRoot(t)
	if dir := os.Getenv("MZCMC_DIST_DIR"); dir != "" {
		if p := filepath.Join(dir, pkgFileName); fileExists(p) {
			return p
		}
	}
	for _, p := range []string{
		// 本地默认：仓库上一级的 dist/（多仓库工作区，产物集中归档）
		filepath.Join(repo, "..", "dist", pkgFileName),
		// 流水线里 MZCMC_DIST_DIR 之外，本地也可能落在仓库内
		filepath.Join(repo, "dist", pkgFileName),
		filepath.Join(repo, "_dist", pkgFileName),
	} {
		if fileExists(p) {
			return filepath.Clean(p)
		}
	}
	t.Skipf("找不到真实发布包（%s）；跑 `go run ./tools` 生成后自动生效", pkgFileName)
	return ""
}

// packageTop 读出包里那一层顶层目录名。
//
// 全部条目都带同一层前缀，而 extractArchive 靠剥掉它来匹配白名单。剥错了名字，
// 白名单一个都匹配不上——更新会「成功」但什么都没换，所以这个前缀名值得单独看一眼。
func packageTop(t *testing.T, archive string) string {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./")
		if i := strings.IndexByte(name, '/'); i > 0 {
			return name[:i]
		}
	}
	t.Fatal("包里找不到带顶层目录的条目")
	return ""
}

// findFirst 返回目录下第一个文件（相对路径），没有就返回空串。
func findFirst(t *testing.T, root string, parts ...string) string {
	t.Helper()
	base := filepath.Join(append([]string{root}, parts...)...)
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) == 0 {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			return e.Name()
		}
	}
	return ""
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Size() > 0
}

// pkgFileName 是 tools/package.go 打出的包名，config/update.go 的默认值也是它。
// 两处各写一遍字面量，改名时只改一处就会让更新找不到包——这里至少有一处跟着改。
const pkgFileName = "backend-linux-amd64.tar.gz"

// countTopLevel 数一包里剥掉顶层目录后剩下的顶层条目，把真实布局打进测试输出。
// 现场反馈「更新没生效」时，看这一行就能判断是包不对还是替换不对。
func countTopLevel(t *testing.T, archive string) int {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	top := packageTop(t, archive)
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./")
		name = strings.TrimPrefix(name, top+"/")
		if i := strings.IndexByte(name, '/'); i > 0 {
			seen[name[:i]] = true
		} else if name != "" && name != top {
			seen[name] = true
		}
	}
	return len(seen)
}
