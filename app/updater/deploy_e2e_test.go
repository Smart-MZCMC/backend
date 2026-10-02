package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// 造一个带完整站点内容的发布包（模拟 tools/package.go 的产物布局）。
func makeSiteArchive(t *testing.T, top string, adminFiles map[string]string) string {
	t.Helper()
	entries := map[string]string{
		top + "/smart-mzcmc":                   "ELFBINARY",
		top + "/public/index.html":             "<html>home</html>",
		top + "/public/docs/index.html":        "<html>docs</html>",
		top + "/public/interviewer/index.html": "<html>iv</html>",
		top + "/resources/views/a.html":        "<html>view</html>",
		top + "/database/.keep":                "占位",
	}
	for k, v := range adminFiles {
		entries[top+"/public/admin/"+k] = v
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pkg.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// 端到端：解包 → 校验完整性 → 整份换掉站点 → 清理暂存。
//
// 覆盖的是本次改动的主线：站点此前根本不在更新范围内（只解可执行文件），
// 而这里确认它不但会被取出来，还会被整份换掉、并且不留下上一版的残渣。
func TestDeploy_端到端整份换掉站点并清理暂存(t *testing.T) {
	root := t.TempDir()

	// 现场现状：上一版后台 + 上一版哈希产物 + 一个多出来的第二份后台
	writeFile(t, filepath.Join(root, "public", "admin", "index.html"), "OLD ADMIN")
	writeFile(t, filepath.Join(root, "public", "admin", "_app", "old-AAA.js"), "OLDJS")
	writeFile(t, filepath.Join(root, "public", "admin", "build", "index.html"), "SECOND")
	writeFile(t, filepath.Join(root, "public", "docs", "index.html"), "OLD DOCS")
	writeFile(t, filepath.Join(root, "public", "interviewer", "index.html"), "OLD IV")
	writeFile(t, filepath.Join(root, "public", "index.html"), "OLD HOME")
	writeFile(t, filepath.Join(root, "resources", "views", "a.html"), "OLD VIEW")
	writeFile(t, filepath.Join(root, "database", "smart-mzcmc.db"), "REAL DATABASE")
	writeFile(t, filepath.Join(root, ".env"), "JWT_SECRET=REAL")

	archive := makeSiteArchive(t, "backend-linux-amd64", map[string]string{
		"index.html":                "<html>NEW ADMIN</html>",
		"_app/immutable/new-BBB.js": "NEWJS",
	})

	stagingRoot := filepath.Join(updateDir(filepath.Join(root, "smart-mzcmc")), "stage")
	want := append([]string{"smart-mzcmc"}, deployTargets...)
	want = append(want, deployFiles...)
	found, err := extractArchive(archive, stagingRoot, want, nil)
	if err != nil {
		t.Fatalf("解包失败: %v", err)
	}
	for _, w := range want {
		if !found[w] {
			t.Fatalf("%s 应被认领", w)
		}
	}
	if err := verifyStagedSites(stagingRoot, found); err != nil {
		t.Fatalf("站点校验不该失败: %v", err)
	}

	// 逐个整份替换
	dir := updateDir(filepath.Join(root, "smart-mzcmc"))
	var pending []replaced
	for _, rel := range deployTargets {
		if !found[rel] {
			continue
		}
		if err := replaceDir(root, filepath.Join(stagingRoot, filepath.FromSlash(rel)), rel); err != nil {
			t.Fatalf("替换 %s 失败: %v", rel, err)
		}
		pending = append(pending, replaced{rel: rel, kind: kindDir, backup: oldPathFor(root, rel)})
	}
	for _, rel := range deployFiles {
		if err := replaceFile(root, filepath.Join(stagingRoot, filepath.FromSlash(rel)), rel); err != nil {
			t.Fatalf("替换 %s 失败: %v", rel, err)
		}
		pending = append(pending, replaced{rel: rel, kind: kindFile, backup: oldPathFor(root, rel)})
	}

	// 新版本上线
	if got := readFile(t, filepath.Join(root, "public", "admin", "index.html")); got != "<html>NEW ADMIN</html>" {
		t.Errorf("后台首页应是新版，实际 %q", got)
	}
	if got := readFile(t, filepath.Join(root, "public", "docs", "index.html")); got != "<html>docs</html>" {
		t.Errorf("文档站应是新版，实际 %q", got)
	}
	if got := readFile(t, filepath.Join(root, "public", "index.html")); got != "<html>home</html>" {
		t.Errorf("首页应是新版，实际 %q", got)
	}
	if got := readFile(t, filepath.Join(root, "resources", "views", "a.html")); got != "<html>view</html>" {
		t.Errorf("视图模板应是新版，实际 %q", got)
	}
	// 残渣必须消失
	if _, err := os.Stat(filepath.Join(root, "public", "admin", "_app", "old-AAA.js")); err == nil {
		t.Error("上一版哈希产物必须消失")
	}
	if _, err := os.Stat(filepath.Join(root, "public", "admin", "build")); err == nil {
		t.Error("多出来的第二份后台必须消失")
	}
	// 运行期数据一个字节都不能动
	if got := readFile(t, filepath.Join(root, "database", "smart-mzcmc.db")); got != "REAL DATABASE" {
		t.Errorf("真实数据库被动了，实际 %q", got)
	}
	if got := readFile(t, filepath.Join(root, ".env")); got != "JWT_SECRET=REAL" {
		t.Errorf(".env 被动了，实际 %q", got)
	}

	// 迁移失败 -> 全部回退
	for i := len(pending) - 1; i >= 0; i-- {
		if err := pending[i].restore(root); err != nil {
			t.Fatalf("回退 %s 失败: %v", pending[i].rel, err)
		}
	}
	if got := readFile(t, filepath.Join(root, "public", "admin", "index.html")); got != "OLD ADMIN" {
		t.Errorf("回退后台首页应是旧版，实际 %q", got)
	}
	if got := readFile(t, filepath.Join(root, "public", "admin", "build", "index.html")); got != "SECOND" {
		t.Errorf("回退后旧目录应原样恢复，实际 %q", got)
	}
	if got := readFile(t, filepath.Join(root, "public", "docs", "index.html")); got != "OLD DOCS" {
		t.Errorf("回退文档站应是旧版，实际 %q", got)
	}

	// 更新成功后的清理：整个暂存目录（含发布包、.part、旧站点备份）都不留
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("暂存目录应被整个删掉")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(b)
}
