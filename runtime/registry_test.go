package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// writeTokenConfig 写一份最小可用的基座配置到临时文件，返回路径。
func writeTokenConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.toml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const okTokenConfig = `
[[tokens]]
token = "t-ro"
name = "readonly-agent"
applicant = "tester"
allow = ["capability=read"]
`

func TestRegistryUseAndRequiredPlugins(t *testing.T) {
	r := NewRegistry(Config{RequiredPlugins: []string{"audit"}})
	r.Named("audit")
	r.Use(func(next Handler) Handler { return next })
	if err := r.validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(r.Middlewares()) != 1 {
		t.Errorf("middlewares = %d, want 1", len(r.Middlewares()))
	}

	r2 := NewRegistry(Config{RequiredPlugins: []string{"audit"}})
	if err := r2.validate(); err == nil {
		t.Error("missing required plugin should fail validation")
	}

	var stopped bool
	r.OnStop(func() { stopped = true })
	r.RunStop(context.Background())
	if !stopped {
		t.Error("OnStop hook not run")
	}
}

// TestRegistryMatchSyntaxErrorFailsStartup：cfg.Match 语法错误必须在启动期失败，
// 而不是「服务起来了但 tools/list 是空的」。
func TestRegistryMatchSyntaxErrorFailsStartup(t *testing.T) {
	var registrarCalled bool
	r := New(Config{Match: "risk in high", ConfigPath: writeTokenConfig(t, okTokenConfig)},
		func(s *mcp.Server, opts RegisterOptions) { registrarCalled = true })
	if registrarCalled {
		t.Error("registrar must not run with an invalid match selector")
	}
	err := r.validate()
	if err == nil {
		t.Fatal("invalid match selector must fail startup")
	}
	if !strings.Contains(err.Error(), "risk in high") {
		t.Errorf("error must echo the offending selector, got %v", err)
	}
	if _, _, err := r.Handlers(); err == nil {
		t.Error("Handlers must propagate the startup error")
	}
}

// TestRegistryLogsRegisteredAndFiltered：注册期计数必须能反映「selector 合法但
// label 拼错导致 0 匹配」这类 Parse 抓不到的事故。
func TestRegistryLogsRegisteredAndFiltered(t *testing.T) {
	r := New(Config{Match: "capability=read", ConfigPath: writeTokenConfig(t, okTokenConfig)},
		func(s *mcp.Server, opts RegisterOptions) {
			if opts.Allow("demo", map[string]string{"capability": "read"}) {
				RegisterTool(ToolInfo{Name: "demo.read", Pkg: "demo",
					Labels: map[string]string{"capability": "read"}})
			}
			if opts.Allow("demo", map[string]string{"capability": "write"}) {
				t.Error("write tool must be filtered out")
			}
		})
	registered, filtered := r.RegisterCounts()
	if registered != 1 || filtered != 1 {
		t.Errorf("registered/filtered = %d/%d, want 1/1", registered, filtered)
	}
}

// TestRegisterOptionsCompile：Compile 预编译 matcher，行为与现场 Parse 一致；
// 语法错误在 Compile 时报出而不是被 Allow 静默吞掉。
func TestRegisterOptionsCompile(t *testing.T) {
	o, err := RegisterOptions{Match: "risk in (high)"}.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if o.matcher == nil {
		t.Fatal("Compile must precompile the matcher")
	}
	if !o.Allow("orders", map[string]string{"risk": "high"}) {
		t.Error("compiled matcher should allow risk=high")
	}
	if o.Allow("orders", map[string]string{"risk": "low"}) {
		t.Error("compiled matcher should reject risk=low")
	}

	if _, err := (RegisterOptions{Match: "risk in high"}).Compile(); err == nil {
		t.Error("Compile must reject invalid selector syntax")
	}

	// 空 Match 视同不过滤，Compile 不报错也不留 matcher。
	blank, err := RegisterOptions{Match: "  "}.Compile()
	if err != nil {
		t.Fatalf("Compile(blank): %v", err)
	}
	if blank.matcher != nil {
		t.Error("blank match must not produce a matcher")
	}
	if !blank.Allow("orders", nil) {
		t.Error("blank match should allow everything")
	}
}

// TestRegistryRequiresTokens：token 鉴权始终启用——没有 ConfigPath 或配置里没有
// [[tokens]] 一律启动失败，而不是静默起一个无鉴权端点。
func TestRegistryRequiresTokens(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no config path", Config{}},
		{"config without tokens", Config{ConfigPath: writeTokenConfig(t, "identity_headers = [\"X-MCP-User\"]\n")}},
		{"missing file", Config{ConfigPath: filepath.Join(t.TempDir(), "absent.toml")}},
	}
	for _, c := range cases {
		r := New(c.cfg, func(s *mcp.Server, opts RegisterOptions) {})
		if _, _, err := r.Handlers(); err == nil {
			t.Errorf("%s: Handlers must fail without tokens", c.name)
		}
	}
}

// TestRegistryHandlersEnforceAuth：HTTP 路径必须有鉴权——无 token 401，
// 已配置的 token 放行到 MCP 层。
func TestRegistryHandlersEnforceAuth(t *testing.T) {
	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)},
		func(s *mcp.Server, opts RegisterOptions) {})
	h, routes, err := r.Handlers()
	if err != nil {
		t.Fatalf("Handlers: %v", err)
	}
	if h == nil {
		t.Fatal("mcp handler must not be nil")
	}
	if routes == nil {
		t.Error("routes map must not be nil")
	}

	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token => status %d, want 401", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer t-ro")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Error("valid token must not be rejected with 401")
	}
}

// TestRegistryRoutes：插件注册的 HTTP 路由要能被基座取到并挂载；需鉴权的路由在
// token 准入尚未初始化时 fail-closed 不外露，显式声明的公开路由不受影响。
func TestRegistryRoutes(t *testing.T) {
	r := NewRegistry(Config{})
	r.Route("/spill/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	r.RoutePublic("/healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	got := r.Routes()
	if _, ok := got["/healthz"]; !ok {
		t.Error("公开路由必须能被取到")
	}
	if _, ok := got["/spill/"]; ok {
		t.Error("鉴权未初始化时不得外露需鉴权的路由")
	}
}

// TestTokenAuthzRunsBeforePluginChain：基座的 token 准入必须固定装在插件链之前——
// 被 token 拒掉的调用不应进入任何插件中间件，否则插件顺序就能绕过访问控制。
func TestTokenAuthzRunsBeforePluginChain(t *testing.T) {
	ResetToolsForTest()
	defer ResetToolsForTest()

	var pluginRan bool
	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)},
		func(s *mcp.Server, opts RegisterOptions) {
			mcp.AddTool(s, &mcp.Tool{Name: "demo.write", Description: "write tool"},
				func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
					return nil, "done", nil
				})
			RegisterTool(ToolInfo{Name: "demo.write", Pkg: "demo",
				Labels: map[string]string{"capability": "write", "risk": "high"}})
		})
	r.Named("probe")
	r.Use(func(next Handler) Handler {
		return func(ctx context.Context, c *Call) (*Result, error) {
			pluginRan = true
			return next(ctx, c)
		}
	})

	h, _, err := r.Handlers()
	if err != nil {
		t.Fatalf("Handlers: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"demo.write","arguments":{}}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer t-ro")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(raw), "permission denied") {
		t.Fatalf("read-only token must be denied on a write tool, got %s", raw)
	}
	if pluginRan {
		t.Error("插件中间件不应看到被 token 准入拒掉的调用（准入必须在链之前）")
	}
}

// TestConfigExampleLoads：仓库里唯一一份 [[tokens]] schema 说明必须真的能被加载器接受。
// 文档里的 schema 写错比没有文档更糟——照抄的人会得到一个启动失败或全拒的服务。
func TestConfigExampleLoads(t *testing.T) {
	r := New(Config{ConfigPath: filepath.Join("..", "config.example.toml")},
		func(s *mcp.Server, opts RegisterOptions) {})
	if _, _, err := r.Handlers(); err != nil {
		t.Fatalf("config.example.toml 无法被基座加载: %v", err)
	}
}
