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
// 静态白名单写在 Config.Peers 里，与本函数互为覆盖，后设置者生效。应在启动 server 前调用。
func SetPeerProvider(fn func() []string) { runtime.SetPeerProvider(fn) }

// 有状态插件接入 owner 路由（owned id、按参数/按路由登记、对外 URL）用的是 runtime 包的
// 同名 API：runtime.NewOwnedID / RegisterOwnerRouted / RegisterOwnerRoutedRoute /
// RoutePath / PublicURL。本包不做一层同名转发——插件本来就依赖 runtime.Registry，
// 两套入口并存只会让「该用哪个」变成一次没必要的选择。

// RegisterToolMeta 为「非注解生成、运行时注册」的外部工具登记 labels（准入判据）。
// 必须在处理请求前调用：未登记 labels 的工具在 token 准入里一律不可见、不可执行
// （deny-by-default）。labels 的 key 语义由配置里的 selector 与插件解释，基座不解释。
func RegisterToolMeta(name, pkg string, labels map[string]string) {
	runtime.RegisterTool(runtime.ToolInfo{Name: name, Pkg: pkg, Labels: labels})
}
