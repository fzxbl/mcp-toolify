package runtime

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// resetRoutePrefix 让用例之间互不影响：前缀是进程级状态（与 PublicBaseURL 同源的取舍）。
func resetRoutePrefix(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { setRoutePrefix("") })
}

// TestRoutePrefixAppliedToRoutes：宿主声明了前缀时，Routes() 交出来的 pattern 必须**已经
// 带上前缀**。
//
// 为什么是基座施加而不是宿主 mount 时自己加：与挂载点必须一致的地方有三处——Routes()
// 的 pattern、插件拼给 agent 的绝对 URL、副本间转发的目标路径。宿主自己加只能改到第一处，
// 另外两处仍指向旧路径，表现是「URL 能下但换个副本 404」「回调 404」，单副本部署看不出来。
func TestRoutePrefixAppliedToRoutes(t *testing.T) {
	resetRoutePrefix(t)
	if err := setRoutePrefix("/mcp/plugin"); err != nil {
		t.Fatalf("setRoutePrefix: %v", err)
	}
	r := NewRegistry(Config{})
	r.RoutePublic("/confirm", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	got := r.Routes()
	if _, ok := got["/mcp/plugin/confirm"]; !ok {
		t.Fatalf("Routes() 没有带前缀的 pattern，实际 %v", keysOf(got))
	}
	if _, ok := got["/confirm"]; ok {
		t.Errorf("未加前缀的 pattern 不该同时存在：%v", keysOf(got))
	}
}

// TestRoutePathAndPublicURL：插件拼路径与绝对 URL 都要经基座，才能和挂载点同源。
func TestRoutePathAndPublicURL(t *testing.T) {
	resetRoutePrefix(t)
	SetPublicBaseURL("http://10.0.0.1:8013")
	SetSelfAddr("")
	t.Cleanup(func() { SetPublicBaseURL(""); SetSelfAddr("") })
	if err := setRoutePrefix("/mcp/plugin"); err != nil {
		t.Fatalf("setRoutePrefix: %v", err)
	}
	if got, want := RoutePath("/spill/")+"x1", "/mcp/plugin/spill/x1"; got != want {
		t.Errorf("RoutePath = %q, want %q", got, want)
	}
	if got, want := PublicURL("/spill/")+"x1",
		"http://10.0.0.1:8013/mcp/plugin/spill/x1"; got != want {
		t.Errorf("PublicURL = %q, want %q", got, want)
	}
	// 两个地址都没有时 PublicURL 必须是空串，而不是一条只有路径的半截 URL：
	// 后者拼进给模型的文案就是一个点不开的链接，比不给更糟。
	SetPublicBaseURL("")
	if got := PublicURL("/spill/"); got != "" {
		t.Errorf("没有对外地址时 PublicURL = %q, want 空串", got)
	}
}

// TestPublicBaseURLOnlyAffectsLinks：对外入口只改交给外部的链接，不动副本身份。
//
// 为什么要分成两项：SelfAddr 必须是副本自己的直连地址（Pod IP 之类），否则 id 里没有区分
// 副本的信息、owner 路由失效；但那类地址往往不在 agent 或人的可达范围内，链接拼得出来也
// 点不开。属主信息在 id 里，请求落到任意副本都会被反代到属主，所以对外链接可以是域名/VIP。
// 这条同时守住「两个 Config 字段真的都被接线了」——配置项没接线就是静默失效。
func TestPublicBaseURLOnlyAffectsLinks(t *testing.T) {
	resetRoutePrefix(t)
	t.Cleanup(func() { SetSelfAddr(""); SetPublicBaseURL("") })

	r := New(Config{
		ConfigPath:    writeTokenConfig(t, okTokenConfig),
		SelfAddr:      "10.1.2.3:8011",
		PublicBaseURL: "https://mcp.example.com/",
	}, nil)
	t.Cleanup(func() { r.RunStop(context.Background()) })
	// 走宿主的真实入口：Mount 一次同时定下前缀与挂载点。
	if err := r.Mount("/mcp", func(string, http.Handler) {}); err != nil {
		t.Fatalf("Mount: %v", err)
	}

	if got, want := PublicURL("/spill/")+"x1",
		"https://mcp.example.com/mcp/plugin/spill/x1"; got != want {
		t.Errorf("PublicURL = %q, want %q（末尾斜杠应被去掉）", got, want)
	}
	if got, want := SelfHostPort(), "10.1.2.3:8011"; got != want {
		t.Errorf("SelfHostPort = %q, want %q：对外入口不该影响副本身份", got, want)
	}

	// 不配对外入口时由副本身份推导（内网直连部署的默认形态）。
	SetPublicBaseURL("")
	if got, want := PublicURL("/spill/"), "http://10.1.2.3:8011/mcp/plugin/spill/"; got != want {
		t.Errorf("回退后的 PublicURL = %q, want %q", got, want)
	}
}

// TestNoRoutePrefixKeepsPatternsAsIs：不声明前缀就是原行为（挂在根上）。
func TestNoRoutePrefixKeepsPatternsAsIs(t *testing.T) {
	resetRoutePrefix(t)
	r := NewRegistry(Config{})
	r.RoutePublic("/healthz", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if _, ok := r.Routes()["/healthz"]; !ok {
		t.Errorf("没声明前缀时 pattern 应原样保留，实际 %v", keysOf(r.Routes()))
	}
	if got := RoutePath("/spill/"); got != "/spill/" {
		t.Errorf("RoutePath = %q, want /spill/", got)
	}
}

// TestInvalidMountPrefixFailsStartup：挂载前缀写错必须让 Mount 失败，且一条路由都不挂。
//
// 不能容错纠正（比如自动补斜杠）：前缀写错的现象是外部回调 404，而 404 不指向前缀。
// 宁可起不来，也不要起来之后一条公开回调静默不可达。
func TestInvalidMountPrefixFailsStartup(t *testing.T) {
	for _, prefix := range []string{"mcp", "/mcp/", "/mcp/*", "/mcp plugin"} {
		resetRoutePrefix(t)
		r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
		mounted := 0
		err := r.Mount(prefix, func(string, http.Handler) { mounted++ })
		r.RunStop(context.Background())
		if err == nil {
			t.Errorf("非法前缀 %q 却挂载成功", prefix)
			continue
		}
		if !strings.Contains(err.Error(), "挂载前缀") {
			t.Errorf("非法前缀 %q 的报错没点名挂载前缀: %v", prefix, err)
		}
		if mounted != 0 {
			t.Errorf("非法前缀 %q 仍挂上了 %d 条路由", prefix, mounted)
		}
	}
}

func keysOf(m map[string]http.Handler) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
