package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// traceMW 返回一个只在链上留下进出痕迹的中间件。
func traceMW(trace *[]string, name string) Middleware {
	return func(next Handler) Handler {
		return func(ctx context.Context, c *Call) (*Result, error) {
			*trace = append(*trace, "pre:"+name)
			res, err := next(ctx, c)
			*trace = append(*trace, "post:"+name)
			return res, err
		}
	}
}

func TestChainOrderAndShortCircuit(t *testing.T) {
	var trace []string
	stop := func(next Handler) Handler {
		return func(ctx context.Context, c *Call) (*Result, error) {
			trace = append(trace, "deny")
			return DenyResult(c, "stopper", "nope"), nil
		}
	}
	h := Chain([]Middleware{traceMW(&trace, "a"), stop, traceMW(&trace, "b")},
		func(ctx context.Context, c *Call) (*Result, error) {
			trace = append(trace, "exec")
			return TextResult("ok"), nil
		})
	c := &Call{Method: "tools/call", Tool: "x"}
	res, err := h(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Tool.IsError {
		t.Error("expected denied result")
	}
	want := []string{"pre:a", "deny", "post:a"}
	if len(trace) != len(want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
	for i := range want {
		if trace[i] != want[i] {
			t.Fatalf("trace = %v, want %v", trace, want)
		}
	}
	if c.Meta[MetaDeniedBy] != "stopper" {
		t.Errorf("denied_by = %v", c.Meta[MetaDeniedBy])
	}
}

// TestChainOnionOrder 覆盖不短路时的完整洋葱顺序。
func TestChainOnionOrder(t *testing.T) {
	var trace []string
	h := Chain([]Middleware{traceMW(&trace, "a"), traceMW(&trace, "b")},
		func(ctx context.Context, c *Call) (*Result, error) {
			trace = append(trace, "exec")
			return TextResult("ok"), nil
		})
	if _, err := h(context.Background(), &Call{Method: "tools/call"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"pre:a", "pre:b", "exec", "post:b", "post:a"}
	if len(trace) != len(want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
	for i := range want {
		if trace[i] != want[i] {
			t.Fatalf("trace = %v, want %v", trace, want)
		}
	}
}

// TestChainNilAndEmpty 覆盖 nil 占位中间件与空 slice：均直通 final。
func TestChainNilAndEmpty(t *testing.T) {
	final := func(ctx context.Context, c *Call) (*Result, error) { return TextResult("ok"), nil }

	for _, tc := range []struct {
		name string
		mws  []Middleware
	}{
		{"nil placeholder", []Middleware{nil}},
		{"empty slice", []Middleware{}},
		{"nil slice", nil},
	} {
		res, err := Chain(tc.mws, final)(context.Background(), &Call{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if res == nil || res.Tool == nil {
			t.Fatalf("%s: res = %+v", tc.name, res)
		}
	}
}

// TestNewCallToolsCall 覆盖 tools/call 的投影：工具已注册时带 name/pkg 投影，
// 未注册时 Labels 为 nil。
func TestNewCallToolsCall(t *testing.T) {
	ResetToolsForTest()
	defer ResetToolsForTest()
	RegisterTool(ToolInfo{Name: "orders.create", Pkg: "orders", Labels: map[string]string{"risk": "high"}})

	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
		Name:      "orders.create",
		Arguments: json.RawMessage(`{"app":"a"}`),
	}}
	ctx := WithLogID(WithHeaders(context.Background(), http.Header{"X-Mcp-User": {"zhangsan"}}), "abc123")
	c := newCall(ctx, "tools/call", req)

	if c.Method != "tools/call" || c.Tool != "orders.create" {
		t.Errorf("method/tool = %q/%q", c.Method, c.Tool)
	}
	if string(c.Args) != `{"app":"a"}` {
		t.Errorf("args = %s", c.Args)
	}
	if c.LogID != "abc123" {
		t.Errorf("logid = %q", c.LogID)
	}
	if c.Headers.Get("X-MCP-User") != "zhangsan" {
		t.Errorf("headers = %v", c.Headers)
	}
	if c.Subject == nil {
		t.Fatal("Subject must be non-nil")
	}
	if c.Tools == nil {
		t.Fatal("Tools must be non-nil")
	}
	want := map[string]string{"risk": "high", "name": "orders.create", "pkg": "orders"}
	if len(c.Labels) != len(want) {
		t.Fatalf("labels = %v, want %v", c.Labels, want)
	}
	for k, v := range want {
		if c.Labels[k] != v {
			t.Fatalf("labels = %v, want %v", c.Labels, want)
		}
	}

	// 未注册工具：Labels 为 nil，其余字段照常投影。
	c = newCall(context.Background(), "tools/call",
		&mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "unknown.tool"}})
	if c.Labels != nil {
		t.Errorf("labels for unregistered tool = %v, want nil", c.Labels)
	}
	if c.Tool != "unknown.tool" {
		t.Errorf("tool = %q", c.Tool)
	}
}

// TestNewCallNonToolsCall 覆盖 tools/list 等非 tools/call 方法：不 panic，Tool 为空。
func TestNewCallNonToolsCall(t *testing.T) {
	c := newCall(context.Background(), "tools/list", &mcp.ListToolsRequest{})
	if c.Tool != "" || c.Args != nil || c.Labels != nil {
		t.Errorf("non tools/call projection = %+v", c)
	}
	if c.Method != "tools/list" || c.Subject == nil || c.Headers == nil {
		t.Errorf("call = %+v", c)
	}
	// 参数为 nil 的 tools/call 也不应 panic。
	if c := newCall(context.Background(), "tools/call", &mcp.CallToolRequest{}); c.Tool != "" {
		t.Errorf("nil params tool = %q", c.Tool)
	}
}

// TestResultProjectionRoundTrip 覆盖三类结果的往返无损：tools/call、tools/list
// 与 raw 直通（initialize / ping 这类方法）。
func TestResultProjectionRoundTrip(t *testing.T) {
	tool := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}
	if got := unwrapResult(wrapMCPResult(tool)); got != mcp.Result(tool) {
		t.Errorf("tools/call roundtrip = %#v", got)
	}

	list := &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: "a"}}}
	if got := unwrapResult(wrapMCPResult(list)); got != mcp.Result(list) {
		t.Errorf("tools/list roundtrip = %#v", got)
	}

	// mcp.Result 的 isResult() 未导出，包外无法自造假实现，
	// 这里用 SDK 自带的 initialize 结果代表 raw 直通路径。
	init := &mcp.InitializeResult{ProtocolVersion: "2025-06-18"}
	got := unwrapResult(wrapMCPResult(init))
	if got != mcp.Result(init) {
		t.Errorf("raw roundtrip = %#v, want same pointer", got)
	}
	if wrapMCPResult(init).Tool != nil || wrapMCPResult(init).List != nil {
		t.Error("raw result must not be projected into Tool/List")
	}

	if got := unwrapResult(nil); got != nil {
		t.Errorf("unwrapResult(nil) = %#v", got)
	}
	// 三者全为 nil 的非 nil Result：等同于没有结果。
	if got := unwrapResult(&Result{}); got != nil {
		t.Errorf("unwrapResult(&Result{}) = %#v, want nil", got)
	}
}

// TestWithHeadersClones 断言注入的是快照：改原 Header 不影响 ctx 里的副本。
func TestWithHeadersClones(t *testing.T) {
	h := http.Header{"X-A": {"1"}}
	ctx := WithHeaders(context.Background(), h)
	h.Set("X-A", "2")
	h.Set("X-B", "3")

	got := HeadersFromContext(ctx)
	if got.Get("X-A") != "1" || got.Get("X-B") != "" {
		t.Errorf("snapshot = %v, want frozen copy", got)
	}
}

// TestHeadersFromContextNonNil 覆盖 nil Header 与未注入的 ctx：都必须返回非 nil
// 空 Header，否则插件 Set 会 panic。
func TestHeadersFromContextNonNil(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"no injection", context.Background()},
		{"nil header injected", WithHeaders(context.Background(), nil)},
		{"empty header injected", WithHeaders(context.Background(), http.Header{})},
	} {
		got := HeadersFromContext(tc.ctx)
		if got == nil {
			t.Fatalf("%s: got nil header", tc.name)
		}
		got.Set("X-Plugin", "1") // nil map 会在这里 panic
		if got.Get("X-Plugin") != "1" {
			t.Errorf("%s: set on snapshot failed", tc.name)
		}
	}
}

// TestAsMCPMiddleware 验证适配层：能拦住 tools/call（next 不被调用），
// 能改写 tools/list 的返回值。
func TestAsMCPMiddleware(t *testing.T) {
	var reached bool
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		reached = true
		if method == "tools/list" {
			return &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: "a"}, {Name: "b"}}}, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "exec"}}}, nil
	}

	deny := func(next Handler) Handler {
		return func(ctx context.Context, c *Call) (*Result, error) {
			if c.Method == "tools/call" {
				return DenyResult(c, "test", "blocked"), nil
			}
			return next(ctx, c)
		}
	}
	filter := func(next Handler) Handler {
		return func(ctx context.Context, c *Call) (*Result, error) {
			res, err := next(ctx, c)
			if err != nil || res == nil || res.List == nil {
				return res, err
			}
			res.List.Tools = res.List.Tools[:1]
			return res, nil
		}
	}
	h := asMCPMiddleware([]Middleware{deny, filter})(next)

	res, err := h(context.Background(), "tools/call",
		&mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if reached {
		t.Error("next must not be called when call is denied")
	}
	if ctr, ok := res.(*mcp.CallToolResult); !ok || !ctr.IsError {
		t.Errorf("denied result = %#v", res)
	}

	res, err = h(context.Background(), "tools/list", &mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !reached {
		t.Error("next must be called for tools/list")
	}
	lr, ok := res.(*mcp.ListToolsResult)
	if !ok || len(lr.Tools) != 1 || lr.Tools[0].Name != "a" {
		t.Errorf("filtered list = %#v", res)
	}
}
