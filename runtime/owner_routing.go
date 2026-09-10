package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
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

// anyOwnerRoutedTool 是否有任何工具声明了按参数路由。没有的话 MCP 那条包裹就不必为了
// 找工具名去读 body——那是每个 POST 都要付的一次全量拷贝。
func anyOwnerRoutedTool() bool {
	ownerRoutedMu.RLock()
	defer ownerRoutedMu.RUnlock()
	return len(ownerRouted) > 0
}

// OwnerRoutedParamForTest 返回某工具声明的 owner 路由参数名；ok=false 表示未声明。
// **仅测试使用**，与 OwnerRoutedPathRegisteredForTest 成对：两种提取形态都要能被插件的
// 用例断言「我真的登记了、且登记的名字与工具 schema 里的 json 名一致」。漏登记或改名
// 没同步，在单副本部署下完全看不出来。
func OwnerRoutedParamForTest(toolName string) (paramName string, ok bool) {
	return ownerRoutedParam(toolName)
}

// ResetOwnerRoutedForTest 清空按参数路由的声明表，**仅测试使用，禁止在运行期调用**：
// 表是进程级的，运行期清空会让多副本部署静默退化成「结果/任务取不回来」。
// 用途是覆盖「一个工具都没登记」这条分支（那时 MCP 包裹不该读 body）。
func ResetOwnerRoutedForTest() {
	ownerRoutedMu.Lock()
	ownerRouted = map[string]string{}
	ownerRoutedMu.Unlock()
}

const ownerForwardedHeader = "X-Mcp-Owner-Forwarded"

// PathOwnerExtractor 从一次 HTTP 请求里取出 owned id；返回空串表示「这条请求不参与
// owner 路由」（本副本按普通请求处理）——也用它表达「本地已经能回答」，spill 的下载端点
// 就靠这个做本地优先。
//
// 允许读 body（confirm 的回执 id 就藏在加密载荷里），但读了必须自己兜住两件事：
//
//  1. 限长：body 大小由外部决定，裸 io.ReadAll 是一个免费的内存放大器；
//  2. 复位：读完要把 r.Body 换回可再读的 reader
//     （r.Body = io.NopCloser(bytes.NewReader(b))）。
//
// 基座在路径形态下刻意不缓冲 body（见 WithPathOwnerRouting），漏了复位，属主副本收到的
// 就是空请求体，而本地单副本一切正常——这类差异只在多副本部署下暴露。
type PathOwnerExtractor func(r *http.Request) string

var (
	ownerRoutedPathMu sync.RWMutex
	// **插件自己的 pattern** -> 提取器。存 pattern 而不是加好前缀的实际路径：前缀是
	// 进程级状态，登记可能早于宿主 Mount 声明前缀（比如插件在 init 里登记），
	// 那一刻算出来的实际路径是错的，且只在多副本部署下暴露。改成匹配时再补前缀，
	// 登记与前缀的先后顺序就不再是隐性依赖。
	ownerRoutedPaths = map[string]PathOwnerExtractor{}
)

// RegisterOwnerRoutedRoute 声明「插件自己的 pattern 下的请求，按提取器给出的 owned id
// 路由到属主副本」。应在启动期调用（插件的 Install 里），早于开始处理请求。
//
// 匹配的是**实际请求路径**：pattern 在匹配时补上宿主给出的挂载前缀，挂在根上时补的是
// 空串，于是「有前缀」与「挂在根上」是同一条代码路径——因此只需要这一个登记入口，
// 不必再有一个「按裸绝对路径登记」的入口（它在有前缀的部署里永远匹配不上）。
//
// 存在的理由：有状态插件的回调入口不一定是 tools/call。带外人工确认走的是
// IM → 推送服务 → HTTP 回调，而挂起的请求只存在于发起副本；没有这条路，回调被负载
// 均衡打到别的副本就是一次静默的「确认丢失」，而单副本部署完全看不出来。
func RegisterOwnerRoutedRoute(pattern string, fn PathOwnerExtractor) {
	if pattern == "" || fn == nil {
		panic("runtime.RegisterOwnerRoutedRoute 需要非空 pattern 与非空提取器")
	}
	ownerRoutedPathMu.Lock()
	ownerRoutedPaths[pattern] = fn
	ownerRoutedPathMu.Unlock()
}

// OwnerRoutedPathRegisteredForTest 返回某**实际路径**前缀（含挂载前缀）是否
// 已被某条登记覆盖。**仅测试使用**：给包外插件的用例一条断言「我的
// RegisterOwnerRoutedRoute 真的调到了，且它换算出来的路径正是宿主挂载的那条」——
// 漏调在单副本部署下完全看不出来，多副本上线才会变成「点了确认没反应 / 结果取不回来」。
func OwnerRoutedPathRegisteredForTest(path string) bool {
	ownerRoutedPathMu.RLock()
	defer ownerRoutedPathMu.RUnlock()
	for pattern := range ownerRoutedPaths {
		if RoutePath(pattern) == path {
			return true
		}
	}
	return false
}

// ownerRoutedPatterns 返回已登记路径 owner 路由的插件 pattern 列表（未加前缀）。
// 供 Mount 在启动期比对「登记了却没有对应路由」的不一致。
func ownerRoutedPatterns() []string {
	ownerRoutedPathMu.RLock()
	defer ownerRoutedPathMu.RUnlock()
	out := make([]string, 0, len(ownerRoutedPaths))
	for pattern := range ownerRoutedPaths {
		out = append(out, pattern)
	}
	return out
}

// ownerFromPath 按注册的 pattern（补上当前前缀后）找提取器并取 owner。最长前缀优先：
// 允许 /a/ 与 /a/b/ 各注册一个而不互相吞掉。
func ownerFromPath(r *http.Request) (string, bool) {
	prefix := currentRoutePrefix() // 取一次，别在循环里反复加锁
	ownerRoutedPathMu.RLock()
	best, bestFn := "", PathOwnerExtractor(nil)
	for pattern, fn := range ownerRoutedPaths {
		p := prefix + pattern
		if strings.HasPrefix(r.URL.Path, p) && len(p) > len(best) {
			best, bestFn = p, fn
		}
	}
	ownerRoutedPathMu.RUnlock()
	if bestFn == nil {
		return "", false
	}
	// 提取器是使用方的代码，出锁再调：持读锁调外部函数，一次 RegisterOwnerRoutedRoute
	// 就能与它互等（提取器里再注册一次即自锁）。
	id := bestFn(r)
	if id == "" {
		return "", false
	}
	return OwnerOf(id)
}

// ResetOwnerRoutedPathsForTest 清空路径路由声明表，**仅测试使用，禁止在运行期调用**：
// 表是进程级的，运行期清空会让多副本部署静默退化成「回调丢失 / 结果取不回来」。
func ResetOwnerRoutedPathsForTest() {
	ownerRoutedPathMu.Lock()
	ownerRoutedPaths = map[string]PathOwnerExtractor{}
	ownerRoutedPathMu.Unlock()
}

// withMCPOwnerRouting 包裹上游 **MCP handler**：若请求携带的 owned id 归属兄弟副本，
// 则把整条请求单跳反代到该副本的**同一路径**（属主本地执行）；其余交 next 本地处理。
//
// 未导出：唯一调用方是 Registry.Handlers（宿主拿到的 MCP handler 已经包好了），
// 宿主与插件都没有自己装它的场合。
//
// 它比 WithPathOwnerRouting 多管一种提取形态：
//   - tools/call 参数（RegisterOwnerRouted）：要读 body 解析 JSON-RPC，只对 POST 成立；
//   - HTTP 路径（RegisterOwnerRoutedRoute）：不读 body，因此 GET/DELETE 这类回调同样适用。
//
// 路径形态在这里也要判：宿主常把 MCP handler 挂成通配（如 /mcp*），插件路由落在同一前缀
// 之下时可能先经过它。
func withMCPOwnerRouting(next http.Handler) http.Handler {
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
		// 没有任何工具声明按参数路由时不读 body：解析 JSON-RPC 只为拿到工具名，
		// 而读 body 是每个 POST 都要付的一次全量拷贝（工具入参可以很大）。
		if !anyOwnerRoutedTool() {
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

// WithPathOwnerRouting 做**路径形态**的 owner 路由，基座用它包裹插件注册的 HTTP 路由
// （见 Registry.Routes）；插件的用例要复现「生产形态的入口」时也用它。
//
// 为什么不和 MCP handler 那条合成一个：那条对 POST 会把整个 body 读进内存来解析
// JSON-RPC，而插件路由的 POST body 形状与大小都不由基座决定（上传类路由完全合法），
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
// 路径保持 r.URL.Path 原样：peer 是同一份二进制、挂载前缀必然相同，因此宿主把 MCP
// handler 挂在任何前缀下都不会把请求反代到不存在的路径。
//
// 转发失败一律回 404、只把属主地址写进服务端日志：这条路任何持合法 token 的调用方都能
// 触发，回显属主等于把内网拓扑告诉调用方；而对调用方来说「属主不可达」与「资源不存在」
// 本来就是同一件事（默认 ErrorHandler 会回 502 并暴露更多细节）。
//
// 每次转发都记一行：跨副本转发在本副本原本不留任何痕迹，「这个回调/下载到底去哪了」
// 只能去属主侧翻日志。带上 logid，好和属主那边的同一条请求对起来。
func proxyToOwner(w http.ResponseWriter, r *http.Request, owner string, body []byte) {
	log.Printf("[mcp] owner routing: %s %s → 属主副本 %s logid=%s",
		r.Method, r.URL.Path, owner, LogIDFromContext(r.Context()))
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
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[mcp] owner routing warning: 转发 %s 到属主副本 %s 失败: %v",
				r.URL.Path, owner, err)
			http.NotFound(w, r)
		},
	}
	rp.ServeHTTP(w, r)
}
