package runtime

import (
	"fmt"
	"strings"
	"sync"
)

// 本文件是「插件 HTTP 路由前缀由上层应用定义」这一能力（Config.RoutePrefix）。
//
// 插件只声明自己的 pattern（spill 的 /spill/），挂在哪个前缀之下由宿主决定。
// 基座把前缀施加到三处：Routes() 交出的 pattern、插件给 agent 的绝对 URL（PublicURL）、
// 插件转发到属主副本的目标路径（RoutePath）。三处同源，否则「URL 能下、跨副本 404」
// 这类差异只在多副本部署下才暴露。
//
// 前缀是进程级状态（与 PublicBaseURL / Peers 同一取舍）：插件在 Install 期就要用它，
// 那时 Registry 之外还没有可用的载体；一个进程一个基座。

var (
	routePrefixMu sync.RWMutex
	routePrefix   string
)

// setRoutePrefix 校验并设置前缀；非法前缀返回 error（由 NewRegistry 记成启动失败）。
// 未导出：唯一来源是 Config.RoutePrefix。路由一旦挂上宿主的 mux 就固定了，
// 运行期改前缀只会让转发与 URL 指向不存在的路径。
func setRoutePrefix(prefix string) error {
	if err := validateRoutePrefix(prefix); err != nil {
		routePrefixMu.Lock()
		routePrefix = ""
		routePrefixMu.Unlock()
		return err
	}
	routePrefixMu.Lock()
	routePrefix = prefix
	routePrefixMu.Unlock()
	return nil
}

// validateRoutePrefix 拒绝会静默变成 404 的写法，不做容错纠正：
// 前缀写错的现象是外部请求 404，而 404 不指向前缀。宁可启动失败，让部署方立刻看到原因。
func validateRoutePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	switch {
	case !strings.HasPrefix(prefix, "/"):
		return fmt.Errorf("Config.RoutePrefix %q 必须以 / 开头", prefix)
	case strings.HasSuffix(prefix, "/"):
		return fmt.Errorf("Config.RoutePrefix %q 不能以 / 结尾"+
			"（插件的 pattern 自带前导斜杠，否则会拼出双斜杠）", prefix)
	case strings.ContainsAny(prefix, "*? \t"):
		return fmt.Errorf("Config.RoutePrefix %q 不能含通配符或空白"+
			"（它是一段字面路径，通配由宿主 mount 时自己加）", prefix)
	}
	return nil
}

// RoutePrefix 返回生效的插件路由前缀；未声明时为空串。
func RoutePrefix() string {
	routePrefixMu.RLock()
	defer routePrefixMu.RUnlock()
	return routePrefix
}

// RoutePath 把插件自己的 pattern 换算成对外的绝对路径。
// 插件凡是要说出「这个端点在哪」的地方都用它：副本间转发的目标路径、日志。
// 直接用 pattern 等于假设挂在根上。
func RoutePath(pattern string) string {
	return RoutePrefix() + pattern
}

// PublicURL 返回某 pattern 的对外绝对地址（PublicBaseURL + 前缀 + pattern）。
// 没有对外地址时返回空串（而不是半截 URL：拼进给模型的文案就是一个点不开的链接），
// 调用方自行判空。
func PublicURL(pattern string) string {
	base := PublicBaseURL()
	if base == "" {
		return ""
	}
	return base + RoutePath(pattern)
}

// RegisterOwnerRoutedRoute 与 RegisterOwnerRoutedPath 等价，但传插件自己的 pattern，
// 前缀由基座补齐。插件登记 owner 路由都该用它：匹配的是请求的实际路径，
// 裸 pattern 在有前缀时永远匹配不上，而那种漏配只在多副本部署下暴露。
func RegisterOwnerRoutedRoute(pattern string, fn PathOwnerExtractor) {
	RegisterOwnerRoutedPath(RoutePath(pattern), fn)
}
