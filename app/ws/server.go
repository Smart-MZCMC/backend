package ws

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

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

func StartServer(addr string, webDir string) {
	mux := http.NewServeMux()

	// WebSocket
	mux.HandleFunc("/ws", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		HandleWebSocket(DefaultHub, w, r)
	}))
	mux.HandleFunc("/ws/status", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","online_count":` + strconv.Itoa(DefaultHub.GetOnlineCount()) + `}`))
	}))

	// 采访端：用通配模式处理所有 /interviewer/ 开头的请求
	if webDir != "" {
		if info, err := os.Stat(webDir); err == nil && info.IsDir() {
			// 注册一个兜底处理器，匹配所有 /interviewer/ 路径
			mux.HandleFunc("/interviewer/", serveInterviewer(webDir))
			log.Printf("[WS] 采访端 Web: %s", webDir)
		} else {
			log.Printf("[WS] 采访端 Web 目录不存在: %s", webDir)
		}
	}

	log.Printf("[WS] 服务器启动: %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("[WS] 服务器启动失败: %v", err)
	}
}

func serveInterviewer(webDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 去掉 /interviewer/ 前缀，得到相对路径
		relPath := strings.TrimPrefix(r.URL.Path, "/interviewer/")
		if relPath == "" || relPath == "/" {
			relPath = "index.html"
		}

		// 清理路径防止目录穿越
		relPath = filepath.Clean(relPath)
		fullPath := filepath.Join(webDir, relPath)

		// 检查文件是否存在
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			// SPA fallback: 返回 index.html
			http.ServeFile(w, r, filepath.Join(webDir, "index.html"))
			return
		}

		// 设置正确的 Content-Type
		ext := filepath.Ext(fullPath)
		switch ext {
		case ".js":
			w.Header().Set("Content-Type", "application/javascript")
		case ".json":
			w.Header().Set("Content-Type", "application/json")
		case ".css":
			w.Header().Set("Content-Type", "text/css")
		case ".html":
			w.Header().Set("Content-Type", "text/html")
		}

		http.ServeFile(w, r, fullPath)
	}
}
