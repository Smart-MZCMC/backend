package routes

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/goravel/framework/contracts/http"
)

// staticSite 描述一份由 Go 进程直接托管的静态站点构建产物。
type staticSite struct {
	// Mount 是站点的前缀，例如 "/admin"、"/docs"。
	Mount string
	// Root 是构建产物所在目录，相对于后端工作目录。
	Root string
	// CleanURLs 为 true 时会额外尝试 "<path>.html"，
	// 用于 VitePress 的 cleanUrls（/docs/operation-manual -> operation-manual.html）。
	CleanURLs bool
	// SPA 为 true 时，未命中的请求回退到 index.html，由前端路由接管
	// （SvelteKit 后台）。为 false 时回退到站点的 404.html，
	// 没有 404.html 才退回 index.html（VitePress 文档站）。
	SPA bool
}

// 站点实例。挂在 bootstrap/app.go 的全局中间件里，见 StaticSites() 的说明。
var staticSites = []staticSite{
	{Mount: "/admin", Root: "./public/admin", SPA: true},
	{Mount: "/docs", Root: "./public/docs", CleanURLs: true},
}

// fileIn 在 Root 内查找 rel 对应的真实普通文件，找不到返回空字符串。
// 只接受普通文件，因此目录不会被当作页面返回。
func (s staticSite) fileIn(rel string) string {
	if rel == "" || rel == "." || rel == "/" {
		return ""
	}

	candidates := []string{rel}
	if s.CleanURLs && filepath.Ext(rel) == "" {
		candidates = append(candidates, rel+".html")
	}

	for _, candidate := range candidates {
		full := filepath.Join(s.Root, filepath.FromSlash(candidate))
		if info, err := os.Stat(full); err == nil && info.Mode().IsRegular() {
			return full
		}
	}
	return ""
}

// contentTypeFor 按扩展名返回响应 Content-Type。
func contentTypeFor(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".xml":
		return "application/xml; charset=utf-8"
	case ".pdf":
		return "application/pdf"
	case ".wasm":
		return "application/wasm"
	default:
		return "application/octet-stream"
	}
}

// writeFile 读取文件并写入响应，返回是否成功。
//
// 不用 ctx.Response().File()：gin 的 File() 依赖 http.Dir + http.ServeFile，
// 在 Windows 上遇到 "D:\..." 形式的绝对路径会被 http.Dir 的路径校验拒绝，
// 表现为 200 + text/plain + Content-Length: 0 的静默空响应，
// 导致 .js/.css 等资源全部加载失败。这里自行读字节并渲染响应。
func writeFile(ctx http.Context, file string) bool {
	data, err := os.ReadFile(file)
	if err != nil {
		return false
	}

	response := ctx.Response()
	response.Header("Content-Type", contentTypeFor(file))
	if strings.Contains(filepath.ToSlash(file), "/_app/") {
		// SvelteKit 的资源文件名带内容哈希，可以长期缓存。
		response.Header("Cache-Control", "public, max-age=31536000, immutable")
	}
	if err := response.Data(http.StatusOK, contentTypeFor(file), data).Render(); err != nil {
		return false
	}
	return true
}

// handle 尝试用该站点处理请求，返回是否已接管。
//
// 这里刻意不调用 ctx.Request().Abort()：gin 的 AbortWithStatus 会重置响应，
// 把已经写好的内容清空（实测会变成空响应）。
// /admin 与 /docs 下没有任何业务路由，所以写响应后不终止链路也不会被覆盖。
func (s staticSite) handle(ctx http.Context) bool {
	requestPath := ctx.Request().Path()

	// 只接管自己前缀下的请求，其余放行给业务路由。
	if requestPath != s.Mount && !strings.HasPrefix(requestPath, s.Mount+"/") {
		return false
	}

	// rel 为相对 Root 的子路径；path.Clean 消掉 ../ 与重复斜杠。
	rel := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(requestPath, s.Mount+"/")), "/")

	// 归一化后仍以 .. 开头 => 试图逃出 Root，一律按未命中处理。
	escaped := rel == ".." || strings.HasPrefix(rel, "../")

	writePage := func(name string, status int) bool {
		if writeFile(ctx, filepath.Join(s.Root, name)) {
			return true
		}
		if status == http.StatusNotFound {
			ctx.Response().NoContent(http.StatusNotFound)
		}
		return true
	}

	if !escaped {
		// rel 为空说明请求的就是站点根（/admin、/admin/、/docs、/docs/），给首页。
		if rel == "" {
			return writePage("index.html", http.StatusOK)
		}
		// 精确命中真实文件（含 cleanUrls 的 .html 兜底）。
		if file := s.fileIn(rel); file != "" {
			if writeFile(ctx, file) {
				return true
			}
		}
	}

	// 未命中：SPA 回退到入口页（由前端路由决定渲染什么），
	// 静态站点（VitePress）回退到 404 页。
	if s.SPA && !escaped {
		return writePage("index.html", http.StatusOK)
	}
	if file := s.fileIn("404.html"); file != "" {
		if writeFile(ctx, file) {
			return true
		}
	}
	return writePage("index.html", http.StatusNotFound)
}

// StaticSites 返回一个全局中间件，把静态站点挂到各自的路径前缀下。
//
// 为什么用中间件，而不是路由或 Route().Fallback()：
//
//   - 不能只写 "/docs/{path...}" 路由：Goravel 把 "{path...}" 转成 gin 的
//     ":path"，而 ":path" 只匹配单个路径段，/docs/assets/app.js 这类多段
//     路径根本不会命中，会直接 404。
//   - 不能在 Static() 之外再注册 "/docs"、"/docs/"、"/docs/{path...}"：
//     Static() 内部注册的是 "/docs/*filepath"，gin 认为它已覆盖这些路径，
//     重复注册会 panic。
//   - 不能用 Route().Fallback()：NoRoute 路径上返回的 Response 不会被渲染，
//     客户端只会拿到空响应。
//
// 中间件在业务处理器之前运行：命中静态站点就写响应，未命中则返回 false
// 继续匹配业务路由。
func StaticSites() http.Middleware {
	return &staticSitesMiddleware{}
}

type staticSitesMiddleware struct{}

// Signature 供 Goravel 识别中间件，需全局唯一。
func (m *staticSitesMiddleware) Signature() string {
	return "smart-mzcmc-static-sites"
}

func (m *staticSitesMiddleware) Handle(ctx http.Context) {
	for _, site := range staticSites {
		if site.handle(ctx) {
			return
		}
	}
}
