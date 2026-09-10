package runtime

import (
	"fmt"
	"strings"
	"sync"
)

// 本文件是「插件 HTTP 路由前缀」这一能力：前缀由宿主在 Registry.Mount 里一次给出。
//
// 插件只声明自己的 pattern（spill 的 /spill/），挂在哪个前缀之下由宿主决定。
// 基座把前缀施加到三处：Mount 交给宿主的挂载 pattern、插件给 agent 的绝对 URL
// （PublicURL）、副本间转发的目标路径（RoutePath）。三处同源，否则「URL 能下、跨副本 404」
// 这类差异只在多副本部署下才暴露——而唯一的来源是 Mount 的那一个参数，没有第二处可写歪。
//
// 前缀是进程级状态（与 SelfAddr / Peers 同一取舍）：插件与转发逻辑都要读它，
// 一个进程一个基座。

var (
	routePrefixMu sync.RWMutex
	routePrefix   string
)

// setRoutePrefix 校验并设置前缀；非法前缀返回 error（由 Mount 记成启动失败）。
// 未导出：唯一来源是 Registry.Mount 的 prefix 参数。路由一旦挂上宿主的 router 就固定了，
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

// SetRoutePrefixForTest 直接设置插件路由前缀，**仅测试使用**：包外插件的用例要复现
// 「被宿主挂在某个前缀之下」的形态，而生产里唯一入口是 Registry.Mount（它还要求一份完整
// 的 token 配置）。运行期调用它会让转发与 URL 指向不存在的路径。
func SetRoutePrefixForTest(prefix string) error { return setRoutePrefix(prefix) }

// validateRoutePrefix 拒绝会静默变成 404 的写法，不做容错纠正：
// 前缀写错的现象是外部请求 404，而 404 不指向前缀。宁可启动失败，让部署方立刻看到原因。
func validateRoutePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	switch {
	case !strings.HasPrefix(prefix, "/"):
		return fmt.Errorf("挂载前缀 %q 必须以 / 开头", prefix)
	case strings.HasSuffix(prefix, "/"):
		return fmt.Errorf("挂载前缀 %q 不能以 / 结尾"+
			"（插件的 pattern 自带前导斜杠，否则会拼出双斜杠）", prefix)
	case strings.ContainsAny(prefix, "*? \t"):
		return fmt.Errorf("挂载前缀 %q 不能含通配符或空白"+
			"（它是一段字面路径，通配由宿主在 MountFunc 里自己加）", prefix)
	}
	return nil
}

// currentRoutePrefix 读生效的前缀。未导出：宿主与插件要的是 RoutePath / PublicURL
// 那两个换算函数，拿到裸前缀自己拼只会多一处走偏的机会。
func currentRoutePrefix() string {
	routePrefixMu.RLock()
	defer routePrefixMu.RUnlock()
	return routePrefix
}

// RoutePath 把插件自己的 pattern 换算成对外的绝对路径。
// 插件凡是要说出「这个端点在哪」的地方都用它：副本间转发的目标路径、日志。
// 直接用 pattern 等于假设挂在根上。未声明前缀时补的是空串，与「挂在根上」同一条路径。
func RoutePath(pattern string) string {
	return currentRoutePrefix() + pattern
}

// PublicURL 返回某 pattern 的对外绝对地址（对外入口 + 前缀 + pattern）。
// 对外入口取 PublicBaseURL()：显式配了 Config.PublicBaseURL 就用它（域名/VIP 均可），
// 否则由 SelfAddr 推导。两者都没有时返回空串（而不是半截 URL：拼进给模型的文案就是一个
// 点不开的链接），调用方自行判空。
func PublicURL(pattern string) string {
	base := PublicBaseURL()
	if base == "" {
		return ""
	}
	return base + RoutePath(pattern)
}
