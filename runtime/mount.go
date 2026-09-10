package runtime

import (
	"fmt"
	"net/http"
	"sort"
)

// 本文件是「宿主随便挂，只说一次」这件事：宿主把挂载动作交给基座，基座在同一次调用里
// 既知道了自己被挂在哪、又把每条路由交给宿主的 router。
//
// 挂载前缀只有这一个来源。给 agent 的绝对 URL 与副本间转发目标都据它推导，因此不会出现
// 「两处前缀对不上、外部请求 404」这类只在换挂载点或多副本部署时才暴露的问题。

// MountFunc 是宿主把一条路由挂到自己 router 上的动作。
//
// pattern 是**对外绝对路径**（基座已按挂载前缀算好），h 已经包好基座该包的一切
// （token 认证、logid、owner 路由）。宿主只负责按自己 router 的写法挂上去，例如：
//
//	// net/http
//	func(pattern string, h http.Handler) { mux.Handle(pattern, h) }
//	// 需要通配符的 router（如 ghttp）
//	func(pattern string, h http.Handler) { router.HandleStd("ANY", pattern+"*", h) }
//
// 不要在这里改写 pattern（再套 StripPrefix、再加一层前缀）：基座据此推导的绝对 URL 与
// 转发目标就会和实际挂载点脱节。要换挂载点就改 Mount 的 prefix。
type MountFunc func(pattern string, h http.Handler)

// Mount 把 MCP 端点与全部插件路由挂到宿主的 router 上，同时把「宿主挂在哪」告诉基座。
//
// prefix 是本基座的挂载前缀（如 "/mcp"；空串表示挂在根上）。布局固定：
//   - MCP 端点：prefix 本身（空前缀时是 "/"）；
//   - 插件路由：prefix + "/plugin" 之下，例如 prefix="/mcp" 时是 /mcp/plugin/spill/。
//
// 一次调用完成三件事：校验并记下前缀 → 组装 handler（含插件 build 期自检）→ 逐条挂载。
// 因此宿主只需说一次「你挂在哪」，基座据此得到的实际路径、给 agent 的绝对 URL、副本间
// 转发目标必然同源。
//
// 返回 error 即启动失败：前缀非法、插件自检没过、或某个插件登记的 owner 路由与它注册的
// 路由对不上（那会让多副本下的回调/下载静默落到非属主副本）。
func (r *Registry) Mount(prefix string, mount MountFunc) error {
	if mount == nil {
		return fmt.Errorf("%s", "runtime.Mount 需要非空 MountFunc")
	}
	if err := validateRoutePrefix(prefix); err != nil {
		return err
	}
	// 插件路由收在 prefix/plugin 之下：一个前缀就够宿主声明，插件自己的 pattern 与
	// 「MCP 端点在哪」都不需要它知道。
	if err := setRoutePrefix(prefix + pluginSubPrefix); err != nil {
		return err
	}
	h, routes, err := r.Handlers()
	if err != nil {
		return err
	}
	if err := checkOwnerRoutedPatternsMounted(routes); err != nil {
		return err
	}
	mcpPattern := prefix
	if mcpPattern == "" {
		mcpPattern = "/"
	}
	mount(mcpPattern, h)
	for pattern, rh := range routes {
		mount(pattern, rh)
	}
	return nil
}

// pluginSubPrefix 是插件路由相对基座挂载点的固定子前缀。
//
// 固定而不可配：它的唯一作用是把插件路由与 MCP 端点分开，让宿主可以用一条通配规则挂
// MCP 端点而不吞掉插件路由。多一个可配项只会多一处两边写歪的机会。
const pluginSubPrefix = "/plugin"

// checkOwnerRoutedPatternsMounted 校验「登记了 owner 路由的 pattern」都真的有对应的路由。
//
// 为什么在启动期查：登记与注册路由是插件里两行独立的代码，写歪一处（pattern 拼错、路由
// 改名没同步登记）在单副本部署下毫无症状——多副本上线后才表现为「点了确认没反应 / 结果
// 取不回来」。这里比对的是两张表的实际内容，因此拼错必然被抓到。
func checkOwnerRoutedPatternsMounted(routes map[string]http.Handler) error {
	var missing []string
	for _, pattern := range ownerRoutedPatterns() {
		if _, ok := routes[RoutePath(pattern)]; !ok {
			missing = append(missing, pattern)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("这些 pattern 登记了 owner 路由却没有对应的插件路由 %v："+
		"多副本下它们的回调/下载会落到非属主副本（登记用 RegisterOwnerRoutedRoute，"+
		"注册路由用 Registry.Route / RoutePublic，两处的 pattern 必须一致）", missing)
}
