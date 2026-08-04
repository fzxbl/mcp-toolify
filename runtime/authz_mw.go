package runtime

import (
	"context"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AuthContext 是从 HTTP header 解析出的连接/调用鉴权上下文。
type AuthContext struct {
	Caps      TokenCaps // 由 Authorization token 决定的读/写风险上限
	Identity  string    // 由 X-MCP-User 转发；可能为空
	TokenName string    // token 用途名（配置里的 name），用于审计日志
}

type authCtxKey struct{}

// WithAuthContext 把鉴权上下文注入 ctx。
func WithAuthContext(ctx context.Context, ac AuthContext) context.Context {
	return context.WithValue(ctx, authCtxKey{}, ac)
}

// AuthFromContext 取出鉴权上下文。
func AuthFromContext(ctx context.Context) (AuthContext, bool) {
	ac, ok := ctx.Value(authCtxKey{}).(AuthContext)
	return ac, ok
}

// allowByToken 只按 token 的读/写风险上限判定（连接级，不看人）。
// 用于 tools/list 过滤，以及 tools/call 的第一层。
func allowByToken(caps TokenCaps, meta ToolMeta) (bool, string) {
	ceiling := caps.Read
	kind := "读"
	if meta.Capability == ReadWrite {
		ceiling = caps.Write
		kind = "写"
		if !caps.WriteOK {
			return false, "当前 token 不允许写操作"
		}
	} else if !caps.ReadOK {
		return false, "当前 token 不允许读操作"
	}
	if meta.Risk > ceiling {
		return false, fmt.Sprintf("%s 风险 %s 超出当前 token 的%s上限 %s", meta.Name, meta.Risk, kind, ceiling)
	}
	return true, ""
}

// allowCall 是调用级判定：先过 token 层（连接能力），再过按人白名单。
// agent 用同一条连接（同一 token）服务很多人：连接与 tools/list 只认 token，
// 真正调用工具时才按 X-MCP-User 校验具体这个人能否执行该风险等级。
// 未登记元数据的工具（如内置 spill resource）默认放行。
func allowCall(authz *Authz, ac AuthContext, name string) (bool, string) {
	meta, ok := LookupMeta(name)
	if !ok {
		return true, ""
	}
	if ok, reason := allowByToken(ac.Caps, meta); !ok {
		return false, reason
	}
	if !authz.CanRun(ac.Identity, meta.Risk) {
		id := ac.Identity
		if id == "" {
			id = "<empty>"
		}
		return false, fmt.Sprintf("执行 %s 需要 %s 风险准入，当前身份 %s 无权限", name, meta.Risk, id)
	}
	return true, ""
}

// listVisible 是 tools/list 过滤判定：只看 token 层（连接级），不看人。
func listVisible(ac AuthContext, name string) bool {
	meta, ok := LookupMeta(name)
	if !ok {
		return true
	}
	ok2, _ := allowByToken(ac.Caps, meta)
	return ok2
}

// AuthzMiddleware 在 tools/call 做 token + 按人的鉴权，tools/list 只按 token 过滤。
func AuthzMiddleware(authz *Authz) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			ac, _ := AuthFromContext(ctx) // 缺失时零值：读写均不允许 + 空身份

			if method == "tools/call" {
				if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && p != nil {
					if ok, reason := allowCall(authz, ac, p.Name); !ok {
						return ToolError(fmt.Errorf("permission denied: %s", reason)), nil
					}
				}
				return next(ctx, method, req)
			}

			res, err := next(ctx, method, req)
			if err == nil && method == "tools/list" {
				if lr, ok := res.(*mcp.ListToolsResult); ok {
					filtered := lr.Tools[:0]
					for _, t := range lr.Tools {
						if listVisible(ac, t.Name) {
							filtered = append(filtered, t)
						}
					}
					lr.Tools = filtered
				}
			}
			return res, err
		}
	}
}

// NewAuthzHandler 构造启用连接级鉴权的 stateless HTTP handler。
// Stateless：每个请求独立成会话，逐次重读 Authorization / X-MCP-User，
// 实现调用级身份透传（多人共用一条 agent 连接的场景）。供 runHTTP 与测试复用。
func NewAuthzHandler(s *mcp.Server, authz *Authz) http.Handler {
	s.AddReceivingMiddleware(AuthzMiddleware(authz))
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	return HTTPAuthContext(mcpHandler, authz)
}

// HTTPAuthContext 包一层 http.Handler：解析 Authorization（能力 token）与
// X-MCP-User（人身份），注入请求 ctx。token 缺失/无效直接 401。
func HTTPAuthContext(next http.Handler, authz *Authz) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r.Header.Get("Authorization"))
		caps, ok := authz.CapsOf(token)
		if !ok {
			http.Error(w, "invalid or missing token", http.StatusUnauthorized)
			return
		}
		ctx := WithAuthContext(r.Context(), AuthContext{
			Caps:      caps,
			Identity:  r.Header.Get("X-MCP-User"),
			TokenName: caps.Name,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bearerToken(h string) string {
	const p = "Bearer "
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return h
}
