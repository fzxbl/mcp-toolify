package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
)

// OwnerOf 的实现见 ownerid.go：从 id 内嵌的 owner-seg 解出属主 host:port。

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

// OwnerRoutedParam 返回某工具声明的 owner 路由参数名；ok=false 表示未声明。
// 供插件与宿主自检「我的 RegisterOwnerRouted 真的生效了」——漏调它在单副本部署下
// 完全看不出来，多副本上线才会变成「回执/资源找不到」。
func OwnerRoutedParam(toolName string) (paramName string, ok bool) {
	return ownerRoutedParam(toolName)
}

// ResetOwnerRoutedForTest 清空 owner 路由声明表，**仅测试使用，禁止在运行期调用**：
// 表是进程级的，运行期清空会让多副本部署静默退化成「回执/资源找不到」。
// 与 ResetOwnerRoutedPathsForTest 成对存在——两张启动期注册表都要能被包外插件的测试
// 复位，少一个会让「探针跑一遍看有没有残留」这类自检得出相反结论。
func ResetOwnerRoutedForTest() {
	ownerRoutedMu.Lock()
	ownerRouted = map[string]string{}
	ownerRoutedMu.Unlock()
}

const ownerForwardedHeader = "X-Mcp-Owner-Forwarded"

// PathOwnerExtractor 从一次 HTTP 请求里取出 owned id；返回空串表示「这条请求不参与
// owner 路由」（本副本按普通请求处理）。
type PathOwnerExtractor func(r *http.Request) string

var (
	ownerRoutedPathMu sync.RWMutex
	// prefix -> 提取器。用前缀而不是精确路径：owned id 在路径里，精确匹配匹不上。
	ownerRoutedPaths = map[string]PathOwnerExtractor{}
)

// RegisterOwnerRoutedPath 声明「前缀 prefix 下的请求按提取器给出的 owned id 路由」。
// 应在启动期调用（插件的 Install 里），早于开始处理请求。
//
// 存在的理由：有状态插件的回调入口不一定是 tools/call。带外人工确认走的是
// IM → 推送服务 → HTTP 回调，而挂起的请求只存在于发起副本；没有这条路，回调被负载
// 均衡打到别的副本就是一次静默的「确认丢失」，而单副本部署完全看不出来。
func RegisterOwnerRoutedPath(prefix string, fn PathOwnerExtractor) {
	if prefix == "" || fn == nil {
		panic("runtime.RegisterOwnerRoutedPath 需要非空前缀与非空提取器")
	}
	ownerRoutedPathMu.Lock()
	ownerRoutedPaths[prefix] = fn
	ownerRoutedPathMu.Unlock()
}

// OwnerRoutedPathRegistered 返回某前缀是否声明过路径 owner 路由，供插件与宿主自检
// 「我的 RegisterOwnerRoutedPath 真的调到了」——漏调它在单副本部署下完全看不出来。
func OwnerRoutedPathRegistered(prefix string) bool {
	ownerRoutedPathMu.RLock()
	defer ownerRoutedPathMu.RUnlock()
	_, ok := ownerRoutedPaths[prefix]
	return ok
}

// ownerFromPath 按注册的前缀找提取器并取 owner。最长前缀优先：允许 /confirm/ 与
// /confirm/admin/ 各注册一个而不互相吞掉。
func ownerFromPath(r *http.Request) (string, bool) {
	ownerRoutedPathMu.RLock()
	best, bestFn := "", PathOwnerExtractor(nil)
	for prefix, fn := range ownerRoutedPaths {
		if strings.HasPrefix(r.URL.Path, prefix) && len(prefix) > len(best) {
			best, bestFn = prefix, fn
		}
	}
	ownerRoutedPathMu.RUnlock()
	if bestFn == nil {
		return "", false
	}
	// 提取器是使用方的代码，出锁再调：持读锁调外部函数，一次 RegisterOwnerRoutedPath
	// 就能与它互等（提取器里再注册一次即自锁）。
	id := bestFn(r)
	if id == "" {
		return "", false
	}
	return OwnerOf(id)
}

// ResetOwnerRoutedPathsForTest 清空路径路由声明表，**仅测试使用**。
// 理由与 ResetOwnerRoutedForTest 相同：表是进程级的，运行期清空会让多副本部署
// 静默退化成「回调丢失」。
func ResetOwnerRoutedPathsForTest() {
	ownerRoutedPathMu.Lock()
	ownerRoutedPaths = map[string]PathOwnerExtractor{}
	ownerRoutedPathMu.Unlock()
}

// WithOwnerRouting 包裹上游 MCP handler：若请求携带的 owned id 归属兄弟副本，则把整条请求
// 单跳反代到该副本的**同一路径**（属主本地执行）；其余交 next 本地处理。
//
// 两种 owner 提取形态：
//   - HTTP 路径（RegisterOwnerRoutedPath）：不读 body，因此 GET/DELETE 这类回调同样适用；
//   - tools/call 参数（RegisterOwnerRouted）：要读 body 解析 JSON-RPC，只对 POST 成立。
func WithOwnerRouting(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ownerForwardedHeader) != "" {
			next.ServeHTTP(w, r)
			return
		}
		// 路径路由先判：它不需要读 body，对 GET/DELETE 这类回调同样成立。
		if routedByPath(w, r) {
			return
		}
		if r.Method != http.MethodPost {
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
		if !ok || owner == SelfHostPort() || !PeerAllowed(owner) {
			next.ServeHTTP(w, r)
			return
		}
		proxyToOwner(w, r, owner, body)
	})
}

// WithPathOwnerRouting 只做**路径形态**的 owner 路由，基座用它包裹插件注册的 HTTP 路由
// （见 Registry.Routes）。
//
// 与 WithOwnerRouting 分开的理由：后者对 POST 会把整个 body 读进内存来解析 JSON-RPC，
// 而插件路由的 POST body 形状与大小都不由基座决定（上传类路由完全合法），
// 无条件缓冲它是一次白送的内存放大器。插件路由要跨副本，就把 id 放进路径。
func WithPathOwnerRouting(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ownerForwardedHeader) != "" || !routedByPath(w, r) {
			next.ServeHTTP(w, r)
		}
	})
}

// routedByPath 在请求的路径里带着「归属某个已知兄弟副本」的 owned id 时把它反代走，
// 并返回 true（本副本不再处理）。三条判定原样保留：本机判定、peer 白名单、防环头。
func routedByPath(w http.ResponseWriter, r *http.Request) bool {
	owner, ok := ownerFromPath(r)
	if !ok || owner == SelfHostPort() || !PeerAllowed(owner) {
		return false
	}
	proxyToOwner(w, r, owner, nil)
	return true
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

// proxyToOwner 把请求单跳反代到属主副本。
//
// body 为 nil 表示「不重放 body」（路径路由的场景：body 没被读过，ReverseProxy 直接
// 转发 r.Body 即可）；非 nil 表示 tools/call 场景——那里 body 已经被读完用于解析 owner，
// 必须显式塞回去，否则属主收到的是空请求体。
//
// 路径保持 r.URL.Path 原样：peer 是同一份二进制，挂载前缀必然相同。此前硬编码 "/mcp"
// 会让「宿主把 MCP handler 挂在别的前缀下」的部署把请求反代到一个不存在的路径。
func proxyToOwner(w http.ResponseWriter, r *http.Request, owner string, body []byte) {
	target := &url.URL{Scheme: "http", Host: owner}
	rp := &httputil.ReverseProxy{
		FlushInterval: -1, // 立即冲刷，支持 text/event-stream
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.Header.Set(ownerForwardedHeader, "1")
			if body != nil {
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
			}
		},
	}
	rp.ServeHTTP(w, r)
}
