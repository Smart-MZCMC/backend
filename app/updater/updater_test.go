package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 版本比较：更新流程的第一道判断，判断错了要么不更新、要么重复更新。
func TestIsNewer(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"1.2.0", "1.1.0", true},
		{"1.1.1", "1.1.0", true},
		{"2.0.0", "1.9.9", true},
		{"1.1.0", "1.1.0", false},
		{"1.0.0", "1.1.0", false},
		{"0.9.0", "1.0.0", false},
		// tag 带 v 前缀
		{"v1.2.0", "1.1.0", true},
		{"v1.1.0", "v1.1.0", false},
		{"v2.0.0", "v1.9.0", true},
		// 段数不同按补 0 处理
		{"1.2", "1.1.9", true},
		{"1.2.0", "1.2", false},
		// 格式非法时一律 false：宁可让人手动核对，也不要误判成需要更新
		{"abc", "1.1.0", false},
		{"1.1.0", "abc", false},
		{"", "1.1.0", false},
		{"1.2.3.4", "1.1.0", false},
	}
	for _, c := range cases {
		if got := isNewer(c.candidate, c.current); got != c.want {
			t.Errorf("isNewer(%q, %q) = %v，期望 %v", c.candidate, c.current, got, c.want)
		}
	}
}

func TestVerifyChecksum(t *testing.T) {
	sum := strings.Repeat("ab", 32) // 64 位十六进制
	other := strings.Repeat("cd", 32)

	cs := []byte(sum + "  backend-linux-amd64.tar.gz\n" + other + "  other.apk\n")

	if err := verifyChecksum(cs, "backend-linux-amd64.tar.gz", sum); err != nil {
		t.Fatalf("匹配时应通过，实际 %v", err)
	}
	if err := verifyChecksum(cs, "backend-linux-amd64.tar.gz", other); err == nil {
		t.Fatal("摘要不匹配时必须拒绝")
	}

	// 查不到条目 -> 拒绝。
	// 这里最容易写错成「查不到就放行」，那样一个被裁剪过的 checksums.txt
	// 就能让整个校验环节形同虚设。
	if err := verifyChecksum(cs, "missing.tar.gz", sum); err == nil {
		t.Fatal("checksums.txt 里没有该条目时必须拒绝，而不是放行")
	}
	if err := verifyChecksum([]byte("garbage\n"), "x.tar.gz", sum); err == nil {
		t.Fatal("格式不符的 checksums.txt 必须被拒绝")
	}

	// 二进制模式标记 * 与大小写差异都应容忍
	cs2 := []byte(strings.ToUpper(sum) + " *backend.tar.gz\n")
	if err := verifyChecksum(cs2, "backend.tar.gz", sum); err != nil {
		t.Fatalf("应容忍 * 前缀与大小写差异，实际 %v", err)
	}
}

// 归档里的文件名是不可信输入。解压时若直接 Join 到目标目录，
// 一个名为 ../../etc/cron.d/x 的条目就能写到目标之外。
func TestSanitizeEntryPath_RejectsEscape(t *testing.T) {
	mustReject := []string{
		"../evil",
		"../../etc/passwd",
		"a/../../../etc/passwd",
		"/etc/passwd",
		`..\..\windows\system32\x.dll`, // Windows 风格穿越
		"/abs/path",
	}
	for _, name := range mustReject {
		if _, err := sanitizeEntryPath(name); err == nil {
			t.Errorf("sanitizeEntryPath(%q) 应拒绝", name)
		}
	}

	mustAccept := []string{
		"backend-linux-amd64/smart-mzcmc",
		"backend-linux-amd64/public/index.html",
		"smart-mzcmc",
	}
	for _, name := range mustAccept {
		if _, err := sanitizeEntryPath(name); err != nil {
			t.Errorf("sanitizeEntryPath(%q) 应接受，实际 %v", name, err)
		}
	}
}

// 造一个含指定条目的 tar.gz。
func makeArchive(t *testing.T, entries map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
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

func TestExtractBinary_HappyPath(t *testing.T) {
	archive := makeArchive(t, map[string]string{
		"backend-linux-amd64/smart-mzcmc":       "BINARY",
		"backend-linux-amd64/start.sh":          "#!/bin/sh",
		"backend-linux-amd64/public/index.html": "<html>",
	})
	dest := filepath.Join(t.TempDir(), "out")
	if err := extractBinary(archive, "smart-mzcmc", dest); err != nil {
		t.Fatalf("应成功，实际 %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "BINARY" {
		t.Fatalf("内容应为 BINARY，实际 %q", got)
	}
	// 执行权限只在类 Unix 系统上有意义：Windows 没有 exec 位，
	// os.Chmod 不会反映到 Mode().Perm()，在这里断言必然失败。
	// 真正的目标平台是 linux/amd64，权限位在那儿是必需的。
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(dest)
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("解出的可执行文件应带执行权限，实际 %v", info.Mode().Perm())
		}
	}
}

func TestExtractBinary_RejectsTraversal(t *testing.T) {
	// 归档里自称要写到目标之外
	archive := makeArchive(t, map[string]string{
		"../../escaped": "PWNED",
	})
	dest := filepath.Join(t.TempDir(), "out")
	err := extractBinary(archive, "escaped", dest)
	if err == nil {
		t.Fatal("越界路径必须被拒绝")
	}
	if !strings.Contains(err.Error(), "越界") {
		t.Errorf("错误信息应说明是越界路径，实际 %q", err)
	}
}

func TestExtractBinary_RejectsSymlink(t *testing.T) {
	// 发布包里不需要符号链接，接受它只是多给攻击者一条路
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "smart-mzcmc", Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd", Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	path := filepath.Join(t.TempDir(), "evil.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := extractBinary(path, "smart-mzcmc", filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("符号链接条目必须被拒绝")
	}
}

func TestExtractBinary_MissingBinary(t *testing.T) {
	archive := makeArchive(t, map[string]string{"pkg/readme.md": "hello"})
	err := extractBinary(archive, "smart-mzcmc", filepath.Join(t.TempDir(), "out"))
	if err == nil || !strings.Contains(err.Error(), "没有找到") {
		t.Fatalf("包里没有目标文件时应报明确错误，实际 %v", err)
	}
}

func TestExtractBinary_RejectsMultipleCandidates(t *testing.T) {
	// 两个同名文件 -> 不知道该用哪个，必须拒绝而不是随便挑一个
	archive := makeArchive(t, map[string]string{
		"a/smart-mzcmc": "ONE",
		"b/smart-mzcmc": "TWO",
	})
	if err := extractBinary(archive, "smart-mzcmc", filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("同名文件超过一个时必须拒绝")
	}
}

// 下到了一个 HTML 错误页而不是发布包，是内网部署时很常见的情况。
// 必须能识别出来，而不是把它当二进制替换上去然后服务起不来。
func TestElfFingerprint_RejectsNonELF(t *testing.T) {
	dir := t.TempDir()

	html := filepath.Join(dir, "page.html")
	if err := os.WriteFile(html, []byte("<html>404 Not Found</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := elfFingerprint(html); err == nil {
		t.Fatal("HTML 文件不应被当成 ELF")
	}

	tiny := filepath.Join(dir, "tiny")
	if err := os.WriteFile(tiny, []byte{0x7f}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := elfFingerprint(tiny); err == nil {
		t.Fatal("过小的文件不应被当成 ELF")
	}

	// 构造一个 amd64 ELF 头：magic + e_machine(62) 小端
	elf := make([]byte, 20)
	copy(elf[:4], []byte{0x7f, 'E', 'L', 'F'})
	elf[18], elf[19] = 62, 0
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, elf, 0o755); err != nil {
		t.Fatal(err)
	}
	arch, err := elfFingerprint(good)
	if err != nil {
		t.Fatalf("合法 ELF 应识别成功: %v", err)
	}
	if arch != "amd64" {
		t.Fatalf("架构应为 amd64，实际 %q", arch)
	}
}
