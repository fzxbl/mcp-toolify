package runtime

import (
	"context"
	"log/slog"
	"runtime/debug"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestResolveServerIdentity：显式配置优先；缺省时从构建信息推导，宿主没打 tag 时
// 版本是带 commit id 的伪版本；什么都读不到时为 (devel) 并标记待告警。
func TestResolveServerIdentity(t *testing.T) {
	dep := func(v string, rep *debug.Module) []*debug.Module {
		return []*debug.Module{
			{Path: "github.com/BurntSushi/toml", Version: "v1.6.0"},
			{Path: toolifyModule, Version: v, Replace: rep},
		}
	}
	cases := []struct {
		name string
		cfg  Config
		bi   *debug.BuildInfo
		want serverIdentity
	}{
		{
			name: "显式配置覆盖构建信息",
			cfg:  Config{Name: "orders", Version: "1.0.0"},
			bi: &debug.BuildInfo{Main: debug.Module{Path: "github.com/acme/order-mcp", Version: "v2.0.0"},
				Deps: dep("v0.4.2", nil)},
			want: serverIdentity{Name: "orders", Version: "1.0.0", Toolify: "v0.4.2"},
		},
		{
			name: "取 tag 版本",
			bi: &debug.BuildInfo{Main: debug.Module{Path: "github.com/acme/order-mcp", Version: "v1.2.0"},
				Deps: dep("v0.4.2", nil)},
			want: serverIdentity{Name: "order-mcp", Version: "v1.2.0", Toolify: "v0.4.2"},
		},
		{
			name: "没打 tag 取伪版本",
			bi: &debug.BuildInfo{Main: debug.Module{Path: "github.com/acme/order-mcp",
				Version: "v0.0.0-20261102025348-453178eb0cf9+dirty"}, Deps: dep("v0.4.2", nil)},
			want: serverIdentity{Name: "order-mcp", Version: "v0.0.0-20261102025348-453178eb0cf9+dirty", Toolify: "v0.4.2"},
		},
		{
			name: "大版本后缀不是服务名",
			bi: &debug.BuildInfo{Main: debug.Module{Path: "github.com/acme/order-mcp/v3", Version: "v3.1.0"},
				Deps: dep("v0.4.2", nil)},
			want: serverIdentity{Name: "order-mcp", Version: "v3.1.0", Toolify: "v0.4.2"},
		},
		{
			name: "go run 只有 (devel)",
			bi: &debug.BuildInfo{Main: debug.Module{Path: "github.com/acme/order-mcp", Version: "(devel)"},
				Deps: dep("v0.4.2", nil)},
			want: serverIdentity{Name: "order-mcp", Version: develVersion, Toolify: "v0.4.2", VersionUnknown: true},
		},
		{
			name: "显式版本时不告警",
			cfg:  Config{Version: "build-42"},
			bi:   &debug.BuildInfo{Main: debug.Module{Path: "github.com/acme/order-mcp", Version: "(devel)"}},
			want: serverIdentity{Name: "order-mcp", Version: "build-42", Toolify: develVersion},
		},
		{
			name: "基座被 replace 为带版本的模块",
			bi: &debug.BuildInfo{Main: debug.Module{Path: "github.com/acme/order-mcp", Version: "v1.0.0"},
				Deps: dep("v0.4.2", &debug.Module{Path: "github.com/acme/mcp-toolify", Version: "v0.4.3-fix"})},
			want: serverIdentity{Name: "order-mcp", Version: "v1.0.0", Toolify: "v0.4.3-fix"},
		},
		{
			name: "基座被 replace 为本地目录",
			bi: &debug.BuildInfo{Main: debug.Module{Path: "github.com/acme/order-mcp", Version: "v1.0.0"},
				Deps: dep("v0.4.2", &debug.Module{Path: "../mcp-toolify"})},
			want: serverIdentity{Name: "order-mcp", Version: "v1.0.0", Toolify: "(devel) => ../mcp-toolify"},
		},
		{
			name: "宿主就是基座本身",
			bi:   &debug.BuildInfo{Main: debug.Module{Path: toolifyModule, Version: "v0.5.0"}},
			want: serverIdentity{Name: "mcp-toolify", Version: "v0.5.0", Toolify: "v0.5.0"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveServerIdentity(c.cfg, c.bi); got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestResolveServerIdentityNoBuildInfo：读不到构建信息时名字退回可执行文件名，不能是空串。
func TestResolveServerIdentityNoBuildInfo(t *testing.T) {
	got := resolveServerIdentity(Config{}, nil)
	if got.Name == "" {
		t.Error("读不到构建信息时 Name 不能为空")
	}
	if got.Version != develVersion || !got.VersionUnknown || got.Toolify != develVersion {
		t.Errorf("got %+v", got)
	}
}

// TestInitializeReturnsConfiguredServerInfo：客户端在 initialize 时看到的必须是
// 宿主配置的名称与版本，而不是基座自己的名字。
func TestInitializeReturnsConfiguredServerInfo(t *testing.T) {
	l := useLogger(t)
	r := NewRegistry(Config{Name: "order-mcp", Version: "v1.2.0", Logger: l})
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := r.Server().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	info := cs.InitializeResult().ServerInfo
	if info.Name != "order-mcp" || info.Version != "v1.2.0" {
		t.Errorf("serverInfo = %+v", info)
	}
	if _, ok := l.find(`name="order-mcp" version="v1.2.0"`); !ok {
		t.Errorf("启动日志缺少 server 身份: %+v", l.recs)
	}
	if rec, ok := l.find("未能从构建信息获取版本"); ok {
		t.Errorf("显式配置了版本不应告警: %+v", rec)
	}
}

// TestUnknownVersionWarns：版本推导不出来时必须告警（Warn 级别），但不能让启动失败。
// go test 构建的测试二进制主模块版本就是 (devel)，正好是这个形态。
func TestUnknownVersionWarns(t *testing.T) {
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi.Main.Version != develVersion {
		t.Skipf("测试二进制的主模块版本不是 (devel)：%v", bi)
	}
	l := useLogger(t)
	r := NewRegistry(Config{Logger: l})
	rec, ok := l.find("未能从构建信息获取版本")
	if !ok {
		t.Fatalf("没有版本告警: %+v", l.recs)
	}
	if rec.level != slog.LevelWarn {
		t.Errorf("版本告警应为 Warn，实际 %v", rec.level)
	}
	if r.err != nil {
		t.Errorf("版本未知不应记为启动失败: %v", r.err)
	}
}
