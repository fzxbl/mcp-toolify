// Package toolify 把带 `// mcp:tool` 标记的 Go 函数暴露为 MCP（Model Context
// Protocol）工具。生成器（cmd/mcpgen）扫描注解生成 wrapper 代码，本包提供把这些
// wrapper 挂到 MCP server 上运行的入口（stdio / http），并附带连接级鉴权、
// 大返回结果落盘（spill）与调用审计等运行时能力。
//
// 典型用法：
//
//	//go:generate go run github.com/fzxbl/mcp-toolify/cmd/mcpgen -config ./mcpgen.yaml
//	func main() {
//	    _ = toolify.Start(context.Background(), toolify.Config{Transport: "stdio"}, tools.RegisterAll)
//	}
package toolify

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// Config 是 server 启动配置（runtime.Config 的别名，外部只需 import 本包）。
type Config = runtime.Config

// RegisterOptions 控制启用哪些生成的工具（按包名/标签过滤）。
type RegisterOptions = runtime.RegisterOptions

// Registrar 是生成代码暴露的注册函数类型（通常是生成的 tools.RegisterAll）。
type Registrar = runtime.Registrar

// Logger 是 MCP 调用审计日志接口；不注入时回退到标准 log。
type Logger = runtime.Logger

// SetAuditLogger 注入调用审计 logger。应在启动/挂载 server 前调用。
func SetAuditLogger(l Logger) { runtime.SetAuditLogger(l) }

// SetSpillPeerProvider 注册 spill 跨副本代理的动态兄弟副本发现函数（返回可达 host:port 列表）。
// 多副本部署时用它对接任意服务发现作为代理转发白名单来源，无需静态配置；传 nil 清除。
// 与 SetSpillPeers 互为覆盖，后调用者生效。应在挂载/启动 server 前调用。
func SetSpillPeerProvider(fn func() []string) { runtime.SetSpillPeerProvider(fn) }

// SetSpillPeers 设置静态兄弟副本白名单（host:port）；简单部署可用它替代 provider。
// 与 SetSpillPeerProvider 互为覆盖，后调用者生效。
func SetSpillPeers(hosts []string) { runtime.SetSpillPeers(hosts) }

// RegisterOwnerRouted 声明「工具按某 owned-id 参数路由」，供有状态工具接入分布式层。
func RegisterOwnerRouted(toolName, paramName string) {
	runtime.RegisterOwnerRouted(toolName, paramName)
}

// WithOwnerRouting 包裹 MCP handler：把归属兄弟副本的 tools/call 反代到属主 /mcp。
// 挂载到既有 HTTP server 时套在 MCP handler 外层。
func WithOwnerRouting(next http.Handler) http.Handler { return runtime.WithOwnerRouting(next) }

// OwnerOf 解出 owned id 的属主 host:port；ok=false 表示旧式/无归属 id。
func OwnerOf(id string) (string, bool) { return runtime.OwnerOf(id) }

// Start 用给定 registrar 启动 MCP server，阻塞直到 ctx 取消或 server 退出。
func Start(ctx context.Context, cfg Config, registrar Registrar) error {
	return runtime.Run(ctx, cfg, registrar)
}

// Handlers 构造用于挂载到既有 HTTP server 的两个 http.Handler：
//   - mcpHandler：MCP 协议端点（Streamable HTTP），可挂在任意子路径（如 /mcp）。
//   - spillHandler：大返回结果的临时下载端点（默认 /spill/<id>）。
//
// 与 Start 不同，Handlers 不自己监听端口，而是把 handler 交给调用方挂载到已有的
// HTTP server 上，从而与主服务共用同一端口与生命周期。
//
// extra 为可选的额外工具注册器：除生成的 registrar 外，把外部模块的工具注册到
// 同一个 server。注意：外部工具需另行调用 RegisterToolMeta 登记风险等级，
// 否则在 authz 中按“未登记默认放行”处理。
//
// 若 cfg.PublicBaseURL 非空，会设置 spill 下载端点的对外基础地址（跨机部署时 agent
// 直连下载用）。
func Handlers(cfg Config, registrar Registrar, extra ...func(*mcp.Server)) (mcpHandler, spillHandler http.Handler, err error) {
	s := runtime.BuildServer(cfg, func(s *mcp.Server, opts RegisterOptions) {
		registrar(s, opts)
		for _, e := range extra {
			if e != nil {
				e(s)
			}
		}
	})
	if cfg.PublicBaseURL != "" {
		runtime.SetSpillBaseURL(cfg.PublicBaseURL)
	}
	h, err := runtime.MCPHandler(cfg, s)
	if err != nil {
		return nil, nil, err
	}
	return h, runtime.SpillHandler(), nil
}

// RegisterToolMeta 为“非注解生成、运行时注册”的外部工具登记鉴权元数据（能力+风险）。
// write=true 表示写操作（ReadWrite），否则只读；risk 取 none|low|medium|high。
// 必须在处理请求前调用，否则该工具在 authz 里按“未登记默认放行”处理。
func RegisterToolMeta(name string, write bool, risk string) {
	capab := runtime.ReadOnly
	if write {
		capab = runtime.ReadWrite
	}
	r, _ := runtime.ParseRisk(risk)
	runtime.RegisterMeta(runtime.ToolMeta{Name: name, Capability: capab, Risk: r})
}
