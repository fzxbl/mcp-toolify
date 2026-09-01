package runtime

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Chain 把中间件按注册顺序包成洋葱：先注册的在最外层，返回时最后被唤醒。
// nil 元素被跳过，允许调用方按固定位置传 nil 占位。
//
// panic 不在基座兜底：中间件里的 panic 会原样穿透，交由 MCP SDK / HTTP 层处理。
func Chain(mws []Middleware, final Handler) Handler {
	h := final
	for i := len(mws) - 1; i >= 0; i-- {
		if mws[i] != nil {
			h = mws[i](h)
		}
	}
	return h
}

// asMCPMiddleware 把基座中间件与插件链适配成 MCP SDK 的 receiving middleware：
// 把 MCP 请求投影成 Call，跑完链后把 Result 投影回 mcp.Result。
// 由基座在组装 server 时通过 AddReceivingMiddleware 挂载。
//
// mws 是「基座中间件 + 插件链」的完整顺序（基座的 token 准入固定在最前）。
// 曾经把两组分开传，唯一的理由是重放路径只跑基座那一组；重放机制删掉之后
// 只剩一条请求路径，合成一个 slice 即可。
func asMCPMiddleware(mws []Middleware) mcp.Middleware {
	// 全链只在挂载时拼一次：每请求 append 一遍是白白的分配，且会写到调用方的底层数组上。
	all := make([]Middleware, 0, len(mws))
	all = append(all, mws...)
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			// final 是唯一的执行终点，以 *Call 为唯一输入：调用下游之前把 c.Tool / c.Args
			// 回写进合成的 CallToolParamsRaw，因此插件改写参数会真的生效。
			final := terminus(next, method, req)
			c := newCall(ctx, method, req)
			res, err := Chain(all, final)(ctx, c)
			// 链上 err 直接上抛成协议级错误。插件表达「拒绝」应返回 DenyResult（IsError），
			// 只有真异常才 return err。
			if err != nil {
				return nil, err
			}
			return unwrapResult(res), nil
		}
	}
}

// methodToolsCall 是工具调用的 MCP 方法名。
const methodToolsCall = "tools/call"

// terminus 把「执行一次 MCP 请求」包成以 *Call 为唯一输入的执行器。
//
// tools/call 会按 Call 合成一个新的 CallToolRequest（原请求的 Session / Extra 原样带过去，
// 只换 Params 里的工具名与参数），因此插件改写工具名与参数会真的生效。
// 合成而不是就地改 req.Params：那是上游 SDK 持有的对象，就地改写等于改别人手里的数据。
func terminus(next mcp.MethodHandler, method string, req mcp.Request) Handler {
	return func(ctx context.Context, c *Call) (*Result, error) {
		out := req
		if ctr, ok := callToolRequest(req); ok {
			params := *ctr.Params
			params.Name = c.Tool
			params.Arguments = c.Args
			out = &mcp.CallToolRequest{Session: ctr.Session, Params: &params, Extra: ctr.Extra}
		}
		res, err := next(ctx, method, out)
		if err != nil {
			return nil, err
		}
		return wrapMCPResult(res), nil
	}
}

// callToolRequest 判定 req 是否为带参数的 tools/call 请求。
// Params 为 nil 时返回 false：没有可回写的载体，只能原样透传。
func callToolRequest(req mcp.Request) (*mcp.CallToolRequest, bool) {
	ctr, ok := req.(*mcp.CallToolRequest)
	if !ok || ctr == nil || ctr.Params == nil {
		return nil, false
	}
	return ctr, true
}

// newCall 把一次 MCP 请求投影成 Call。tools/call 之外的方法只填公共字段，
// Tool/Args/Labels 留空。
func newCall(ctx context.Context, method string, req mcp.Request) *Call {
	c := &Call{
		Method:  method,
		LogID:   LogIDFromContext(ctx),
		Headers: HeadersFromContext(ctx),
		Subject: subjectOrEmpty(ctx),
		Tools:   Tools,
	}
	// 只覆盖「无请求 / 未投影」这一种场景（调用方显式传 nil 接口），
	// 不是完整的 nil 防御：包在接口里的类型化 nil 仍会走下面的 GetParams。
	if req == nil {
		return c
	}
	if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && p != nil {
		c.Tool = p.Name
		c.Args = p.Arguments
		if info, ok := LookupTool(p.Name); ok {
			c.Labels = info.AllLabels()
		}
	}
	return c
}

// wrapMCPResult 把 SDK 结果投影成 Result：tools/call、tools/list 走具名字段，
// 其余方法（initialize、ping 等）原样放进 raw 直通。
func wrapMCPResult(res mcp.Result) *Result {
	switch r := res.(type) {
	case *mcp.CallToolResult:
		return &Result{Tool: r}
	case *mcp.ListToolsResult:
		return ListResult(r)
	}
	return rawResult(res)
}

// unwrapResult 把 Result 投影回 SDK 结果，读取优先级 Tool → List → raw。
// 三者全为 nil（如插件返回 &Result{}）时返回 nil 接口，等同于没有结果。
func unwrapResult(res *Result) mcp.Result {
	switch {
	case res == nil:
		return nil
	case res.Tool != nil:
		return res.Tool
	case res.List != nil:
		return res.List
	}
	return res.rawOf()
}

// subjectOrEmpty 取 ctx 里认证阶段填好的主体；缺失时返回非 nil 空 Subject。
// 空 Subject 的 Token 为空串，在 byName 里查不到准入规则，等同于全拒（fail-closed）；
// 返回非 nil 是因为插件会直接读 c.Subject.ID/Labels。
func subjectOrEmpty(ctx context.Context) *Subject {
	if s := SubjectFromContext(ctx); s != nil {
		return s
	}
	return &Subject{}
}

type headersKey struct{}

// credentialHeaders 是绝不进请求头快照的凭据类头名（http.Header 规范化大小写）。
//
// Call.Headers 是给插件看的：audit 插件按配置采集请求头时，一旦把 Authorization
// 带上就等于把 token 原文写进日志——tokenauthz 全程避免 token 落地，这里不能开一个口子。
// 认证发生在 HTTP 层、读的是真实的 *http.Request，剔除快照不影响鉴权。
//
// Set-Cookie 也在列：它按规范是响应头，但请求里出现它完全是合法的 HTTP，
// 而反代/网关回填这个头的情况真实存在。这层剔除是所有插件的共同底线，
// 不能只靠某一个插件（如 audit 的 headers 校验）在自己那一侧拦。
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie"}

// WithHeaders 把请求头快照注入 ctx，由 HTTP 层（net/http 中间件）在进入 MCP
// handler 前调用，是 HeadersFromContext / Call.Headers 的唯一数据来源。
// 必须 Clone：Call.Headers 承诺是快照，若直接持有 r.Header，插件写它就会改到活的
// *http.Request，与上游 net/http 中间件共享状态。
// Clone 之后剔除凭据类头（见 credentialHeaders）。
func WithHeaders(ctx context.Context, h http.Header) context.Context {
	snapshot := h.Clone()
	for _, name := range credentialHeaders {
		snapshot.Del(name)
	}
	return context.WithValue(ctx, headersKey{}, snapshot)
}

// HeadersFromContext 取请求头快照；缺失或注入的是 nil Header 时返回非 nil 的空
// Header（h.Clone() 对 nil 返回 nil），保证插件既能 Get 也能 Set 而不 panic。
func HeadersFromContext(ctx context.Context) http.Header {
	if h, ok := ctx.Value(headersKey{}).(http.Header); ok && h != nil {
		return h
	}
	return http.Header{}
}

// HTTPHeaders 是把请求头快照注入 ctx 的 net/http 中间件，装在 MCP handler 之外。
// 插件读 Call.Headers（如按配置采集审计头）全靠它。
func HTTPHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(WithHeaders(r.Context(), r.Header)))
	})
}
