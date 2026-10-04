package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// 在线更新真正跑的解包入口。这几条用例守着它的安全边界：
// 归档里的文件名是不可信输入，而替换阶段会把解出来的东西写进服务目录。
func TestExtractArchive_HappyPath(t *testing.T) {
	archive := makeArchive(t, map[string]string{
		"backend-linux-amd64/smart-mzcmc":       "BINARY",
		"backend-linux-amd64/start.sh":          "#!/bin/sh",
		"backend-linux-amd64/public/index.html": "<html>",
	})
	root := filepath.Join(t.TempDir(), "stage")
	found, err := extractArchive(archive, root, []string{"smart-mzcmc"}, nil)
	if err != nil {
		t.Fatalf("应成功，实际 %v", err)
	}
	if !found["smart-mzcmc"] {
		t.Fatal("应认出 smart-mzcmc")
	}
	got, err := os.ReadFile(filepath.Join(root, "smart-mzcmc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "BINARY" {
		t.Fatalf("内容应为 BINARY，实际 %q", got)
	}
	// 不在白名单里的条目不该被解出来，哪怕它和目标在同一层目录。
	if _, err := os.Stat(filepath.Join(root, "start.sh")); err == nil {
		t.Error("白名单外的条目不该被解出")
	}
}

func TestExtractArchive_RejectsTraversal(t *testing.T) {
	// 归档里自称要写到目标之外
	archive := makeArchive(t, map[string]string{
		"../../escaped": "PWNED",
	})
	_, err := extractArchive(archive, filepath.Join(t.TempDir(), "stage"), []string{"escaped"}, nil)
	if err == nil {
		t.Fatal("越界路径必须被拒绝")
	}
	if !strings.Contains(err.Error(), "越界") {
		t.Errorf("错误信息应说明是越界路径，实际 %q", err)
	}
}

func TestExtractArchive_RejectsSymlink(t *testing.T) {
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
	if _, err := extractArchive(path, filepath.Join(t.TempDir(), "stage"), []string{"smart-mzcmc"}, nil); err == nil {
		t.Fatal("符号链接条目必须被拒绝")
	}
}

func TestExtractArchive_ReportsMissing(t *testing.T) {
	// 包里没有目标时不报错，但必须能让调用方看出「没找到」——
	// 在线更新靠这个判定包里到底带没带前端。
	archive := makeArchive(t, map[string]string{"pkg/readme.md": "hello"})
	found, err := extractArchive(archive, filepath.Join(t.TempDir(), "stage"), []string{"smart-mzcmc"}, nil)
	if err != nil {
		t.Fatalf("解包本身不该报错，实际 %v", err)
	}
	if found["smart-mzcmc"] {
		t.Fatal("包里没有目标文件时不该被认领")
	}
}

func TestExtractArchive_RejectsMultipleCandidates(t *testing.T) {
	// 两个同名文件 -> 不知道该用哪个，必须拒绝而不是随便挑一个
	archive := makeArchive(t, map[string]string{
		"a/smart-mzcmc": "ONE",
		"b/smart-mzcmc": "TWO",
	})
	if _, err := extractArchive(archive, filepath.Join(t.TempDir(), "stage"), []string{"smart-mzcmc"}, nil); err == nil {
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

// 镜像前缀：这是「能查到有新版、但一点更新就卡住」的唯一解药，
// 而配错形态（写成域名而不是前缀、或多一个/）是最容易犯的错。
func TestConfigAssetURL(t *testing.T) {
	const raw = "https://github.com/Smart-MZCMC/backend/releases/download/v1.4.2/backend-linux-amd64.tar.gz"

	cases := []struct {
		name   string
		mirror string
		want   string
	}{
		{
			name:   "未配镜像时原样返回",
			mirror: "",
			want:   raw,
		},
		{
			name:   "前缀包裹原始 URL",
			mirror: "https://ghfast.top/",
			want:   "https://ghfast.top/" + raw,
		},
		{
			// 末尾多个斜杠不应该拼出 "//https://" —— 有些代理会因此 404。
			name:   "前缀末尾多余的斜杠要被去掉",
			mirror: "http://192.168.1.10/github-proxy///",
			want:   "http://192.168.1.10/github-proxy/" + raw,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Config{DownloadMirror: c.mirror}.assetURL(raw)
			if got != c.want {
				t.Fatalf("得到 %q，期望 %q", got, c.want)
			}
		})
	}
}

// 镜像不改变原始 URL 的可追溯性：拼接必须是纯粹的「前缀 + 原串」，
// 不能把 host 换掉——否则 sha256 之外的排查手段（看 URL 就知道来自哪儿）就没了。
func TestConfigAssetURL_KeepsOriginalPath(t *testing.T) {
	cfg := Config{DownloadMirror: "https://mirror.test"}
	got := cfg.assetURL("https://github.com/o/r/releases/download/v1/a.tgz")
	if !strings.HasSuffix(got, "/o/r/releases/download/v1/a.tgz") {
		t.Fatalf("原始路径被改动了: %s", got)
	}
	if !strings.HasPrefix(got, "https://mirror.test/") {
		t.Fatalf("镜像前缀没生效: %s", got)
	}
}

// percentOf：进度条的百分比。total 未知时必须返回 0 而不是猜一个。
func TestPercentOf(t *testing.T) {
	cases := []struct {
		done, total int64
		want        float64
	}{
		{0, 100, 0},
		{50, 100, 50},
		{100, 100, 100},
		{1, 3, 33.3},
		// 少一个字节不该显示 100%——那会让人以为下完了。
		{999, 1000, 99.9},
		// total 未知（镜像没给 Content-Length）时不给百分比。
		{12345, 0, 0},
		{12345, -1, 0},
	}
	for _, c := range cases {
		if got := percentOf(c.done, c.total); got != c.want {
			t.Errorf("percentOf(%d, %d) = %v，期望 %v", c.done, c.total, got, c.want)
		}
	}
}

// 进度字节数单调不减，且最后一次等于总字节数。
//
// 单调性是进度条不往回跳的前提；末值相等才说明「确实下完了」而不是
// 「中途断了但看起来像完成」。
func TestProgressWriter_ReportsMonotonicBytes(t *testing.T) {
	var buf bytes.Buffer
	var seen []int64
	w := &progressWriter{w: &buf, onProgress: func(done int64) { seen = append(seen, done) }}

	payload := []byte("0123456789")
	if _, err := w.Write(payload[:4]); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload[4:]); err != nil {
		t.Fatal(err)
	}

	if len(seen) != 2 {
		t.Fatalf("每次 Write 都应回调一次，实际 %d 次: %v", len(seen), seen)
	}
	if seen[0] != 4 || seen[1] != 10 {
		t.Fatalf("回调的字节数不对: %v", seen)
	}
	if seen[0] >= seen[1] {
		t.Fatalf("字节数必须单调递增: %v", seen)
	}
	if buf.String() != string(payload) {
		t.Fatalf("数据没原样写下去: %q", buf.String())
	}
}

// 没有回调时 progressWriter 也要正常工作（Apply 这条路径不报告进度）。
func TestProgressWriter_WorksWithoutCallback(t *testing.T) {
	var buf bytes.Buffer
	w := &progressWriter{w: &buf}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "abc" {
		t.Fatalf("数据没写下去: %q", buf.String())
	}
}

// humanBytes：进度文案里那个「12.3 MB」。
func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{512 * 1024, "512 KB"},
		{1024 * 1024, "1.0 MB"},
		{26623000, "25.4 MB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q，期望 %q", c.n, got, c.want)
		}
	}
}

// 终态必须同时把 Finished 与 Failed 置成明确值。
//
// 前端是靠 Finished 决定停不停轮询的；一旦出现「stage 像是成功了但 finished=false」
// 这种自相矛盾的状态，界面就会一直转圈。
func TestFinishAndFailProgress_SetTerminalFlags(t *testing.T) {
	BeginProgress()

	FailProgress("下载失败")
	got := CurrentProgress()
	if !got.Finished || !got.Failed {
		t.Fatalf("失败态应为 Finished+Failed，实际 %+v", got)
	}
	if got.Stage != StageFailed || got.Error == "" {
		t.Fatalf("失败态应带 stage 与原因: %+v", got)
	}

	FinishProgress(&ApplyResult{Version: "1.4.4", Replaced: true})
	got = CurrentProgress()
	if !got.Finished || got.Failed {
		t.Fatalf("成功态应为 Finished 且非 Failed，实际 %+v", got)
	}
	if got.Stage != StageFinished || got.Percent != 100 {
		t.Fatalf("成功态应为 stage=finished 且 100%%: %+v", got)
	}
	if got.Result == nil || got.Result.Version != "1.4.4" {
		t.Fatalf("成功态应带上结果: %+v", got)
	}
	// 替换成功意味着马上重启，提示必须说清楚下一步，否则用户会以为卡住了。
	if !strings.Contains(got.Message, "重启") {
		t.Fatalf("替换成功的提示应提到重启: %q", got.Message)
	}
}

// 新一轮任务必须清掉上一轮的残留，尤其是 Failed 与 Steps。
//
// 不清的话，界面上会同时显示上一轮的失败原因和这一轮的进度——
// 「更新失败」和「正在下载」并排出现，没法判断到底哪一次出的问题。
func TestBeginProgress_ResetsPreviousRun(t *testing.T) {
	BeginProgress()
	FailProgress("上一轮的失败")
	BeginProgress()

	got := CurrentProgress()
	if got.Failed || got.Finished || got.Error != "" {
		t.Fatalf("新一轮不该带着上一轮的失败态: %+v", got)
	}
	if len(got.Steps) != 0 {
		t.Fatalf("新一轮不该带着上一轮的日志: %v", got.Steps)
	}
}

// 校验值来源：这是引入下载镜像带来的一处安全退化，必须能被测试钉住。
//
// 用了镜像之后，checksums.txt 默认也经镜像取——于是校验和与被校验的包由同一方
// 提供，攻破镜像即可同时替换两者，sha256 校验形同虚设。配了 ChecksumURL
// 之后才恢复成「异源比对」。
//
// 用两个本地 server 区分「包从哪来」和「校验值从哪来」，断言真的打到了不同的
// 地址——只验字段存不存在毫无意义，那正是这处退化最初被忽略的原因。
func TestFetchAsset_ChecksumComesFromTrustedSource(t *testing.T) {
	var mirrorHits, trustHits int

	// 镜像：包与（未配可信源时）校验值都从这里出
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorHits++
		w.Write([]byte("from-mirror"))
	}))
	defer mirror.Close()

	// 可信源：只提供校验值
	trust := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trustHits++
		w.Write([]byte("from-trust"))
	}))
	defer trust.Close()

	rel := &release{TagName: "v1.0.0"}
	rel.Assets = append(rel.Assets, struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	}{Name: "checksums.txt", BrowserDownloadURL: mirror.URL + "/checksums.txt"})

	// 未配 ChecksumURL：校验值与包同源（不安全但可用）
	plain := New(Config{DownloadMirror: mirror.URL}, nil)
	body, err := plain.fetchAsset(rel, checksumFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "from-mirror" || mirrorHits != 1 || trustHits != 0 {
		t.Fatalf("未配可信源时应走镜像: body=%q mirror=%d trust=%d", body, mirrorHits, trustHits)
	}

	// 配了 ChecksumURL：必须绕开镜像
	pinned := New(Config{DownloadMirror: mirror.URL, ChecksumURL: trust.URL + "/sums.txt"}, nil)
	body, err = pinned.fetchAsset(rel, checksumFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "from-trust" {
		t.Fatalf("配了可信源就不该走镜像，实际拿到 %q", body)
	}
	if trustHits != 1 {
		t.Fatalf("应恰好请求一次可信源，实际 %d 次", trustHits)
	}
}

// 普通资产不受 ChecksumURL 影响：那个配置只针对校验清单。
// 把它错当成全局 URL 覆盖的话，所有资产都会去同一个地址拿，明显是配置写坏了。
func TestFetchAsset_ChecksumURLDoesNotAffectOtherAssets(t *testing.T) {
	var mirrorHits, trustHits int
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorHits++
		w.Write([]byte("asset-from-mirror"))
	}))
	defer mirror.Close()
	trust := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trustHits++
		w.Write([]byte("wrong"))
	}))
	defer trust.Close()

	rel := &release{TagName: "v1.0.0"}
	rel.Assets = append(rel.Assets, struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	}{Name: "bundle.tar.gz", BrowserDownloadURL: mirror.URL + "/bundle.tar.gz"})

	u := New(Config{DownloadMirror: mirror.URL, ChecksumURL: trust.URL}, nil)
	body, err := u.fetchAsset(rel, "bundle.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "asset-from-mirror" || mirrorHits != 1 || trustHits != 0 {
		t.Fatalf("普通资产必须走镜像: body=%q mirror=%d trust=%d", body, mirrorHits, trustHits)
	}
}

// Check 把 Release 正文（更新详情）一起带出来。
//
// 这条规则防的是「更新详情永远是空的」：fetchLatest 早就把 body 解出来了，
// 但 CheckResult 没有对应字段，于是管理后台的「更新详情」区只能显示
// 「暂无」——而运维点「应用更新」之前最需要看的恰恰是这次改了什么。
func TestCheck_带出Release正文作为更新详情(t *testing.T) {
	notes := "## 1.6.3\n\n### 修掉某处\n\n正文里有 `代码` 与 <script>alert(1)</script>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"tag_name": "v1.6.3",
			"name": "v1.6.3",
			"body": ` + strconv.Quote(notes) + `,
			"published_at": "2026-10-04T00:00:00Z",
			"html_url": "https://example.invalid/r/v1.6.3",
			"assets": [{"name": "backend-linux-amd64.tar.gz", "size": 1, "browser_download_url": "https://example.invalid/a"}]
		}`))
	}))
	defer srv.Close()

	u := New(Config{Enabled: true, Repo: "o/r", Asset: "backend-linux-amd64.tar.gz", Server: srv.URL}, nil)
	res := u.Check("1.6.2")

	if res.LatestVersion != "1.6.3" {
		t.Fatalf("LatestVersion = %q, 期望 1.6.3", res.LatestVersion)
	}
	if res.ReleaseNotes != notes {
		t.Fatalf("ReleaseNotes 未原样送达：\n得到 %q\n期望 %q", res.ReleaseNotes, notes)
	}
	// 原样送达是硬要求：正文里含 HTML 时，后端一个字节都不能加工，
	// 否则「转义」这件事就不知道该由谁负责了。
	if !strings.Contains(res.ReleaseNotes, "<script>") {
		t.Error("ReleaseNotes 被加工过：正文里的原始标记应当原样透传，转义交给前端")
	}
}

func TestCheck_未启用或取不到时不带正文(t *testing.T) {
	u := New(Config{Enabled: false, Repo: "o/r", Asset: "x"}, nil)
	if res := u.Check("1.6.2"); res.ReleaseNotes != "" {
		t.Errorf("未启用时 ReleaseNotes = %q, 期望空", res.ReleaseNotes)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	u2 := New(Config{Enabled: true, Repo: "o/r", Asset: "x", Server: srv.URL}, nil)
	if res := u2.Check("1.6.2"); res.ReleaseNotes != "" {
		t.Errorf("取不到 Release 时 ReleaseNotes = %q, 期望空", res.ReleaseNotes)
	}
}
