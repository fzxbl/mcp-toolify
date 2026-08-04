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
