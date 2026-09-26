package ws

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// interviewerWebDir 记录本次启动实际托管的采访端目录，空串表示未部署。
//
// 供 /ws/status 一并回报：系统首页跑在 3000 端口，无法直接探测 3002 上
// 的静态文件（跨域且静态处理器不带 CORS 头），只能由这里告知。
var interviewerWebDir string

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

// resolveWebDir 在候选目录中挑出第一个真实存在的目录。
//
// 采访端 Web 产物的部署位置有两个约定：
//   - public/interviewer：CI 在后端仓库内构建并落盘的静态路由目录，
//     与 public/admin、public/docs 同级，随发布包一起分发。
//   - interviewer/build/web：本地开发时直接在采访端工程里 flutter build web
//     的产物，不经过拷贝。
//
// 按顺序取第一个存在的目录，因此 CI 产物优先，本地开发回退到源码目录。
// 相对路径按进程工作目录解析，返回绝对路径；都不存在时返回空字符串。
func resolveWebDir(candidates ...string) string {
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if !filepath.IsAbs(dir) {
			if abs, err := filepath.Abs(dir); err == nil {
				dir = abs
			}
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return ""
}

// StartServer 启动 3002 端口服务：WebSocket + 采访端静态站点。
func StartServer(addr string, webDirs ...string) {
	mux := http.NewServeMux()

	// WebSocket
	mux.HandleFunc("/ws", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		HandleWebSocket(DefaultHub, w, r)
	}))
	mux.HandleFunc("/ws/status", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","online_count":` + strconv.Itoa(DefaultHub.GetOnlineCount()) +
			`,"interviewer":` + strconv.Quote(interviewerWebDir) + `}`))
	}))

	// 采访端：用通配模式处理所有 /interviewer/ 开头的请求
	webDir := resolveWebDir(webDirs...)
	interviewerWebDir = webDir
	if webDir != "" {
		// 注册一个兜底处理器，匹配所有 /interviewer/ 路径
		mux.HandleFunc("/interviewer/", serveInterviewer(webDir))
		// 裸 /interviewer（无结尾斜杠）不在 "/interviewer/" 的匹配范围内，
		// 不补这一条会走到 ServeMux 的自动重定向，相对资源路径可能拼错。
		mux.HandleFunc("/interviewer", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/interviewer/", http.StatusMovedPermanently)
		})
		log.Printf("[WS] 采访端 Web: %s", webDir)
	} else {
		log.Printf("[WS] 采访端 Web 目录不存在，已跳过静态托管（候选: %s）", strings.Join(webDirs, ", "))
	}

	log.Printf("[WS] 服务器启动: %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("[WS] 服务器启动失败: %v", err)
	}
}

func serveInterviewer(webDir string) http.HandlerFunc {
	// 先取绝对路径：后面判断目录穿越时要用它做前缀比较。
	root, err := filepath.Abs(webDir)
	if err != nil {
		root = webDir
	}

	return func(w http.ResponseWriter, r *http.Request) {
		// 去掉 /interviewer/ 前缀，得到相对路径
		relPath := strings.TrimPrefix(r.URL.Path, "/interviewer/")
		if relPath == "" || relPath == "/" {
			relPath = "index.html"
		}

		// 清理路径防止目录穿越
		relPath = filepath.Clean(relPath)
		fullPath := filepath.Join(root, relPath)

		// 仅接受 root 内的普通文件。filepath.Clean 只消掉字面量 ".."，
		// URL 里的 %2e%2e 等编码形式要靠这里的前缀校验兜底；
		// 目录也一并排除，避免把目录当页面返回。
		if !withinRoot(root, fullPath) {
			http.NotFound(w, r)
			return
		}
		info, statErr := os.Stat(fullPath)
		if statErr != nil || !info.Mode().IsRegular() {
			// SPA fallback: 返回 index.html
			serveInterviewerFile(w, r, filepath.Join(root, "index.html"))
			return
		}

		serveInterviewerFile(w, r, fullPath)
	}
}

// withinRoot 判断 fullPath 是否落在 root 目录内。
func withinRoot(root, fullPath string) bool {
	rel, err := filepath.Rel(root, fullPath)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	return rel != ".." && !strings.HasPrefix(rel, "../")
}

// serveInterviewerFile 设置 Content-Type 与缓存策略后返回文件。
func serveInterviewerFile(w http.ResponseWriter, r *http.Request, file string) {
	// 设置正确的 Content-Type
	switch filepath.Ext(file) {
	case ".js", ".mjs":
		w.Header().Set("Content-Type", "application/javascript")
	case ".json", ".map":
		w.Header().Set("Content-Type", "application/json")
	case ".css":
		w.Header().Set("Content-Type", "text/css")
	case ".html":
		w.Header().Set("Content-Type", "text/html")
	case ".wasm":
		w.Header().Set("Content-Type", "application/wasm")
	case ".png":
		w.Header().Set("Content-Type", "image/png")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	}

	// http.ServeFile 只写 Last-Modified，不写 Cache-Control，浏览器会套用启发式缓存，
	// 更新采访端产物后用户仍会加载到旧版本。Flutter 的产物名不带内容哈希
	// （main.dart.js 始终同名），所以只能走「每次回源校验」。
	// 与 routes/staticSite.go 的 cacheControlFor 是同一个问题。
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")

	http.ServeFile(w, r, file)
}
