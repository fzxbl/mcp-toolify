package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTokenAuthzCallAndList(t *testing.T) {
	ResetToolsForTest()
	RegisterTool(ToolInfo{Name: "a.read", Pkg: "a", Labels: map[string]string{"capability": "read", "risk": "low"}})
	RegisterTool(ToolInfo{Name: "a.write", Pkg: "a", Labels: map[string]string{"capability": "write", "risk": "high"}})
	RegisterTool(ToolInfo{Name: "a.untagged", Pkg: "a"})

	cfg := TokenConfig{Token: "t1", Name: "ro", Applicant: "zhangsan",
		Allow: []string{"capability=read"}, Deny: []string{"!risk"}}
	az, err := NewTokenAuthz(TokenAuthzConfig{Tokens: []TokenConfig{cfg}})
	if err != nil {
		t.Fatal(err)
	}

	// tools/call：无权的写工具被拒
	c := &Call{Method: "tools/call", Tool: "a.write",
		Labels:  map[string]string{"capability": "write", "risk": "high"},
		Subject: &Subject{Token: "ro"}}
	res, _ := Chain([]Middleware{az.Middleware()}, func(ctx context.Context, c *Call) (*Result, error) {
		return TextResult("executed"), nil
	})(context.Background(), c)
	if !res.Tool.IsError {
		t.Error("write tool should be denied for readonly token")
	}

	// tools/list：只剩 a.read（a.untagged 被 !risk 兜底拒掉）
	lc := &Call{Method: "tools/list", Subject: &Subject{Token: "ro"}, Tools: Tools}
	lres, _ := Chain([]Middleware{az.Middleware()}, func(ctx context.Context, c *Call) (*Result, error) {
		return ListResult(&mcp.ListToolsResult{Tools: []*mcp.Tool{
			{Name: "a.read"}, {Name: "a.write"}, {Name: "a.untagged"},
		}}), nil
	})(context.Background(), lc)
	if len(lres.List.Tools) != 1 || lres.List.Tools[0].Name != "a.read" {
		t.Errorf("visible tools = %v", lres.List.Tools)
	}
}

// mustAuthz 构造一个合法的单 token TokenAuthz，token 值固定 t1、用途名 ro。
func mustAuthz(t *testing.T, allow, deny []string) *TokenAuthz {
	t.Helper()
	az, err := NewTokenAuthz(TokenAuthzConfig{Tokens: []TokenConfig{{
		Token: "t1", Name: "ro", Applicant: "zhangsan", Allow: allow, Deny: deny,
	}}})
	if err != nil {
		t.Fatalf("NewTokenAuthz: %v", err)
	}
	return az
}

// TestNewTokenAuthzValidation 覆盖启动期校验：缺必填项、token/name 重复、selector 语法错误。
// 同时断言错误信息里绝不出现 token 值（错误会进日志）。
func TestNewTokenAuthzValidation(t *testing.T) {
	const secret = "s3cr3t-token-value"
	cases := []struct {
		name string
		cfgs []TokenConfig
		want string // 错误信息里应包含的片段
	}{
		{
			name: "缺 token",
			cfgs: []TokenConfig{{Name: "ro", Applicant: "zhangsan"}},
			want: "tokens[0] 缺少 token",
		},
		{
			name: "缺 name",
			cfgs: []TokenConfig{{Token: secret, Applicant: "zhangsan"}},
			want: "tokens[0] 缺少 name",
		},
		{
			name: "缺 applicant",
			cfgs: []TokenConfig{{Token: secret, Name: "ro"}},
			want: "缺少 applicant",
		},
		{
			name: "token 值重复",
			cfgs: []TokenConfig{
				{Token: secret, Name: "ro", Applicant: "zhangsan"},
				{Token: secret, Name: "rw", Applicant: "lisi"},
			},
			want: "与 tokens[0] 重复",
		},
		{
			name: "name 重复",
			cfgs: []TokenConfig{
				{Token: secret, Name: "ro", Applicant: "zhangsan"},
				{Token: "another", Name: "ro", Applicant: "lisi"},
			},
			want: "name=ro 与前面的条目重复",
		},
		{
			name: "allow selector 语法错误",
			cfgs: []TokenConfig{{Token: secret, Name: "ro", Applicant: "zhangsan",
				Allow: []string{"risk notin(high)"}}},
			want: "allow:",
		},
		{
			name: "deny selector 语法错误",
			cfgs: []TokenConfig{{Token: secret, Name: "ro", Applicant: "zhangsan",
				Allow: []string{"capability=read"}, Deny: []string{"!="}}},
			want: "deny:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			az, err := NewTokenAuthz(TokenAuthzConfig{Tokens: tc.cfgs})
			if err == nil {
				t.Fatalf("NewTokenAuthz(%s) 应当报错", tc.name)
			}
			if az != nil {
				t.Errorf("出错时应返回 nil authz，got %v", az)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, 应含 %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("错误信息回显了 token 值: %q", err)
			}
		})
	}
}

// TestTokenAuthzAllowed 覆盖判定优先级：deny 优先于 allow、都不命中即拒（deny-by-default）、
// 以及缺失 label 的兜底（k8s 语义下 notin/!= 对缺失 key 匹配成功，需 !risk 兜底）。
func TestTokenAuthzAllowed(t *testing.T) {
	t.Run("deny 优先于 allow", func(t *testing.T) {
		az := mustAuthz(t, []string{"capability=read"}, []string{"risk=high"})
		if az.allowed("ro", map[string]string{"capability": "read", "risk": "low"}) != true {
			t.Error("allow 命中且 deny 不命中应放行")
		}
		if az.allowed("ro", map[string]string{"capability": "read", "risk": "high"}) {
			t.Error("同时命中 allow 与 deny 时必须拒绝")
		}
	})

	t.Run("都不命中即拒", func(t *testing.T) {
		az := mustAuthz(t, []string{"capability=read"}, []string{"risk=high"})
		if az.allowed("ro", map[string]string{"capability": "write", "risk": "low"}) {
			t.Error("allow/deny 都不命中时必须拒绝（deny-by-default）")
		}
	})

	t.Run("allow 为空即全拒", func(t *testing.T) {
		az := mustAuthz(t, nil, nil)
		if az.allowed("ro", map[string]string{"capability": "read"}) {
			t.Error("未配 allow 的 token 不应能执行任何工具")
		}
	})

	t.Run("未知 token 用途名全拒", func(t *testing.T) {
		az := mustAuthz(t, []string{"capability=read"}, nil)
		if az.allowed("nobody", map[string]string{"capability": "read"}) {
			t.Error("未知 token 用途名必须拒绝")
		}
	})

	t.Run("缺失 label 被 !risk 兜底拒掉", func(t *testing.T) {
		az := mustAuthz(t, []string{"capability=write"}, []string{"!risk"})
		if az.allowed("ro", map[string]string{"capability": "write"}) {
			t.Error("只标了 capability=write 却没标 risk 的工具必须被 !risk 拒掉")
		}
		if !az.allowed("ro", map[string]string{"capability": "write", "risk": "low"}) {
			t.Error("标了 risk 的写工具应放行")
		}
	})
}

// TestTokenAuthzTokenName 覆盖按 token 值取用途名：命中与不命中。
func TestTokenAuthzTokenName(t *testing.T) {
	az := mustAuthz(t, []string{"capability=read"}, []string{"!risk"})
	name, ok := az.TokenName("t1")
	if !ok || name != "ro" {
		t.Errorf("TokenName(t1) = (%q, %v), want (ro, true)", name, ok)
	}
	if _, ok := az.TokenName("wrong"); ok {
		t.Error("未配置的 token 必须 ok=false（HTTP 层据此 401）")
	}
	if _, ok := az.TokenName(""); ok {
		t.Error("空 token 必须 ok=false")
	}
}

// TestTokenAuthzListHidesUnregistered 验证 tools/list 过滤：注册表里查不到元数据的工具
// 一律不可见，即使它命中 allow（labels 无从取得，不能放行）。
func TestTokenAuthzListHidesUnregistered(t *testing.T) {
	ResetToolsForTest()
	RegisterTool(ToolInfo{Name: "a.read", Pkg: "a", Labels: map[string]string{"capability": "read", "risk": "low"}})

	az := mustAuthz(t, []string{"capability=read", "!capability"}, []string{"!risk"})
	lc := &Call{Method: "tools/list", Subject: &Subject{Token: "ro"}, Tools: Tools}
	res, err := Chain([]Middleware{az.Middleware()}, func(ctx context.Context, c *Call) (*Result, error) {
		return ListResult(&mcp.ListToolsResult{Tools: []*mcp.Tool{
			{Name: "a.read"}, {Name: "spill_explore"}, // 后者未 RegisterTool
		}}), nil
	})(context.Background(), lc)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.List.Tools) != 1 || res.List.Tools[0].Name != "a.read" {
		t.Errorf("visible tools = %v，未登记的工具必须不可见", res.List.Tools)
	}
}

// TestTokenAuthzPassthrough 验证放行路径与非 tools 方法透传：
// 命中 allow 的 tools/call 应到达链尾，initialize 之类的方法不做任何判定。
func TestTokenAuthzPassthrough(t *testing.T) {
	ResetToolsForTest()
	RegisterTool(ToolInfo{Name: "a.read", Pkg: "a", Labels: map[string]string{"capability": "read", "risk": "low"}})
	az := mustAuthz(t, []string{"capability=read"}, []string{"!risk"})

	reached := false
	final := func(ctx context.Context, c *Call) (*Result, error) {
		reached = true
		return TextResult("executed"), nil
	}

	c := &Call{Method: "tools/call", Tool: "a.read",
		Labels:  map[string]string{"capability": "read", "risk": "low"},
		Subject: &Subject{Token: "ro"}}
	res, err := Chain([]Middleware{az.Middleware()}, final)(context.Background(), c)
	if err != nil || !reached || res.Tool.IsError {
		t.Errorf("有权的读工具应放行: reached=%v res=%+v err=%v", reached, res, err)
	}

	reached = false
	ic := &Call{Method: "initialize", Subject: &Subject{Token: "unknown"}}
	if _, err := Chain([]Middleware{az.Middleware()}, final)(context.Background(), ic); err != nil {
		t.Fatal(err)
	}
	if !reached {
		t.Error("initialize 等方法必须透传，不参与准入判定")
	}
}

// TestTokenAuthzDenyMeta 验证拒绝时按契约写入 Meta，供审计插件读取。
func TestTokenAuthzDenyMeta(t *testing.T) {
	ResetToolsForTest()
	RegisterTool(ToolInfo{Name: "a.write", Pkg: "a", Labels: map[string]string{"capability": "write", "risk": "high"}})
	az := mustAuthz(t, []string{"capability=read"}, []string{"!risk"})
	c := &Call{Method: "tools/call", Tool: "a.write",
		Labels:  map[string]string{"capability": "write", "risk": "high"},
		Subject: &Subject{Token: "ro"}}
	res, _ := Chain([]Middleware{az.Middleware()}, func(ctx context.Context, c *Call) (*Result, error) {
		t.Error("被拒的调用不应到达链尾")
		return nil, nil
	})(context.Background(), c)
	if res == nil || res.Tool == nil || !res.Tool.IsError {
		t.Fatalf("应返回 IsError 结果，got %+v", res)
	}
	if c.Meta[MetaDeniedBy] != "token_authz" {
		t.Errorf("Meta[%s] = %v, want token_authz", MetaDeniedBy, c.Meta[MetaDeniedBy])
	}
	reason, _ := c.Meta[MetaDenyReason].(string)
	if !strings.Contains(reason, "token ro 无权执行 a.write") {
		t.Errorf("Meta[%s] = %q，规则不匹配时应带用途名与工具名", MetaDenyReason, reason)
	}
}

// TestTokenAuthzUnregisteredToolDenied 是 I2 的回归用例：未登记 labels 的工具在
// tools/list 里不可见，在 tools/call 里也必须不可执行——即使 token 配了否定型
// selector（allow=["!capability"]，k8s 语义下对缺失 key 匹配成功）。
// 「不可见」不是访问控制。
func TestTokenAuthzUnregisteredToolDenied(t *testing.T) {
	ResetToolsForTest() // 注册表为空：模拟直接挂在 mcp.Server 上、没登记 labels 的工具
	az := mustAuthz(t, []string{"!capability"}, nil)

	// 前置断言：这条 allow 对 nil / 空 labels 确实匹配成功，否则本用例证明不了什么。
	if !az.allowed("ro", nil) {
		t.Fatal("前置条件不成立：allow=[\"!capability\"] 应对缺失 label 匹配成功")
	}

	c := &Call{Method: "tools/call", Tool: "unregistered.tool",
		Subject: &Subject{Token: "ro"}} // Labels 为 nil，与 newCall 查不到工具时一致
	res, _ := Chain([]Middleware{az.Middleware()}, func(ctx context.Context, c *Call) (*Result, error) {
		t.Error("未登记 labels 的工具不应到达链尾")
		return TextResult("executed"), nil
	})(context.Background(), c)
	if res == nil || res.Tool == nil || !res.Tool.IsError {
		t.Fatalf("未登记 labels 的工具必须被拒，got %+v", res)
	}
	if reason, _ := c.Meta[MetaDenyReason].(string); !strings.Contains(reason, "未登记 labels") {
		t.Errorf("deny 原因 = %q，应说明工具未登记 labels", reason)
	}

	// 同一 token 的 tools/list 也看不到它，两条路径口径一致。
	lc := &Call{Method: "tools/list", Subject: &Subject{Token: "ro"}, Tools: Tools}
	lres, _ := Chain([]Middleware{az.Middleware()}, func(ctx context.Context, c *Call) (*Result, error) {
		return ListResult(&mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: "unregistered.tool"}}}), nil
	})(context.Background(), lc)
	if len(lres.List.Tools) != 0 {
		t.Errorf("visible tools = %v，未登记 labels 的工具必须不可见", lres.List.Tools)
	}
}

// TestTokenAuthzCallUsesRegistryLabels 验证 call 判据取自注册表而非 Call.Labels：
// 链上任何一环都能改 Call.Labels，伪造的 labels 不得骗过准入。
func TestTokenAuthzCallUsesRegistryLabels(t *testing.T) {
	ResetToolsForTest()
	RegisterTool(ToolInfo{Name: "a.write", Pkg: "a", Labels: map[string]string{"capability": "write", "risk": "high"}})
	az := mustAuthz(t, []string{"capability=read"}, []string{"!risk"})

	c := &Call{Method: "tools/call", Tool: "a.write",
		Labels:  map[string]string{"capability": "read", "risk": "low"}, // 伪造成只读
		Subject: &Subject{Token: "ro"}}
	res, _ := Chain([]Middleware{az.Middleware()}, func(ctx context.Context, c *Call) (*Result, error) {
		t.Error("伪造 labels 不应放行")
		return nil, nil
	})(context.Background(), c)
	if res == nil || res.Tool == nil || !res.Tool.IsError {
		t.Errorf("应按注册表 labels 判定并拒绝，got %+v", res)
	}
}

// TestTokenAuthzUnknownTokenName 验证未知用途名的处理：deny 原因固定为
// unknown token name，绝不回显传入值（上游填错时传入的可能是 token 值）。
func TestTokenAuthzUnknownTokenName(t *testing.T) {
	ResetToolsForTest()
	RegisterTool(ToolInfo{Name: "a.read", Pkg: "a", Labels: map[string]string{"capability": "read", "risk": "low"}})
	az := mustAuthz(t, []string{"capability=read"}, []string{"!risk"})

	const leaked = "s3cr3t-token-value"
	c := &Call{Method: "tools/call", Tool: "a.read",
		Subject: &Subject{Token: leaked}} // 上游误把 token 值塞进来
	res, _ := Chain([]Middleware{az.Middleware()}, func(ctx context.Context, c *Call) (*Result, error) {
		t.Error("未知用途名不应放行")
		return nil, nil
	})(context.Background(), c)
	if res == nil || res.Tool == nil || !res.Tool.IsError {
		t.Fatalf("未知 token 用途名必须拒绝，got %+v", res)
	}
	reason, _ := c.Meta[MetaDenyReason].(string)
	if reason != "unknown token name" {
		t.Errorf("deny 原因 = %q, want unknown token name", reason)
	}
	if strings.Contains(resultText(res), leaked) {
		t.Errorf("返回文案回显了传入的 token 值: %q", resultText(res))
	}
}

// resultText 取 tools/call 结果里的第一段文本，供断言用。
func resultText(res *Result) string {
	if res == nil || res.Tool == nil {
		return ""
	}
	for _, ct := range res.Tool.Content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// TestTokenAuthzListDoesNotAliasUpstream 是 I1 的回归用例：过滤必须返回新切片，
// 不得原地改写调用方仍持有的那个切片（否则一个 token 的过滤结果会污染另一个）。
func TestTokenAuthzListDoesNotAliasUpstream(t *testing.T) {
	ResetToolsForTest()
	RegisterTool(ToolInfo{Name: "a.read", Pkg: "a", Labels: map[string]string{"capability": "read", "risk": "low"}})
	RegisterTool(ToolInfo{Name: "a.write", Pkg: "a", Labels: map[string]string{"capability": "write", "risk": "high"}})
	az := mustAuthz(t, []string{"capability=read"}, []string{"!risk"})

	upstream := []*mcp.Tool{{Name: "a.write"}, {Name: "a.read"}}
	lc := &Call{Method: "tools/list", Subject: &Subject{Token: "ro"}, Tools: Tools}
	res, _ := Chain([]Middleware{az.Middleware()}, func(ctx context.Context, c *Call) (*Result, error) {
		return ListResult(&mcp.ListToolsResult{Tools: upstream}), nil
	})(context.Background(), lc)

	if len(res.List.Tools) != 1 || res.List.Tools[0].Name != "a.read" {
		t.Fatalf("visible tools = %v, want [a.read]", res.List.Tools)
	}
	if upstream[0].Name != "a.write" || upstream[1].Name != "a.read" {
		t.Errorf("调用方持有的切片被就地改写: %v %v", upstream[0].Name, upstream[1].Name)
	}
}

// TestTokenAuthzNilSubject 验证 Subject 缺失时 fail-closed（不 panic、直接拒）。
func TestTokenAuthzNilSubject(t *testing.T) {
	ResetToolsForTest()
	RegisterTool(ToolInfo{Name: "a.read", Pkg: "a", Labels: map[string]string{"capability": "read", "risk": "low"}})
	az := mustAuthz(t, []string{"capability=read"}, []string{"!risk"})
	c := &Call{Method: "tools/call", Tool: "a.read",
		Labels: map[string]string{"capability": "read", "risk": "low"}}
	res, _ := Chain([]Middleware{az.Middleware()}, func(ctx context.Context, c *Call) (*Result, error) {
		return TextResult("executed"), nil
	})(context.Background(), c)
	if res == nil || res.Tool == nil || !res.Tool.IsError {
		t.Errorf("Subject 缺失时必须拒绝，got %+v", res)
	}
}

// TestResolveIdentity 覆盖身份解析：多头名按顺序取第一个非空、全空返回空串、
// 缺省头名生效、大小写不敏感去重。身份头只能在构造时传入（构造后 TokenAuthz 只读）。
func TestResolveIdentity(t *testing.T) {
	newAZ := func(headers []string) *TokenAuthz {
		t.Helper()
		az, err := NewTokenAuthz(TokenAuthzConfig{
			Tokens: []TokenConfig{{Token: "t1", Name: "ro", Applicant: "zhangsan",
				Allow: []string{"capability=read"}, Deny: []string{"!risk"}}},
			IdentityHeaders: headers,
		})
		if err != nil {
			t.Fatalf("NewTokenAuthz: %v", err)
		}
		return az
	}

	// 缺省头名：未配置时用 X-MCP-User。
	az := newAZ(nil)
	if got := az.IdentityHeaders(); len(got) != 1 || got[0] != "X-MCP-User" {
		t.Errorf("缺省 IdentityHeaders = %v, want [X-MCP-User]", got)
	}
	if got := az.ResolveIdentity(func(n string) string {
		if n == "X-MCP-User" {
			return " zhangsan "
		}
		return ""
	}); got != "zhangsan" {
		t.Errorf("ResolveIdentity = %q, want zhangsan", got)
	}

	// 多头名：按配置顺序取第一个非空值；空白项被丢弃。
	az = newAZ([]string{" ", "X-Gateway-User", "X-MCP-User"})
	if got := az.IdentityHeaders(); len(got) != 2 || got[0] != "X-Gateway-User" {
		t.Fatalf("IdentityHeaders = %v, want [X-Gateway-User X-MCP-User]", got)
	}
	got := az.ResolveIdentity(func(n string) string {
		switch n {
		case "X-Gateway-User":
			return ""
		case "X-MCP-User":
			return "lisi"
		}
		return ""
	})
	if got != "lisi" {
		t.Errorf("ResolveIdentity = %q, want lisi（第一个头为空时取下一个）", got)
	}

	// 全空返回空串。
	if got := az.ResolveIdentity(func(string) string { return "" }); got != "" {
		t.Errorf("全空时 ResolveIdentity = %q, want 空串", got)
	}

	// 大小写不敏感去重，保序保留首次出现的写法。
	az = newAZ([]string{"X-MCP-User", "x-mcp-user", "X-Gateway-User"})
	if got := az.IdentityHeaders(); len(got) != 2 || got[0] != "X-MCP-User" || got[1] != "X-Gateway-User" {
		t.Errorf("IdentityHeaders = %v, want [X-MCP-User X-Gateway-User]", got)
	}
}

// TestNeedsMissingKeyWarning 验证启动 warning 的判据：只有 allow 用了 !=/notin
// （k8s 语义下对缺失 key 匹配成功）且 deny 无 DoesNotExist 兜底时才该报警。
// 等值型 allow 本来就匹配不上未标注的工具，不该喊狼。
func TestNeedsMissingKeyWarning(t *testing.T) {
	cases := []struct {
		allow, deny []string
		want        bool
	}{
		{allow: []string{"risk notin (high)"}, deny: nil, want: true},
		{allow: []string{"capability!=write"}, deny: nil, want: true},
		{allow: []string{"risk notin (high)"}, deny: []string{"!risk"}, want: false},
		{allow: []string{"capability!=write"}, deny: []string{"capability=write,!risk"}, want: false},
		{allow: []string{"capability=read"}, deny: nil, want: false},                          // 误报回归：等值 allow 无洞
		{allow: []string{"capability=read"}, deny: []string{"capability!=read"}, want: false}, // != 不是 DoesNotExist
		{allow: nil, deny: nil, want: false},
	}
	for _, tc := range cases {
		allow, err := parseSelectors(tc.allow)
		if err != nil {
			t.Fatalf("parse allow %v: %v", tc.allow, err)
		}
		deny, err := parseSelectors(tc.deny)
		if err != nil {
			t.Fatalf("parse deny %v: %v", tc.deny, err)
		}
		if got := needsMissingKeyWarning(allow, deny); got != tc.want {
			t.Errorf("needsMissingKeyWarning(allow=%v, deny=%v) = %v, want %v",
				tc.allow, tc.deny, got, tc.want)
		}
	}
}
