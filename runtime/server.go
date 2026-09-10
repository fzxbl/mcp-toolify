package runtime

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Config 控制 server 的启动行为。只保留基座关心的项：
// 监听、对外地址、配置文件、工具注册期过滤、必需插件、兄弟副本白名单。
// spill 阈值、审计 header 这类能力相关配置由对应插件自己从 ConfigPath 解析。
type Config struct {
	// Addr 是 HTTP 监听地址，例如 ":8080"；为空时由系统分配端口。
	Addr string
	// SelfAddr 是**本副本可直连**的 host:port（如 10.1.2.3:8011）。它是副本身份：
	// owned id 内嵌它、owner 路由据它判断 id 归属、副本间反代据它拨号，因此不能填
	// 负载均衡地址（那样所有副本看起来是同一个）。为空时独立启动回退到实际监听地址
	// （仅同机/本地场景可用）；嵌入宿主时必须显式配置。
	SelfAddr string
	// PublicBaseURL 是**交给外部**的入口地址（如 https://mcp.example.com），只用于
	// PublicURL 拼给调用方的绝对链接。可以是域名/VIP——属主信息在 id 里，请求落到任意
	// 副本都会被反代到属主。为空时回退 "http://" + SelfAddr。
	PublicBaseURL string
	// ConfigPath 指向含 [[tokens]] 等段的 TOML 文件。基座 token 鉴权必须配置，
	// 为空即启动失败。
	ConfigPath string
	// Enable 是工具注册期的包名白名单，空表示不过滤。
	Enable []string
	// Match 是工具注册期的 label selector，空表示不过滤；语法错误即启动失败。
	Match string
	// RequiredPlugins 声明必须在场的插件名，缺失即启动失败。
	// 全插件化之后基座不认识「审计」「鉴权」这些概念，忘装插件就是静默放开，
	// 这个声明是唯一的补偿手段。也可写在配置文件的同名顶层键里（两者取并集）。
	RequiredPlugins []string `toml:"required_plugins"`
	// Peers 是静态兄弟副本白名单（host:port），供 owner 路由校验反代目标。
	// 多副本部署通常改用 SetPeerProvider 对接服务发现。
	Peers []string
}

// Registrar 是生成代码暴露的注册函数类型（通常是生成的 tools.RegisterAll）。
type Registrar func(s *mcp.Server, opts RegisterOptions)

// Start 校验插件、组装链、挂载路由并阻塞运行 HTTP server，直到 ctx 取消或 server 退出。
// 退出前执行插件注册的 OnStop 钩子。
func (r *Registry) Start(ctx context.Context) error {
	// 清理过的 Registry 不得再启动（含「listen 失败 → 换端口用同一个 Registry 重试」
	// 这条很自然的宿主写法）：那会起一个插件已被清理的降级服务。理由见 checkStopped。
	if err := r.checkStopped(); err != nil {
		return err
	}
	addr := r.cfg.Addr
	if addr == "" {
		addr = ":0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// 端口占用同样是「起不来」：插件在 Install 里起的协程/连接池必须回收，
		// 否则宿主里一次端口冲突就留下一组永不退出的协程。RunStop 幂等。
		r.RunStop(context.Background())
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	defer ln.Close()

	// 副本身份：优先用显式配置的 SelfAddr（跨机部署必填），否则回退到实际监听地址
	// （仅同机/本地场景可用）。必须在 build 之前设置，插件在 Install 之后可能已经据此
	// 生成过 owned id 或拼过 URL。对外入口（PublicBaseURL）没配就由它推导，见 PublicBaseURL()。
	if r.cfg.SelfAddr == "" {
		SetSelfAddr(ln.Addr().String())
	}

	handler, err := r.build()
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	for pattern, h := range r.Routes() {
		mux.Handle(pattern, h)
	}
	mux.Handle("/", handler)
	srv := &http.Server{Handler: mux}

	log.Printf("[mcp] server listening on http://%s (enable=%v match=%q public=%s)",
		ln.Addr().String(), r.cfg.Enable, r.cfg.Match, PublicBaseURL())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithCancel(context.Background())
		cancel()
		err := srv.Shutdown(shutdownCtx)
		r.RunStop(context.Background())
		return err
	case err := <-errCh:
		r.RunStop(context.Background())
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
