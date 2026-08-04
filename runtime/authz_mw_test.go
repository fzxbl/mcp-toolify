package runtime

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAuthContextRoundTrip(t *testing.T) {
	ctx := WithAuthContext(context.Background(), AuthContext{Caps: rwCaps, Identity: "zhangsan"})
	ac, ok := AuthFromContext(ctx)
	if !ok || !ac.Caps.WriteOK || ac.Identity != "zhangsan" {
		t.Errorf("roundtrip = %+v,%v", ac, ok)
	}
	if _, ok := AuthFromContext(context.Background()); ok {
		t.Errorf("empty ctx should have no auth")
	}
}

// TestTokenNameFromCtx 覆盖审计日志取 token 用途名：有值取值，缺失取 "-"。
func TestTokenNameFromCtx(t *testing.T) {
	ctx := WithAuthContext(context.Background(), AuthContext{Caps: rwCaps, TokenName: "readwrite-agent"})
	if got := tokenNameFromCtx(ctx); got != "readwrite-agent" {
		t.Errorf("tokenNameFromCtx = %q, want readwrite-agent", got)
	}
	if got := tokenNameFromCtx(context.Background()); got != "-" {
		t.Errorf("tokenNameFromCtx without auth = %q, want -", got)
	}
	// 有 AuthContext 但 TokenName 为空（如旧配置）同样回退 "-"。
	ctx = WithAuthContext(context.Background(), AuthContext{Caps: rwCaps})
	if got := tokenNameFromCtx(ctx); got != "-" {
		t.Errorf("tokenNameFromCtx with empty name = %q, want -", got)
	}
}

// 测试用的 token 读写上限：
//   - roCaps：只读到 high，不允许写
//   - rwCaps：读写都到 high
//   - rwLowWrite：读到 high，但写只到 low
var (
	roCaps     = TokenCaps{ReadOK: true, Read: RiskHigh}
	rwCaps     = TokenCaps{ReadOK: true, Read: RiskHigh, WriteOK: true, Write: RiskHigh}
	rwLowWrite = TokenCaps{ReadOK: true, Read: RiskHigh, WriteOK: true, Write: RiskLow}
)

func callThrough(t *testing.T, authz *Authz, ac AuthContext, toolName string) (*mcp.CallToolResult, bool) {
	t.Helper()
	var reached bool
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		reached = true
		return &mcp.CallToolResult{}, nil
	}
	mw := AuthzMiddleware(authz)
	ctx := WithAuthContext(context.Background(), ac)
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: toolName}}
	res, err := mw(next)(ctx, "tools/call", req)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	ctr, _ := res.(*mcp.CallToolResult)
	return ctr, reached
}

func TestAuthzMiddleware_Enforcement(t *testing.T) {
	authz := newTestAuthz()
	RegisterMeta(ToolMeta{Name: "t.write_low", Capability: ReadWrite, Risk: RiskLow})
	RegisterMeta(ToolMeta{Name: "t.write_high", Capability: ReadWrite, Risk: RiskHigh})
	RegisterMeta(ToolMeta{Name: "t.read_none", Capability: ReadOnly, Risk: RiskNone})

	if res, reached := callThrough(t, authz, AuthContext{Caps: roCaps}, "t.write_low"); reached || res == nil || !res.IsError {
		t.Errorf("readonly x write should be denied")
	}
	if _, reached := callThrough(t, authz, AuthContext{Caps: rwCaps}, "t.write_low"); !reached {
		t.Errorf("readwrite x low should pass")
	}
	if res, reached := callThrough(t, authz, AuthContext{Caps: rwCaps, Identity: "lisi"}, "t.write_high"); reached || !res.IsError {
		t.Errorf("high x non-allowlisted should be denied")
	}
	if _, reached := callThrough(t, authz, AuthContext{Caps: rwCaps, Identity: "zhangsan"}, "t.write_high"); !reached {
		t.Errorf("high x allowlisted should pass")
	}
	// token 写上限 low，即使身份在 high 白名单，也应被 token 层拦下。
	if res, reached := callThrough(t, authz, AuthContext{Caps: rwLowWrite, Identity: "zhangsan"}, "t.write_high"); reached || !res.IsError {
		t.Errorf("write high should be denied when token write ceiling is low")
	}
	if _, reached := callThrough(t, authz, AuthContext{Caps: roCaps}, "t.read_none"); !reached {
		t.Errorf("none-risk read should pass without identity")
	}
}

func TestAuthzMiddleware_FilterList(t *testing.T) {
	authz := newTestAuthz()
	RegisterMeta(ToolMeta{Name: "t.read_none", Capability: ReadOnly, Risk: RiskNone})
	RegisterMeta(ToolMeta{Name: "t.write_low", Capability: ReadWrite, Risk: RiskLow})
	RegisterMeta(ToolMeta{Name: "t.write_high", Capability: ReadWrite, Risk: RiskHigh})

	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		return &mcp.ListToolsResult{Tools: []*mcp.Tool{
			{Name: "t.read_none"}, {Name: "t.write_low"}, {Name: "t.write_high"},
		}}, nil
	}
	mw := AuthzMiddleware(authz)

	// 只读 token：只看到读工具（写工具被 token 层挡下）。身份无关。
	ctx := WithAuthContext(context.Background(), AuthContext{Caps: roCaps})
	res, _ := mw(next)(ctx, "tools/list", &mcp.ListToolsRequest{})
	got := res.(*mcp.ListToolsResult)
	if len(got.Tools) != 1 || got.Tools[0].Name != "t.read_none" {
		t.Errorf("readonly list = %v", toolNames(got.Tools))
	}

	// 读写 token 且【不带身份】：tools/list 只看 token，应看到全部 3 个。
	ctx = WithAuthContext(context.Background(), AuthContext{Caps: rwCaps})
	res, _ = mw(next)(ctx, "tools/list", &mcp.ListToolsRequest{})
	if len(res.(*mcp.ListToolsResult).Tools) != 3 {
		t.Errorf("rw token should see all 3 regardless of identity, got %v", toolNames(res.(*mcp.ListToolsResult).Tools))
	}

	// 写上限 low 的 token：高危写工具被 token 层挡下，只剩 read_none + write_low。
	ctx = WithAuthContext(context.Background(), AuthContext{Caps: rwLowWrite})
	res, _ = mw(next)(ctx, "tools/list", &mcp.ListToolsRequest{})
	if names := toolNames(res.(*mcp.ListToolsResult).Tools); len(names) != 2 {
		t.Errorf("rwLowWrite should see read_none + write_low, got %v", names)
	}
}

func toolNames(ts []*mcp.Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}
