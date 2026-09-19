package ws

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// 造一个最小站点目录树，验证前缀剥离、SPA 回退与目录穿越防护。
func setupSite(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "assets"), 0o755))
	must(os.MkdirAll(filepath.Join(root, "canvaskit"), 0o755))
	must(os.WriteFile(filepath.Join(root, "index.html"), []byte("<html>INDEX</html>"), 0o644))
	must(os.WriteFile(filepath.Join(root, "main.dart.js"), []byte("console.log(1)"), 0o644))
	must(os.WriteFile(filepath.Join(root, "assets", "app.css"), []byte("body{}"), 0o644))
	must(os.WriteFile(filepath.Join(root, "canvaskit", "canvaskit.wasm"), []byte{0x00, 'a', 's', 'm'}, 0o644))
	// 站点外文件，用于穿越测试。
	must(os.WriteFile(filepath.Join(filepath.Dir(root), "secret.txt"), []byte("SECRET"), 0o644))
	return root
}

func TestServeInterviewerRoutes(t *testing.T) {
	root := setupSite(t)
	h := serveInterviewer(root)

	cases := []struct {
		name       string
		path       string
		wantStatus int
		wantCT     string
		wantBody   string
	}{
		{"root index", "/interviewer/", 200, "text/html", "<html>INDEX</html>"},
		{"nested asset", "/interviewer/assets/app.css", 200, "text/css", "body{}"},
		{"dart js", "/interviewer/main.dart.js", 200, "application/javascript", "console.log(1)"},
		{"wasm", "/interviewer/canvaskit/canvaskit.wasm", 200, "application/wasm", "\x00asm"},
		{"spa fallback", "/interviewer/some/deep/route", 200, "text/html", "<html>INDEX</html>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest("GET", c.path, nil))
			if rec.Code != c.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, c.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); ct != c.wantCT && c.wantCT != "" {
				t.Errorf("content-type = %q, want %q", ct, c.wantCT)
			}
			if c.wantBody != "" && rec.Body.String() != c.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), c.wantBody)
			}
		})
	}
}

func TestServeInterviewerTraversalBlocked(t *testing.T) {
	root := setupSite(t)
	h := serveInterviewer(root)

	for _, p := range []string{
		"/interviewer/../secret.txt",
		"/interviewer/../../secret.txt",
		"/interviewer/%2e%2e/secret.txt",
	} {
		t.Run(p, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest("GET", p, nil))
			if body := rec.Body.String(); body == "SECRET" {
				t.Fatalf("目录穿越未被拦截，泄漏了站点外文件: %s", p)
			}
		})
	}
}

// resolveWebDir 应优先返回 CI 产物目录，其次回退源码目录。
func TestResolveWebDirPriority(t *testing.T) {
	publicDir := t.TempDir()
	sourceDir := t.TempDir()
	empty := filepath.Join(t.TempDir(), "missing")

	if got := resolveWebDir(publicDir, sourceDir); got == "" {
		t.Fatal("应命中第一个存在的目录")
	} else if resolved, _ := filepath.Abs(publicDir); got != resolved {
		t.Errorf("got %q, want %q", got, resolved)
	}
	if got := resolveWebDir(empty, sourceDir); got == "" {
		t.Fatal("应跳过不存在的目录回退到源码目录")
	}
	if got := resolveWebDir(empty, ""); got != "" {
		t.Errorf("全都不存在时应返回空字符串, got %q", got)
	}
}

// 裸 /interviewer 必须重定向到 /interviewer/，否则相对资源路径会拼错。
func TestInterviewerBarePathRedirect(t *testing.T) {
	root := setupSite(t)
	if got := resolveWebDir(root); got == "" {
		t.Fatal("resolveWebDir 未命中测试目录")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/interviewer/", serveInterviewer(root))
	mux.HandleFunc("/interviewer", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/interviewer/", http.StatusMovedPermanently)
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/interviewer", nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("status = %d, want 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/interviewer/" {
		t.Errorf("location = %q, want /interviewer/", loc)
	}
}
