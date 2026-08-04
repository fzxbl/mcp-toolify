package runtime

import (
	"context"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// BuildServer 构造并配置好一个 MCP server（注册工具、spill 资源、日志中间件），
// 供 HTTP 挂载（MCPHandler）或独立启动（Run）复用。
func BuildServer(cfg Config, registrar Registrar) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-toolify",
		Version: "0.1.0",
	}, nil)
	registrar(s, RegisterOptions{Enable: cfg.Enable, Tags: cfg.Tags})
	InitSpillStore(context.Background(), cfg.SpillDir, spillTTLConfig{})
	InitSpillConfig(cfg.ConfigPath)
	RegisterSpillResource(s)
	runStartupHooks(context.Background())
	s.AddReceivingMiddleware(LoggingMiddleware())
	return s
}

// MCPHandler 返回处理 MCP 协议（Streamable HTTP）的 http.Handler，用于挂载到既有
// HTTP server。该 handler 不依赖具体请求路径，可挂在任意子路径。
//
// cfg.AuthzEnabled 为 true 时叠加连接级能力 + 调用级风险鉴权，鉴权配置读取 cfg.ConfigPath。
func MCPHandler(cfg Config, s *mcp.Server) (http.Handler, error) {
	if cfg.AuthzEnabled {
		if cfg.ConfigPath == "" {
			return nil, fmt.Errorf("AuthzEnabled=true but ConfigPath is empty")
		}
		authzCfg, err := LoadAuthzConfig(cfg.ConfigPath)
		if err != nil {
			return nil, fmt.Errorf("load mcp authz config: %w", err)
		}
		if err := authzCfg.Validate(); err != nil {
			return nil, fmt.Errorf("invalid mcp authz config: %w", err)
		}
		return NewAuthzHandler(s, NewAuthz(authzCfg)), nil
	}
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil), nil
}

// SpillHandler 返回 /spill/<id> 大结果下载端点的 http.Handler。
// 与 MCPHandler 挂在同一个 HTTP server 上即可供 agent 直连下载。
func SpillHandler() http.Handler {
	return SpillDownloadHandler()
}
