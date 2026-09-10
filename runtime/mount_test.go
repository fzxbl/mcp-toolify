package runtime

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// TestMountHandsHostTheActualPaths：Mount 交给宿主的 pattern 必须是**加好前缀的实际路径**，
// 且与基座自己推导的绝对 URL / 转发目标同源。
//
// 这是「宿主只说一次挂在哪」的核心断言：宿主拿到的 pattern 原样挂上去之后，
// PublicURL 与副本间转发必然指向同一条路径——没有第二处可以写歪。
func TestMountHandsHostTheActualPaths(t *testing.T) {
	resetRoutePrefix(t)
	SetSelfAddr("10.1.2.3:8011")
	t.Cleanup(func() { SetSelfAddr("") })
	ResetOwnerRoutedPathsForTest()
	t.Cleanup(ResetOwnerRoutedPathsForTest)

	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	t.Cleanup(func() { r.RunStop(context.Background()) })
	r.RoutePublic("/confirm", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r.Route("/spill/", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	RegisterOwnerRoutedRoute("/spill/", func(*http.Request) string { return "" })

	var mounted []string
	if err := r.Mount("/mcp", func(pattern string, h http.Handler) {
		if h == nil {
			t.Errorf("pattern %s 的 handler 为 nil", pattern)
		}
		mounted = append(mounted, pattern)
	}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	sort.Strings(mounted)
	want := []string{"/mcp", "/mcp/plugin/confirm", "/mcp/plugin/spill/"}
	if strings.Join(mounted, ",") != strings.Join(want, ",") {
		t.Fatalf("挂载的 pattern = %v，want %v（MCP 端点在前缀本身，插件在 /plugin 之下）",
			mounted, want)
	}
	// 同一个前缀的另外两个下游：给 agent 的 URL 与副本间转发的目标路径。
	if got, want := PublicURL("/spill/"), "http://10.1.2.3:8011/mcp/plugin/spill/"; got != want {
		t.Errorf("PublicURL = %q, want %q", got, want)
	}
	if got, want := RoutePath("/spill/"), "/mcp/plugin/spill/"; got != want {
		t.Errorf("RoutePath = %q, want %q", got, want)
	}
}

// TestMountRejectsOwnerRoutedPatternWithoutRoute：登记了 owner 路由却没注册对应路由时，
// Mount 必须启动失败。
//
// 登记与注册路由是插件里两行独立的代码。写歪一处（pattern 拼错、路由改名没同步）在单副本
// 部署下毫无症状，多副本上线后才表现为「点了确认没反应 / 结果取不回来」——所以这一条要在
// 启动期就把两张表比对出来。
func TestMountRejectsOwnerRoutedPatternWithoutRoute(t *testing.T) {
	resetRoutePrefix(t)
	ResetOwnerRoutedPathsForTest()
	t.Cleanup(ResetOwnerRoutedPathsForTest)

	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	t.Cleanup(func() { r.RunStop(context.Background()) })
	r.Route("/spill/", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	// 登记的是 /spil/（少一个 l）：真实部署里就是这种拼错。
	RegisterOwnerRoutedRoute("/spil/", func(*http.Request) string { return "" })

	err := r.Mount("/mcp", func(string, http.Handler) {})
	if err == nil {
		t.Fatal("登记的 pattern 没有对应路由，Mount 却成功了")
	}
	if !strings.Contains(err.Error(), "/spil/") {
		t.Errorf("报错没点名拼错的 pattern: %v", err)
	}
}

// TestMountAtRootKeepsPathsBare：不给前缀时挂在根上——MCP 端点是 "/"，插件在 /plugin 之下。
func TestMountAtRootKeepsPathsBare(t *testing.T) {
	resetRoutePrefix(t)
	ResetOwnerRoutedPathsForTest()
	t.Cleanup(ResetOwnerRoutedPathsForTest)

	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	t.Cleanup(func() { r.RunStop(context.Background()) })
	r.RoutePublic("/healthz", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	var mounted []string
	if err := r.Mount("", func(pattern string, _ http.Handler) {
		mounted = append(mounted, pattern)
	}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	sort.Strings(mounted)
	if want := []string{"/", "/plugin/healthz"}; strings.Join(mounted, ",") != strings.Join(want, ",") {
		t.Fatalf("挂载的 pattern = %v，want %v", mounted, want)
	}
}

// TestMountRequiresMountFunc：不给挂载动作直接报错，而不是静默什么都不挂。
func TestMountRequiresMountFunc(t *testing.T) {
	resetRoutePrefix(t)
	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	t.Cleanup(func() { r.RunStop(context.Background()) })
	if err := r.Mount("/mcp", nil); err == nil {
		t.Fatal("MountFunc 为 nil 却成功了")
	}
}
