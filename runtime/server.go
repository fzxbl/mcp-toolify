package runtime

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Config controls server startup behavior.
type Config struct {
	Transport string   // "stdio" | "http"
	Addr      string   // http listen addr，例如 ":8080"；为空时由系统分配端口
	Enable    []string // package-name whitelist
	Tags      []string // tag whitelist

	// PublicBaseURL 是 agent 侧可直连的对外基础地址（如 http://host:8011）。
	// 跨机部署时必须设置，spill 下载 URL 会基于它拼接；为空时回退到实际监听地址
	// （仅适用于同机/本地场景）。
	PublicBaseURL string

	// ConfigPath 指向包含 [spill] 与 [[tokens]]/[risk_allowlist] 段的 TOML 文件。
	// 为空时：spill 阈值用默认值；若 AuthzEnabled=true 则启动报错（鉴权必须有配置）。
	ConfigPath string

	// SpillDir 是大返回结果落盘目录。为空时用 <os.TempDir>/mcp-toolify/spill。
	SpillDir string

	// AuthzEnabled 为 true 时（仅 http 生效）启用连接级能力 + 调用级风险鉴权，
	// 配置来自 ConfigPath。默认关闭。
	AuthzEnabled bool
}

// Registrar is the function generated tools expose (typically tools.RegisterAll).
type Registrar func(s *mcp.Server, opts RegisterOptions)

// Run starts the MCP server with the given registrar.
//
// 启动后会向标准日志（log 包）打印监听信息：
//   - stdio：打印 "MCP server running on stdio"
//   - http：打印 "MCP server listening on http://<addr>" （含实际端口）
func Run(ctx context.Context, cfg Config, registrar Registrar) error {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-toolify",
		Version: "0.1.0",
	}, nil)

	registrar(s, RegisterOptions{Enable: cfg.Enable, Tags: cfg.Tags})
	InitSpillStore(ctx, cfg.SpillDir, spillTTLConfig{})
	InitSpillConfig(cfg.ConfigPath)
	InitAuditConfig(cfg.ConfigPath)
	InitLogConfig(cfg.ConfigPath)
	RegisterSpillResource(s)
	runStartupHooks(ctx)
	s.AddReceivingMiddleware(LoggingMiddleware())

	switch cfg.Transport {
	case "", "stdio":
		log.Printf("MCP server running on stdio (enable=%v tags=%v)", cfg.Enable, cfg.Tags)
		return s.Run(ctx, &mcp.StdioTransport{})
	case "http":
		return runHTTP(ctx, cfg, s)
	default:
		return fmt.Errorf("unknown transport %q", cfg.Transport)
	}
}

func runHTTP(ctx context.Context, cfg Config, s *mcp.Server) error {
	addr := cfg.Addr
	if addr == "" {
		addr = ":0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	// 对外可直连的基础地址：优先用显式配置的 PublicBaseURL（跨机部署必填），
	// 否则回退到实际监听地址（仅同机/本地场景可用）。
	base := cfg.PublicBaseURL
	if base == "" {
		base = "http://" + ln.Addr().String()
	}
	SetSpillBaseURL(base)

	mux := http.NewServeMux()
	mux.Handle(spillDownloadPath, SpillDownloadHandler()) // /spill/<id> 大结果下载

	var handler http.Handler
	if cfg.AuthzEnabled {
		if cfg.ConfigPath == "" {
			return fmt.Errorf("AuthzEnabled=true but ConfigPath is empty")
		}
		authzCfg, err := LoadAuthzConfig(cfg.ConfigPath)
		if err != nil {
			return fmt.Errorf("load mcp authz config: %w", err)
		}
		if err := authzCfg.Validate(); err != nil {
			return fmt.Errorf("invalid mcp authz config: %w", err)
		}
		handler = NewAuthzHandler(s, NewAuthz(authzCfg)) // stateless：调用级身份透传
	} else {
		handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	}
	mux.Handle("/", HTTPAuditHeaders(handler)) // 其余交给 MCP 传输（先采集审计 header）
	// logid 在最外层解析/生成（注入 ctx 供审计与 access 日志共用），其内是内置接入层
	// access 日志：独立 Run() 启动时提供一份 service 日志，与审计日志靠同一 logid 串联。
	srv := &http.Server{Handler: HTTPLogID(httpAccessLog(mux))}

	log.Printf("MCP server listening on http://%s (enable=%v tags=%v, spill base=%s)",
		ln.Addr().String(), cfg.Enable, cfg.Tags, base)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithCancel(context.Background())
		cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
