package runtime

import (
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
	r := NewRegistry(Config{RoutePrefix: "/mcp/plugin"})
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
	t.Cleanup(func() { SetPublicBaseURL("") })
	NewRegistry(Config{RoutePrefix: "/mcp/plugin"})
	if got, want := RoutePath("/spill/")+"x1", "/mcp/plugin/spill/x1"; got != want {
		t.Errorf("RoutePath = %q, want %q", got, want)
	}
	if got, want := PublicURL("/spill/")+"x1",
		"http://10.0.0.1:8013/mcp/plugin/spill/x1"; got != want {
		t.Errorf("PublicURL = %q, want %q", got, want)
	}
	// 没有对外地址时 PublicURL 必须是空串，而不是一条只有路径的半截 URL：
	// 后者拼进给模型的文案就是一个点不开的链接，比不给更糟。
	SetPublicBaseURL("")
	if got := PublicURL("/spill/"); got != "" {
		t.Errorf("没有对外地址时 PublicURL = %q, want 空串", got)
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

// TestInvalidRoutePrefixFailsStartup：前缀写错必须启动失败。
//
// 不能容错纠正（比如自动补斜杠）：前缀写错的现象是外部回调 404，而 404 不指向前缀。
// 宁可起不来，也不要起来之后一条公开回调静默不可达。
func TestInvalidRoutePrefixFailsStartup(t *testing.T) {
	for _, prefix := range []string{"mcp/plugin", "/mcp/plugin/", "/mcp/*", "/mcp plugin"} {
		resetRoutePrefix(t)
		r := New(Config{RoutePrefix: prefix, ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
		_, _, err := r.Handlers()
		if err == nil {
			t.Errorf("非法前缀 %q 却启动成功", prefix)
			continue
		}
		if !strings.Contains(err.Error(), "RoutePrefix") {
			t.Errorf("非法前缀 %q 的报错没点名 RoutePrefix: %v", prefix, err)
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
