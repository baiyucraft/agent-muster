// Package httpapi 提供中心服务的 HTTP 接口。
package httpapi

import (
	"io"
	"net/http"
)

// NewHandler 注册中心服务的路由。健康接口仅表示 HTTP 进程可响应请求。
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{\"status\":\"ok\"}\n")
	})
	return mux
}
