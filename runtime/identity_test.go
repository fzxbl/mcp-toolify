package runtime

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// subjectProbe 装一个中间件把本次调用的 Subject 抄出来，供身份来源相关断言使用。
func subjectProbe(t *testing.T, cfgBody string) func(token, userHeader string) *Subject {
	t.Helper()
	var seen *Subject
	r := New(Config{ConfigPath: writeTokenConfig(t, cfgBody)},
		func(s *mcp.Server, opts RegisterOptions) {})
	r.Use(func(next Handler) Handler {
		return func(ctx context.Context, c *Call) (*Result, error) {
			cp := *c.Subject
			seen = &cp
			return next(ctx, c)
		}
	})
	srv := mountAll(t, r)
	return func(token, userHeader string) *Subject {
		seen = nil
		req, _ := http.NewRequest(http.MethodPost, srv.URL,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		if userHeader != "" {
			req.Header.Set("X-MCP-User", userHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
		if seen == nil {
			t.Fatal("插件未拿到 Subject")
		}
		return seen
	}
}

// TestIdentityHeaderNotTrustedByDefault 覆盖 I1：默认不信任客户端身份头。
// quota 的计数键、confirm 的「同一人才能确认」都以 Subject.ID 为唯一判据，
// 默认信任等于一行 header 就能冒充任意人，且部署漏配网关时静默失效。
func TestIdentityHeaderNotTrustedByDefault(t *testing.T) {
	probe := subjectProbe(t, `
[[tokens]]
token = "t-ro"
name = "readonly-agent"
applicant = "tester"
allow = ["capability=read"]
`)
	got := probe("t-ro", "root-admin")
	if got.ID != "" {
		t.Errorf("默认应不信任身份头，Subject.ID = %q，want 空", got.ID)
	}
	if got.Token != "readonly-agent" {
		t.Errorf("Subject.Token = %q", got.Token)
	}
}

// TestIdentityHeaderTrustedWhenEnabled 显式打开开关后才按 identity_headers 取身份。
func TestIdentityHeaderTrustedWhenEnabled(t *testing.T) {
	probe := subjectProbe(t, `
trust_identity_header = true
identity_headers = ["X-MCP-User"]

[[tokens]]
token = "t-ro"
name = "readonly-agent"
applicant = "tester"
allow = ["capability=read"]
`)
	if got := probe("t-ro", "zhangsan"); got.ID != "zhangsan" {
		t.Errorf("开关打开后 Subject.ID = %q，want zhangsan", got.ID)
	}
}

// TestIdentityFixedOnToken token 绑定固定身份时，客户端头改不动它。
func TestIdentityFixedOnToken(t *testing.T) {
	probe := subjectProbe(t, `
trust_identity_header = true

[[tokens]]
token = "t-bot"
name = "bot-agent"
applicant = "tester"
identity = "fixed:svc-bot"
allow = ["capability=read"]
`)
	if got := probe("t-bot", "root-admin"); got.ID != "svc-bot" {
		t.Errorf("固定身份应优先于请求头，Subject.ID = %q，want svc-bot", got.ID)
	}
}

// TestIdentityMalformedFailsStartup identity 写法不合法必须启动失败，
// 而不是退化成「没有身份」。
func TestIdentityMalformedFailsStartup(t *testing.T) {
	for _, bad := range []string{"svc-bot", "fixed:", "header:X-User", "fixed: "} {
		_, err := NewTokenAuthz(TokenAuthzConfig{Tokens: []TokenConfig{{
			Token: "t", Name: "n", Applicant: "a", Identity: bad,
			Allow: []string{"capability=read"},
		}}})
		if err == nil {
			t.Errorf("identity=%q 应报错", bad)
		}
	}
	az, err := NewTokenAuthz(TokenAuthzConfig{Tokens: []TokenConfig{{
		Token: "t", Name: "n", Applicant: "a", Identity: "fixed:svc-bot",
		Allow: []string{"capability=read"},
	}}})
	if err != nil {
		t.Fatalf("合法 identity 报错：%v", err)
	}
	if got, _ := az.SubjectOf("t", func(string) string { return "spoofed" }); got.ID != "svc-bot" {
		t.Errorf("SubjectOf().ID = %q，want svc-bot", got.ID)
	}
}

// TestUnknownConfigKeysFailStartup 覆盖 I4/I5：配置里没人认领的键一律启动失败。
// 段落名或字段名拼错时静默用默认值，对 confirm 就是「高危工具不再二次确认」，
// 对 required_plugins 就是「补偿机制自己静默失效」。
func TestUnknownConfigKeysFailStartup(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantPart string
	}{
		{"required_plugins 键名拼错", okTokenConfig + "\nrequired_plugin = [\"audit\"]\n", "required_plugin"},
		{"插件段落名拼错", okTokenConfig + "\n[qouta]\nbackend = \"redis\"\n", "qouta"},
		{"token 字段拼错", `
[[tokens]]
token = "t-ro"
name = "readonly-agent"
applicant = "tester"
allow = ["capability=read"]
denny = ["dangerous=true"]
`, "tokens.denny"},
	}
	for _, c := range cases {
		r := New(Config{ConfigPath: writeTokenConfig(t, c.body)},
			func(s *mcp.Server, opts RegisterOptions) {})
		_, _, err := r.Handlers()
		if err == nil {
			t.Errorf("%s：应启动失败", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantPart) {
			t.Errorf("%s：错误应指出具体键 %q，实际 %v", c.name, c.wantPart, err)
		}
	}
}

// TestPluginClaimedConfigKeysAccepted 插件用 Registry.Config 认领过的段落不算未知键；
// 但段落内字段拼错仍要失败。
func TestPluginClaimedConfigKeysAccepted(t *testing.T) {
	type quotaCfg struct {
		Quota struct {
			Backend string `toml:"backend"`
		} `toml:"quota"`
	}

	good := New(Config{ConfigPath: writeTokenConfig(t,
		okTokenConfig+"\n[quota]\nbackend = \"redis\"\n")},
		func(s *mcp.Server, opts RegisterOptions) {})
	var qc quotaCfg
	if err := good.Config(&qc); err != nil {
		t.Fatalf("plugin Config: %v", err)
	}
	if qc.Quota.Backend != "redis" {
		t.Errorf("plugin config not decoded: %+v", qc)
	}
	if _, _, err := good.Handlers(); err != nil {
		t.Errorf("插件认领过的段落不应导致启动失败：%v", err)
	}

	bad := New(Config{ConfigPath: writeTokenConfig(t,
		okTokenConfig+"\n[quota]\nbakcend = \"redis\"\n")},
		func(s *mcp.Server, opts RegisterOptions) {})
	var qc2 quotaCfg
	if err := bad.Config(&qc2); err != nil {
		t.Fatalf("plugin Config: %v", err)
	}
	if _, _, err := bad.Handlers(); err == nil {
		t.Error("插件段落内字段拼错应启动失败")
	}
}
