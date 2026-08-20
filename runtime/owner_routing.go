package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

// OwnerOf 解出 owned spill id 内嵌的属主 host:port（见 spill_id.go）。
// ok=false 表示旧式/无归属 id。中性命名，供通用 owner 路由复用。
func OwnerOf(id string) (hostPort string, ok bool) { return splitOwner(id) }

var (
	ownerRoutedMu sync.RWMutex
	ownerRouted   = map[string]string{} // toolName -> routing param name
)

// RegisterOwnerRouted 声明「工具 toolName 按参数 paramName（owned id）路由」。
// 应在 init/启动期调用，早于开始处理请求。
func RegisterOwnerRouted(toolName, paramName string) {
	ownerRoutedMu.Lock()
	ownerRouted[toolName] = paramName
	ownerRoutedMu.Unlock()
}

func ownerRoutedParam(toolName string) (string, bool) {
	ownerRoutedMu.RLock()
	defer ownerRoutedMu.RUnlock()
	p, ok := ownerRouted[toolName]
	return p, ok
}

const ownerForwardedHeader = "X-Mcp-Owner-Forwarded"

// WithOwnerRouting 包裹上游 MCP handler：tools/call 若其声明的路由参数是归属兄弟副本的
// owned id，则把整条调用反代到该副本 /mcp（属主本地执行）；其余交 next 本地处理。
func WithOwnerRouting(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get(ownerForwardedHeader) != "" {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))

		owner, ok := ownerFromCall(body)
		if !ok || owner == SpillSelfHostPort() || !SpillPeerAllowed(owner) {
			next.ServeHTTP(w, r)
			return
		}
		proxyToOwner(w, r, owner, body)
	})
}

func ownerFromCall(body []byte) (string, bool) {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil || msg.Method != "tools/call" {
		return "", false
	}
	param, ok := ownerRoutedParam(msg.Params.Name)
	if !ok {
		return "", false
	}
	var args map[string]any
	if err := json.Unmarshal(msg.Params.Arguments, &args); err != nil {
		return "", false
	}
	id, _ := args[param].(string)
	if id == "" {
		return "", false
	}
	return OwnerOf(id)
}

func proxyToOwner(w http.ResponseWriter, r *http.Request, owner string, body []byte) {
	target := &url.URL{Scheme: "http", Host: owner}
	rp := &httputil.ReverseProxy{
		FlushInterval: -1, // 立即冲刷，支持 text/event-stream
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.URL.Path = "/mcp"
			req.Header.Set(ownerForwardedHeader, "1")
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		},
	}
	rp.ServeHTTP(w, r)
}
