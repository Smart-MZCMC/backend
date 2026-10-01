package routes

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ginframework "github.com/gin-gonic/gin"
	ginadapter "github.com/goravel/gin"
)

func TestWriteFileRendersCompleteResponse(t *testing.T) {
	file := filepath.Join(t.TempDir(), "index.html")
	want := []byte("<html>admin</html>")
	if err := os.WriteFile(file, want, 0o600); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := ginframework.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest("GET", "/admin", nil)

	if !writeFile(ginadapter.NewContext(ginContext), file) {
		t.Fatal("writeFile returned false")
	}
	if recorder.Code != 200 {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Body.Bytes(); string(got) != string(want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/html; charset=utf-8", got)
	}
}

// TestCacheControlFor_VersionJSONMustNotBeImmutable 防的是「前端更新不生效」。
//
// SvelteKit 的自动更新检测完全依赖 _app/version.json：构建时把版本号内联进
// entry JS，运行时 fetch 它比对，不一致就硬刷新。而它路径固定、内容每次构建
// 都变——与「文件名带内容哈希所以可以 immutable 长期缓存」正好相反。
//
// 给它 immutable + max-age=31536000 之后，浏览器一年不回源，版本号永远比对不出来，
// 于是前端永远不更新：页面看着正常、数据也在动，跑的却是几个月前的 JS。
//
// 前端 fetch 虽然带了 cache-control: no-cache 请求头，但 immutable 是强化指令，
// 浏览器对它的处理比普通缓存指令强硬得多，实测不会回源。
func TestCacheControlFor_VersionJSONMustNotBeImmutable(t *testing.T) {
	for _, path := range []string{
		"public/admin/_app/version.json",
		"_app/version.json",
	} {
		got := cacheControlFor(filepath.FromSlash(path))
		if strings.Contains(got, "immutable") {
			t.Errorf("%s 的缓存头 %q 里有 immutable —— 前端将永远检测不到新版本", path, got)
		}
		if !strings.Contains(got, "no-cache") {
			t.Errorf("%s 的缓存头应为 no-cache，实际 %q", path, got)
		}
	}
}

// 带内容哈希的资源仍然要 immutable：这条不能被上面的规则误伤。
//
// 它是「前端能长期缓存」与「前端能发现更新」之间的平衡点：哈希资源内容不变时
// 名字就不变，可以放心永久缓存；version.json 名字不变而内容会变，必须反过来。
func TestCacheControlFor_HashedAssetsStayImmutable(t *testing.T) {
	got := cacheControlFor(filepath.FromSlash("public/admin/_app/immutable/entry/app.tl6BM16I.js"))
	if !strings.Contains(got, "immutable") {
		t.Errorf("哈希资源应继续 immutable，实际 %q", got)
	}
	if !strings.Contains(got, "max-age=31536000") {
		t.Errorf("哈希资源应长期缓存，实际 %q", got)
	}
}

// HTML 每次回源校验：改完站点刷新还是旧页面就是这么来的。
func TestCacheControlFor_HTMLAlwaysRevalidates(t *testing.T) {
	for _, path := range []string{"public/admin/index.html", "public/docs/404.html"} {
		got := cacheControlFor(filepath.FromSlash(path))
		if !strings.Contains(got, "no-cache") {
			t.Errorf("%s 应为 no-cache，实际 %q", path, got)
		}
	}
}

// 其他静态资源（图片、字体）缓存 1 天兜底。
func TestCacheControlFor_OtherAssetsOneDay(t *testing.T) {
	got := cacheControlFor(filepath.FromSlash("public/admin/favicon.ico"))
	if !strings.Contains(got, "max-age=86400") {
		t.Errorf("普通静态资源应缓存 1 天，实际 %q", got)
	}
}
