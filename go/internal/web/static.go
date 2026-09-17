// Package web 服务编译期嵌入的前端静态资源。
//
// 与 Rust 侧 src-tauri/src/static_files.rs 行为一致：
//  1. 精确匹配请求路径对应的文件
//  2. 未命中且路径以 api/ 开头 → 404（未知 API 不得回落到 SPA 外壳）
//  3. 其余未命中路径返回 index.html（SPA 路由支持）
//  4. 前端未嵌入时给出降级提示
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// FallbackHTML 前端未嵌入时的降级提示页。
const FallbackHTML = `<html><body><h2>Model Bridge</h2><p>Frontend not embedded. Build with <code>pnpm build</code> first, then rebuild the Go binary.</p></body></html>`

// MIMEByExt 按扩展名推断 MIME（与 Rust 侧 guess_mime 对齐）。
func MIMEByExt(path string) string {
	idx := strings.LastIndex(path, ".")
	if idx < 0 {
		return "application/octet-stream"
	}
	switch strings.ToLower(path[idx+1:]) {
	case "html":
		return "text/html; charset=utf-8"
	case "js":
		return "application/javascript; charset=utf-8"
	case "css":
		return "text/css; charset=utf-8"
	case "json":
		return "application/json"
	case "png":
		return "image/png"
	case "svg":
		return "image/svg+xml"
	case "ico":
		return "image/x-icon"
	case "woff2":
		return "font/woff2"
	case "woff":
		return "font/woff"
	case "ttf":
		return "font/ttf"
	case "map":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// readFile 从嵌入 FS 读取文件内容。
func readFile(name string) ([]byte, bool) {
	if name == "" || strings.Contains(name, "..") {
		return nil, false
	}
	data, err := fs.ReadFile(distFS, "dist/"+name)
	if err != nil {
		return nil, false
	}
	return data, true
}

// StaticHandler 作为兜底处理器，服务内嵌前端。
func StaticHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	lookup := path
	if lookup == "" {
		lookup = "index.html"
	}

	if data, ok := readFile(lookup); ok {
		w.Header().Set("Content-Type", MIMEByExt(lookup))
		_, _ = w.Write(data)
		return
	}

	// 未知 API 路径不得回落到 SPA 外壳
	if strings.HasPrefix(path, "api/") {
		http.NotFound(w, r)
		return
	}

	if data, ok := readFile("index.html"); ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(FallbackHTML))
}

// HasEmbeddedFrontend 报告是否嵌入了前端（用于启动日志与验证）。
func HasEmbeddedFrontend() bool {
	_, ok := readFile("index.html")
	return ok
}

// FileCount 返回嵌入的文件数（用于启动日志与验证）。
func FileCount() int {
	n := 0
	_ = fs.WalkDir(distFS, "dist", func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}
