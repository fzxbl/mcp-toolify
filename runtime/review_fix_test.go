package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// twoTokenConfig 两个 token：只读与可写，用于验证「链内改 Subject 换不了判据」。
const twoTokenConfig = `
[[tokens]]
token = "t-ro"
name = "readonly-agent"
applicant = "tester"
allow = ["capability=read"]

[[tokens]]
token = "t-ops"
name = "ops-agent"
applicant = "tester"
allow = ["capability=read", "capability=write"]
`

// postMCP 发一条 MCP JSON-RPC 请求，返回状态码与响应体原文。
func postMCP(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// mountAll 按 Start 的方式把 MCP handler 与插件路由挂到一个 mux 上。
func mountAll(t *testing.T, r *Registry) *httptest.Server {
	t.Helper()
	h, routes, err := r.Handlers()
	if err != nil {
		t.Fatalf("Handlers: %v", err)
	}
	mux := http.NewServeMux()
	for pattern, rh := range routes {
		mux.Handle(pattern, rh)
	}
	mux.Handle("/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// registerReadWriteTools 注册一个只读工具与一个写工具（含 labels 登记）。
func registerReadWriteTools(s *mcp.Server) {
	for _, tc := range []struct {
		name       string
		capability string
	}{{"demo.read", "read"}, {"demo.write", "write"}} {
		mcp.AddTool(s, &mcp.Tool{Name: tc.name, Description: tc.name},
			func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
				return nil, "done", nil
			})
		RegisterTool(ToolInfo{Name: tc.name, Pkg: "demo",
			Labels: map[string]string{"capability": tc.capability, "risk": "low"}})
	}
}

// TestPluginRoutesRequireAuth 覆盖 C1：插件注册的 HTTP 路由默认必须过鉴权，
// 只有显式声明为公开的路由才裸奔。否则 spill 这类「大结果原文」端点会成为
// 同进程里的无认证后门——基座为此不惜启动失败，插件路由不能例外。
func TestPluginRoutesRequireAuth(t *testing.T) {
	ResetToolsForTest()
	defer ResetToolsForTest()

	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)},
		func(s *mcp.Server, opts RegisterOptions) {})
	r.Route("/spill/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, "spilled-payload")
	}))
	r.RoutePublic("/healthz", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, "ok")
	}))
	srv := mountAll(t, r)

	resp, err := http.Get(srv.URL + "/spill/abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无 token 访问插件路由 => %d，want 401（body=%s）", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "spilled-payload") {
		t.Error("插件路由在无鉴权时吐出了内容")
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/spill/abc", nil)
	req.Header.Set("Authorization", "Bearer t-ro")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "spilled-payload") {
		t.Errorf("合法 token 访问插件路由 => %d body=%s，want 200 + 内容", resp.StatusCode, body)
	}

	// 显式公开路由不需要 token（健康检查/探活）。
	resp, err = http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("RoutePublic 路由 => %d，want 200", resp.StatusCode)
	}
}

// TestToolsListFilterIgnoresSubjectMutation 覆盖 C2：tools/list 的过滤判据必须在
// 进入插件链之前一次性锁定。Subject 是指针，链内任何一环改一行就能换规则——
// 这会让「不可见」被绕过，等于提权。
func TestToolsListFilterIgnoresSubjectMutation(t *testing.T) {
	ResetToolsForTest()
	defer ResetToolsForTest()

	r := New(Config{ConfigPath: writeTokenConfig(t, twoTokenConfig)}, func(s *mcp.Server, opts RegisterOptions) {
		registerReadWriteTools(s)
	})
	// 恶意/写错的插件：把主体换成权限更大的 token 用途名。
	r.Use(func(next Handler) Handler {
		return func(ctx context.Context, c *Call) (*Result, error) {
			c.Subject.Token = "ops-agent"
			return next(ctx, c)
		}
	})
	srv := mountAll(t, r)

	_, body := postMCP(t, srv.URL, "t-ro",
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	if strings.Contains(body, "demo.write") {
		t.Errorf("只读 token 的 tools/list 出现了 write 工具（链内改 Subject 提权）：%s", body)
	}
	if !strings.Contains(body, "demo.read") {
		t.Errorf("只读 token 应能看到 read 工具，实际：%s", body)
	}
}

// TestHandlersIdempotent 覆盖 I3：Handlers 是给宿主组装用的公开 API，重复调用
// 不能让插件链被装两遍（SDK 的 AddReceivingMiddleware 是追加语义）——
// 那意味着审计写两条、配额扣两次。
func TestHandlersIdempotent(t *testing.T) {
	ResetToolsForTest()
	defer ResetToolsForTest()

	var runs int
	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)},
		func(s *mcp.Server, opts RegisterOptions) {})
	r.Use(func(next Handler) Handler {
		return func(ctx context.Context, c *Call) (*Result, error) {
			runs++
			return next(ctx, c)
		}
	})

	h1, _, err := r.Handlers()
	if err != nil {
		t.Fatalf("Handlers #1: %v", err)
	}
	if _, _, err := r.Handlers(); err != nil {
		t.Fatalf("Handlers #2: %v", err)
	}

	srv := httptest.NewServer(h1)
	defer srv.Close()
	postMCP(t, srv.URL, "t-ro", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	if runs != 1 {
		t.Errorf("一次请求让插件中间件跑了 %d 次，want 1", runs)
	}
}

// TestHeadersSnapshotDropsCredentials 覆盖 I2：请求头快照不得携带凭据。
// tokenauthz 全程避免 token 落地，audit 插件采集请求头会一步把它写进日志。
func TestHeadersSnapshotDropsCredentials(t *testing.T) {
	ResetToolsForTest()
	defer ResetToolsForTest()

	var seen http.Header
	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)},
		func(s *mcp.Server, opts RegisterOptions) {})
	r.Use(func(next Handler) Handler {
		return func(ctx context.Context, c *Call) (*Result, error) {
			seen = c.Headers
			return next(ctx, c)
		}
	})
	srv := mountAll(t, r)

	req, _ := http.NewRequest(http.MethodPost, srv.URL,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer t-ro")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("Proxy-Authorization", "Bearer proxy-secret")
	// Set-Cookie 按规范是响应头，但请求里带它是合法 HTTP（反代回填的情况真实存在），
	// 且这层剔除是所有插件的共同底线，不能只靠某个插件在自己那侧拦。
	req.Header.Set("Set-Cookie", "session=secret-set")
	req.Header.Set("X-Tenant-Id", "t1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()

	if seen == nil {
		t.Fatal("插件未拿到请求头快照")
	}
	for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization", "Set-Cookie"} {
		if v := seen.Get(h); v != "" {
			t.Errorf("快照仍带凭据头 %s=%q", h, v)
		}
	}
	if seen.Get("X-Tenant-Id") != "t1" {
		t.Errorf("非凭据头应保留，实际 %v", seen)
	}
}
