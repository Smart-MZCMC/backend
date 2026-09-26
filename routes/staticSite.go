package routes

import (
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/goravel/framework/contracts/http"
)

// workingDir 返回进程运行目录，仅用于错误提示。
func workingDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return "(未知)"
	}
	return wd
}

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
	contentType := contentTypeFor(file)
	response.Header("Content-Type", contentType)
	response.Header("Cache-Control", cacheControlFor(file))
	if err := response.Data(http.StatusOK, contentType, data).Render(); err != nil {
		return false
	}
	return true
}

// cacheControlFor 按文件性质决定缓存策略。
//
// 没有这条规则时 Gin 只写 Last-Modified、不写 Cache-Control，浏览器会套用
// 「启发式缓存」（通常是 Last-Modified 距今 10% 的时长），于是在有效期内
// 根本不回源请求——改完首页刷新却还是旧内容，就是这么来的。
//
// 规则：
//   - HTML 文档（index.html / 404.html / VitePress 页面）必须每次回源校验，
//     否则更新站点后用户会一直看到旧页面。no-cache 不是「不缓存」，
//     它允许存储但强制先向服务器确认，命中 304 时不重复传输。
//   - SvelteKit 的 /_app/ 资源文件名带内容哈希，可以 immutable 长期缓存。
//   - 其余（图片、字体等）缓存 1 天，给哈希资源兜底。
func cacheControlFor(file string) string {
	slash := filepath.ToSlash(file)
	switch {
	case strings.Contains(slash, "/_app/"):
		return "public, max-age=31536000, immutable"
	case strings.EqualFold(filepath.Ext(file), ".html"), strings.EqualFold(filepath.Ext(file), ".htm"):
		return "no-cache, must-revalidate"
	default:
		return "public, max-age=86400"
	}
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
		// 入口页本身不存在：说明站点产物没部署，或进程运行目录不对
		// （Root 是相对 CWD 的 "./public/xxx"）。这种情况必须回 404 并说明原因，
		// 否则会返回「200 + 空 body」——浏览器只显示一个白页，极难排查。
		log.Printf("[StaticSites] %s 缺少入口页 %s（站点根=%s，运行目录=%s）",
			s.Mount, name, s.Root, workingDir())
		const text = "text/plain; charset=utf-8"
		ctx.Response().Header("Content-Type", text)
		ctx.Response().Header("Cache-Control", "no-store")
		// 必须 Render()：只设 Data 不会把响应写出去，
		// 结果会是 200 + Content-Length: 0 的空响应。
		ctx.Response().Data(http.StatusNotFound, text, []byte(
			s.Mount+" 站点产物未就绪。\n"+
				"缺少文件: "+filepath.ToSlash(filepath.Join(s.Root, name))+"\n"+
				"请确认 public/"+strings.TrimPrefix(s.Mount, "/")+" 已随发布包上传，"+
				"并且进程运行目录就是包含 public/ 的那一层。\n")).Render()
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
