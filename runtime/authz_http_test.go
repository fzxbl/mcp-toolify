package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// emptyIn 是测试工具的空入参。
type emptyIn struct{}

// headerRT 在每个 HTTP 请求上强制注入固定 header，
// 用于验证 stateless 模式下逐次重读身份 header。
type headerRT struct {
	base http.RoundTripper
	h    map[string]string
}

func (rt headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	for k, v := range rt.h {
		if v != "" {
			r.Header.Set(k, v)
		}
	}
	return rt.base.RoundTrip(r)
}

func connect(ctx context.Context, url string, headers map[string]string) (*mcp.ClientSession, error) {
	httpClient := &http.Client{Transport: headerRT{http.DefaultTransport, headers}}
	tr := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: httpClient, DisableStandaloneSSE: true}
	cli := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	return cli.Connect(ctx, tr, nil)
}

// TestAuthzHTTP_EndToEnd 通过真实 HTTP（httptest）验证 stateless 模式下
// 连接级能力鉴权 + 调用级风险鉴权按每请求身份生效。
func TestAuthzHTTP_EndToEnd(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)

	okHandler := func(ctx context.Context, req *mcp.CallToolRequest, in emptyIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	}
	mcp.AddTool(s, &mcp.Tool{Name: "t.hi"}, okHandler)
	mcp.AddTool(s, &mcp.Tool{Name: "t.lo"}, okHandler)
	RegisterMeta(ToolMeta{Name: "t.hi", Capability: ReadWrite, Risk: RiskHigh})
	RegisterMeta(ToolMeta{Name: "t.lo", Capability: ReadOnly, Risk: RiskNone})

	authz := NewAuthz(AuthzConfig{
		Tokens: []TokenEntry{
			{Token: "ro-1", Name: "readonly-agent", Applicant: "alice", Read: "high"},
			{Token: "rw-1", Name: "readwrite-agent", Applicant: "alice", Read: "high", Write: "high"},
		},
		RiskAllowlist: map[string][]string{"high": {"zhangsan"}},
	})

	handler := NewAuthzHandler(s, authz)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// call 建立一条新连接并调用一个工具；返回 (session, callResult, error)。
	call := func(t *testing.T, headers map[string]string, tool string) (*mcp.ClientSession, *mcp.CallToolResult, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sess, err := connect(ctx, srv.URL, headers)
		if err != nil {
			return nil, nil, err
		}
		res, callErr := sess.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{}})
		return sess, res, callErr
	}

	t.Run("no token rejected", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sess, err := connect(ctx, srv.URL, map[string]string{})
		if err == nil {
			// 若 Connect 未直接报错，则后续 CallTool 必须失败——不允许无凭证放行。
			defer sess.Close()
			res, callErr := sess.CallTool(ctx, &mcp.CallToolParams{Name: "t.lo", Arguments: map[string]any{}})
			if callErr == nil && (res == nil || !res.IsError) {
				t.Fatalf("expected unauthenticated client to be blocked, but connect+call succeeded")
			}
			return
		}
		// 预期路径：initialize POST 收到 401，Connect 返回 error。
	})

	t.Run("readonly calling write tool denied", func(t *testing.T) {
		sess, res, err := call(t, map[string]string{"Authorization": "Bearer ro-1"}, "t.hi")
		if err != nil {
			t.Fatalf("connect/call error: %v", err)
		}
		defer sess.Close()
		if !res.IsError {
			t.Fatalf("expected IsError for readonly token calling write tool t.hi")
		}
	})

	t.Run("readwrite zhangsan high-risk allowed", func(t *testing.T) {
		sess, res, err := call(t, map[string]string{"Authorization": "Bearer rw-1", "X-MCP-User": "zhangsan"}, "t.hi")
		if err != nil {
			t.Fatalf("connect/call error: %v", err)
		}
		defer sess.Close()
		if res.IsError {
			t.Fatalf("expected success for rw-1 + zhangsan calling high-risk t.hi, got IsError")
		}
	})

	t.Run("readwrite lisi high-risk denied", func(t *testing.T) {
		sess, res, err := call(t, map[string]string{"Authorization": "Bearer rw-1", "X-MCP-User": "lisi"}, "t.hi")
		if err != nil {
			t.Fatalf("connect/call error: %v", err)
		}
		defer sess.Close()
		if !res.IsError {
			t.Fatalf("expected IsError for rw-1 + lisi calling high-risk t.hi")
		}
	})

	t.Run("readonly none-risk allowed", func(t *testing.T) {
		sess, res, err := call(t, map[string]string{"Authorization": "Bearer ro-1"}, "t.lo")
		if err != nil {
			t.Fatalf("connect/call error: %v", err)
		}
		defer sess.Close()
		if res.IsError {
			t.Fatalf("expected success for ro-1 calling none-risk t.lo, got IsError")
		}
	})
}

// TestHTTPAuthContext_InjectsTokenName 验证 HTTPAuthContext 把 token 用途名
// （审计日志用）与调用人身份一起注入请求 ctx。
func TestHTTPAuthContext_InjectsTokenName(t *testing.T) {
	authz := NewAuthz(AuthzConfig{Tokens: []TokenEntry{
		{Token: "ro-1", Name: "readonly-agent", Applicant: "alice", Read: "high"},
	}})

	var got AuthContext
	var ok bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = AuthFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(HTTPAuthContext(inner, authz))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer ro-1")
	req.Header.Set("X-MCP-User", "zhangsan")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !ok {
		t.Fatalf("auth context missing in ctx")
	}
	if got.TokenName != "readonly-agent" {
		t.Errorf("TokenName = %q, want readonly-agent", got.TokenName)
	}
	if got.Identity != "zhangsan" {
		t.Errorf("Identity = %q", got.Identity)
	}
}
