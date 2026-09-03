// Package toolify 把带 `// mcp:tool` 标记的 Go 函数暴露为 MCP（Model Context
// Protocol）工具。生成器（cmd/mcpgen）扫描注解生成 wrapper 代码，本包提供把这些
// wrapper 组装成一个 HTTP MCP server 的最小基座：MCP 协议、带不透明 labels 的工具
// 注册表、label selector 引擎、token 认证与准入、logid、owner 路由和一条中间件链。
//
// 审计、大结果落盘（spill）、配额、二次确认等能力都是独立插件，按需 Install；
// 不 Install 的插件不进编译产物。
//
// 典型用法：
//
//	//go:generate go run github.com/fzxbl/mcp-toolify/cmd/mcpgen -config ./mcpgen.yaml
//	func main() {
//	    r := toolify.New(toolify.Config{Addr: ":8080", ConfigPath: "mcp.toml"}, tools.RegisterAll)
//	    // audit.Install(r) / spill.Install(r) ... 顺序即中间件链的进入顺序
//	    _ = r.Start(context.Background())
//	}
package toolify

import (
	"net/http"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// Config 是 server 启动配置（runtime.Config 的别名，外部只需 import 本包）。
type Config = runtime.Config

// RegisterOptions 控制启用哪些生成的工具（按包名 / label selector 过滤）。
type RegisterOptions = runtime.RegisterOptions

// Registrar 是生成代码暴露的注册函数类型（通常是生成的 tools.RegisterAll）。
type Registrar = runtime.Registrar

// Registry 是插件的唯一依赖类型：注册中间件、MCP 工具、HTTP 路由与清理钩子。
type Registry = runtime.Registry

// Call 是一次 MCP 请求的上下文，插件中间件的入参。
type Call = runtime.Call

// Subject 是调用主体（人 + token 用途名 + 插件自定义标注）。
type Subject = runtime.Subject

// Result 是一次调用的结果。
type Result = runtime.Result

// Handler 是中间件链上的一环。
type Handler = runtime.Handler

// Middleware 是插件唯一的注入点。
type Middleware = runtime.Middleware

// New 构造 Registry 并注册生成的工具，返回值交给各插件 Install。
func New(cfg Config, registrar Registrar) *Registry { return runtime.New(cfg, registrar) }

// SetPeerProvider 注册 owner 路由的动态兄弟副本发现函数（返回可达 host:port 列表）。
// 多副本部署时用它对接任意服务发现作为反代白名单来源，无需静态配置；传 nil 清除。
// 与 SetPeers 互为覆盖，后调用者生效。应在启动 server 前调用。
func SetPeerProvider(fn func() []string) { runtime.SetPeerProvider(fn) }

// SetPeers 设置静态兄弟副本白名单（host:port）；简单部署可用它替代 provider。
// 与 SetPeerProvider 互为覆盖，后调用者生效。
func SetPeers(hosts []string) { runtime.SetPeers(hosts) }

// RegisterOwnerRouted 声明「工具按某 owned-id 参数路由」，供有状态插件接入分布式层。
func RegisterOwnerRouted(toolName, paramName string) {
	runtime.RegisterOwnerRouted(toolName, paramName)
}

// RegisterOwnerRoutedPath 声明「某 HTTP 路由前缀下的请求按路径里的 owned id 路由」，
// 供有状态插件的 HTTP 回调（带外确认、下载）接入分布式层。
//
// prefix 是**请求的实际路径前缀**。宿主用 Config.RoutePrefix 给插件路由加了前缀时，
// 插件应改用 RegisterOwnerRoutedRoute（传自己的 pattern，由基座补前缀）。
func RegisterOwnerRoutedPath(prefix string, fn runtime.PathOwnerExtractor) {
	runtime.RegisterOwnerRoutedPath(prefix, fn)
}

// RegisterOwnerRoutedRoute 同上，但前缀用插件自己的 pattern 表达，由基座补上
// Config.RoutePrefix —— 插件不必知道宿主把它挂在哪。
func RegisterOwnerRoutedRoute(pattern string, fn runtime.PathOwnerExtractor) {
	runtime.RegisterOwnerRoutedRoute(pattern, fn)
}

// RoutePath 把插件自己的 pattern 换算成对外绝对路径（含 Config.RoutePrefix）。
func RoutePath(pattern string) string { return runtime.RoutePath(pattern) }

// PublicURL 返回某 pattern 的对外绝对地址；没有对外地址时为空串。
func PublicURL(pattern string) string { return runtime.PublicURL(pattern) }

// WithOwnerRouting 包裹 MCP handler：把归属兄弟副本的 tools/call 反代到属主副本。
// 基座已在内部装好，仅在自定义组装时需要。
func WithOwnerRouting(next http.Handler) http.Handler { return runtime.WithOwnerRouting(next) }

// NewOwnedID 生成内嵌本副本 host:port 的不透明 id，供有状态插件使用。
func NewOwnedID() string { return runtime.NewOwnedID() }

// OwnerOf 解出 owned id 的属主 host:port；ok=false 表示无归属 id。
func OwnerOf(id string) (string, bool) { return runtime.OwnerOf(id) }

// RegisterToolMeta 为「非注解生成、运行时注册」的外部工具登记 labels（准入判据）。
// 必须在处理请求前调用：未登记 labels 的工具在 token 准入里一律不可见、不可执行
// （deny-by-default）。labels 的 key 语义由配置里的 selector 与插件解释，基座不解释。
func RegisterToolMeta(name, pkg string, labels map[string]string) {
	runtime.RegisterTool(runtime.ToolInfo{Name: name, Pkg: pkg, Labels: labels})
}
