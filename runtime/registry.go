package runtime

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Registry 是插件的唯一依赖类型：注册中间件、MCP 工具、HTTP 路由、清理钩子。
//
// 生命周期：New 构造并注册生成的工具 → 各插件 Install(r) → Start/Handlers 组装并运行。
// 组装之后不应再调用注册类方法（Use/Tool/Route）：链与 handler 已定型，改它没效果。
type Registry struct {
	cfg          Config
	srv          *mcp.Server
	opts         RegisterOptions
	mws          []Middleware
	names        []string
	routes       map[string]http.Handler
	publicRoutes map[string]http.Handler
	onStop       []func()
	// onBuild 是插件注册的 build 期校验钩子：在基座自身的校验全部通过之后、组装出
	// handler（开始接流）之前依次执行，任一钩子报错即启动失败。见 OnBuild 的注释。
	onBuild []func() error

	// err 记录构造期（New）就已确定的启动失败原因，由 Handlers/Start 报出。
	// New 不返回 error（插件安装链要能连写），但坏配置绝不能一路静默到线上。
	err error
	// fileRequired 是从配置文件读到的 required_plugins，与 cfg 里的并集生效。
	fileRequired []string
	// az 是启动期从配置构造的 token 准入器，由 build 填充。
	az *TokenAuthz

	// claimedKeys 是配置文件里已被基座或某个插件解码走的键（点分路径）。
	// 剩下没人认领的键说明段落名/字段名拼错了，build 时启动失败——
	// 「[qouta] 静默变默认值」这种事故没有任何运行期信号。
	claimedKeys map[string]bool
	// configKeys 是配置文件里出现过的全部键，首次解码时记录。
	configKeys []string
	// configCalls / toolCalls 记 Config、Tool 被调用的次数，用于比对 build 期钩子
	// 有没有偷偷读配置（那时配置认领检查已经跑过，多认领的键再没人核对）或加工具
	// （那时工具注册日志已经打过，新加的工具不会出现在里面）。
	configCalls int
	toolCalls   int
	// stopped 记「清理钩子已经跑过」。跑过之后本 Registry 就报废了：插件的连接池已关、
	// 回收协程已停，此时再 build/Start 会得到一个「只读工具能用、受配额管辖的高危工具
	// 永久被拒、落盘与待确认单再也不被回收」的降级服务——比起不来危险得多。
	stopped bool
	// stopOnce 让 RunStop 进程级幂等：启动失败时基座会自己兜一次清理，
	// 宿主的 defer RunStop 不该把钩子再跑一遍。
	stopOnce sync.Once
	// buildOnce 保证插件链只装一次：SDK 的 AddReceivingMiddleware 是追加语义，
	// Handlers 被宿主调两次就会让审计写两条、配额扣两次。
	buildOnce sync.Once
	builtHTTP http.Handler
	buildErr  error
}

// NewRegistry 构造一个空 Registry（不注册任何工具），供测试与自定义组装使用。
//
// 挂载前缀不在这里给：它由宿主在 Registry.Mount 里连同挂载动作一起交出，
// 于是「基座以为自己在哪」与「实际挂在哪」不可能对不上。未经 Mount 时前缀为空
// （挂在根上），Start 与直接用 Handlers 的自定义组装都是这个形态。
func NewRegistry(cfg Config) *Registry {
	r := &Registry{
		cfg: cfg,
		srv: mcp.NewServer(&mcp.Implementation{
			Name:    "mcp-toolify",
			Version: "0.1.0",
		}, nil),
		routes:       map[string]http.Handler{},
		publicRoutes: map[string]http.Handler{},
		claimedKeys:  map[string]bool{},
	}
	return r
}

// New 构造 Registry 并按 cfg 的包白名单 / label selector 注册生成的工具，
// 返回值交给各插件的 Install 安装自己。
//
// cfg.Match 在 registrar 之前就被 Parse：语法错误直接记为启动失败并跳过注册，
// 避免「服务起来了但 tools/list 是空的」这种无痕迹故障。
func New(cfg Config, registrar Registrar) *Registry {
	r := NewRegistry(cfg)
	opts, err := RegisterOptions{Enable: cfg.Enable, Match: cfg.Match}.Compile()
	if err != nil {
		r.err = fmt.Errorf("config match %q: %w", cfg.Match, err)
		return r
	}
	r.opts = opts
	if registrar != nil {
		registrar(r.srv, opts)
	}
	registered, filtered := opts.Counts()
	// 计数是「selector 合法但 label 拼错导致 0 匹配」的唯一线索：Parse 抓不到它。
	log.Printf("[mcp] tools: effective match=%q enable=%v registered=%d filtered=%d",
		cfg.Match, cfg.Enable, registered, filtered)
	return r
}

// RegisterCounts 返回本次注册放行与被过滤的工具数（New 之外的构造方式下均为 0）。
func (r *Registry) RegisterCounts() (registered, filtered int) { return r.opts.Counts() }

// Server 返回底层 MCP server，供插件之外的宿主代码注册手写工具。
// 注意：这样注册的工具必须另行调用 RegisterTool 登记 labels，否则在 token 准入里
// 既不可见也不可执行（deny-by-default）。
func (r *Registry) Server() *mcp.Server { return r.srv }

// Named 声明当前插件名，用于 required_plugins 校验与启动日志。
func (r *Registry) Named(name string) { r.names = append(r.names, name) }

// Config 把 ConfigPath 指向的 TOML 解码到 dst（插件自带段落名），并把解码到的键
// 登记为「已认领」：没人认领的键会在启动时报错，避免段落名或字段名拼错后静默用默认值。
// 文件未配置或不存在时不改动 dst，插件应自带可用默认值。
//
// 插件必须遵守的两条契约。认领制把「配置拼错检查」这项安全属性下沉给了插件，
// 基座无法代为保证——toml.MetaData.Undecoded() 只能看出「哪些键没被解码」，
// 看不出「解到哪去了」：
//
//  1. 禁止用 map 兜底解码自己的段落（map[string]any / map[string]string），
//     必须用具名字段结构体。用 map 时段内**所有**键都会被判为「已解码」，
//     于是 `[quota] bakcend=... limmit=...` 这类拼错被完全吞掉，Handlers() 照常
//     返回 nil error、插件按默认值上线——正是认领制要拦的那类事故。
//  2. 文档里承诺支持的每一个字段都必须在结构体里声明。只声明一部分时，
//     部署方照文档把配置写全反而启动失败（报「无人认领的配置项 [quota.addr ...]」）。
//     换言之：结构体的字段集就是该段落的对外契约，不能比文档窄。
func (r *Registry) Config(dst any) error {
	if r.cfg.ConfigPath == "" {
		return nil
	}
	if _, err := os.Stat(r.cfg.ConfigPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", r.cfg.ConfigPath, err)
	}
	md, err := toml.DecodeFile(r.cfg.ConfigPath, dst)
	if err != nil {
		return fmt.Errorf("decode %s: %w", r.cfg.ConfigPath, err)
	}
	r.configCalls++
	r.claimKeys(md)
	return nil
}

// claimKeys 记录本次解码认领到的键，并在首次调用时记下文件里的全部键。
func (r *Registry) claimKeys(md toml.MetaData) {
	if r.claimedKeys == nil {
		r.claimedKeys = map[string]bool{}
	}
	if r.configKeys == nil {
		for _, k := range md.Keys() {
			r.configKeys = append(r.configKeys, k.String())
		}
	}
	undecoded := map[string]bool{}
	for _, k := range md.Undecoded() {
		undecoded[k.String()] = true
	}
	for _, k := range md.Keys() {
		if s := k.String(); !undecoded[s] {
			r.claimedKeys[s] = true
		}
	}
}

// unclaimedKeys 返回配置文件里没有任何一方解码走的键（按出现顺序）。
func (r *Registry) unclaimedKeys() []string {
	var out []string
	for _, k := range r.configKeys {
		if !r.claimedKeys[k] {
			out = append(out, k)
		}
	}
	return out
}

// Use 注册中间件。注册顺序即洋葱进入顺序（先注册的在最外层）。
func (r *Registry) Use(mw Middleware) { r.mws = append(r.mws, mw) }

// Middlewares 返回已注册的插件中间件（基座内部与测试使用）。
func (r *Registry) Middlewares() []Middleware { return r.mws }

// Tool 注册一个插件自带的 MCP 工具。
func (r *Registry) Tool(add func(s *mcp.Server)) {
	if add != nil {
		r.toolCalls++
		add(r.srv)
	}
}

// Route 注册插件自带的、**需要鉴权**的 HTTP 路由。
// 与 RoutePublic 分成两个方法而不是加一个 bool 参数：每个路由都必须显式表态，
// 漏写一个 public 只会多一层鉴权，漏写一个 bool 却会少一层。
// 同一个 pattern 重复注册即启动失败（见 claimRoute）。
//
// 需要鉴权的端点用它；公开下载、健康检查一类用 RoutePublic。
func (r *Registry) Route(pattern string, h http.Handler) {
	if r.claimRoute(pattern) {
		r.routes[pattern] = h
	}
}

// claimRoute 占用一个路由 pattern；已被占用时记下启动失败原因并返回 false。
//
// 为什么直接拒绝而不是「后者覆盖前者」：两个插件抢同一个 pattern，覆盖掉的那一路
// 从此静默不可达（例如两个插件都注册 /healthz，其中一个的探活永远返回另一个的结果），
// 而 map 赋值不留任何痕迹。重复注册在**任何时刻**都是 bug，与是不是在 build 期钩子里
// 无关，所以拦在注册入口而不是靠 build 期的规模比对（后者对「同条数不同内容」失效）。
func (r *Registry) claimRoute(pattern string) bool {
	_, dupAuth := r.routes[pattern]
	_, dupPublic := r.publicRoutes[pattern]
	if !dupAuth && !dupPublic {
		return true
	}
	kind := "需鉴权"
	if dupPublic {
		kind = "免鉴权"
	}
	if r.err == nil {
		r.err = fmt.Errorf("HTTP 路由 %q 被重复注册（已存在一条%s的同名路由）："+
			"后注册者若覆盖前者，被覆盖的那一路从此静默不可达，故拒绝启动", pattern, kind)
	}
	log.Printf("[mcp] error: HTTP 路由 %q 重复注册，已拒绝（已存在%s的同名路由）", pattern, kind)
	return false
}

// RoutePublic 注册**不需要鉴权**的 HTTP 路由（健康检查、探活这类）。
// 与 Route 分成两个方法而不是加一个 bool 参数：每个路由都必须显式表态，
// 漏写一个 public 只会多一层鉴权，漏写一个 bool 却会少一层。
// 同一个 pattern 重复注册即启动失败（见 claimRoute）。
func (r *Registry) RoutePublic(pattern string, h http.Handler) {
	if r.claimRoute(pattern) {
		r.publicRoutes[pattern] = h
	}
}

// Routes 返回插件路由，需鉴权的已套上认证层（基座挂载与测试使用）。
// 必须在配置加载之后调用；未加载时 fail-closed 返回空 map。
//
// pattern 是对外的绝对路径（已按宿主 Mount 给出的挂载前缀加好前缀），宿主原样 mount 即可。
//
// 每条路由都再套一层 WithPathOwnerRouting：插件路由上的 id 常常是有归属的
// （如 spill 的 /spill/<id>），而资源只存在于产出它的那个副本。少了这一层，
// 多副本部署下「请求被负载均衡打到别的副本」就是一次静默失败。
// 顺序与 MCP 端点一致（认证在外、owner 路由在内）：反代出去的请求会被属主再认证一次。
func (r *Registry) Routes() map[string]http.Handler {
	out := make(map[string]http.Handler, len(r.routes)+len(r.publicRoutes))
	for pattern, h := range r.publicRoutes {
		out[RoutePath(pattern)] = WithPathOwnerRouting(h)
	}
	if len(r.routes) == 0 {
		return out
	}
	if r.az == nil {
		// 只可能出现在「绕过 build 直接调 Routes」的路径上。宁可少挂路由，
		// 也不能把需要鉴权的路由无保护地交出去。
		log.Printf("[mcp] warning: token 准入尚未初始化，%d 条需鉴权的插件路由被跳过", len(r.routes))
		return out
	}
	for pattern, h := range r.routes {
		out[RoutePath(pattern)] = r.az.HTTPMiddleware(WithPathOwnerRouting(h))
	}
	return out
}

// PluginNames 返回已安装插件的名字（**外→内**，即安装顺序）。
// 与 `plugin chain (outer→inner)` 启动日志同源，供宿主自检链序——
// 「A 装在 B 之内还是之外」这类顺序约束只有断言得到名单才守得住。
func (r *Registry) PluginNames() []string {
	return append([]string{}, r.names...)
}

// OnBuild 注册 build 期校验钩子：在基座自身的校验（loadBase / 配置认领检查 /
// validate）全部通过之后、组装出 handler 之前执行，任一钩子返回 error 即启动失败。
//
// 存在的理由（都是插件在 Install 里做不到的）：
//
//  1. **消除「Install 期校验」的假阳性**：使用方注入的回调（如 audit 的 sink）允许在
//     Install 之后、开始接流之前才注册，Install 里检查「有没有注册」会把这种合法用法
//     误判成启动失败。
//  2. **把「注册时机晚于接流」的空窗堵在接流前**，而不是只靠运行期兜底：
//     插件的运行期兜底（deny）仍必须保留，两层不是二选一——OnBuild 只覆盖
//     「启动之前就能看出来」的那一半，运行期把回调置回 nil 这类情况只有兜底管得住。
//  3. 钩子能看到最终生效的全局状态（如 PublicBaseURL），Install 时它还可能未定型。
//
// 语义：按注册顺序执行，首个报错或 panic 即中止（后面的钩子不再跑）；随 build 一起
// 幂等，一次进程生命周期内恰好执行一次；nil 钩子被忽略。钩子 panic 会被 recover 并
// 转成启动失败（理由见 runOnBuild）。
//
// 钩子里**只做校验与日志**：注册类调用（Use / Tool / Route / RoutePublic / Named /
// Config / OnBuild）在这里一律被拒绝——runOnBuild 前后比对注册表状态，变了就启动失败。
// 为什么拒绝而不是「文档里劝一句」：这些调用此刻的表现各不相同且**全是静默的**
// —— 此时链还没装，钩子里 `r.Use` 会真的生效却不会出现在**前一步已经打印**的
// `plugin chain` 日志里（部署方从日志看不到那个中间件）；`r.Named` 赶不上
// required_plugins 校验；`r.Config` 赶不上配置认领检查（多认领的键没人再核对）。
// 三种形态都属于本项目一贯要消灭的「静默失效」，所以直接报错。
// `OnStop` 不在此列：清理钩子在 RunStop 时才用，钩子里登记是真生效的。
func (r *Registry) OnBuild(fn func() error) { r.onBuild = append(r.onBuild, fn) }

// registryShape 是注册表的可观测规模，用于比对 build 期钩子有没有偷偷改注册表。
// 只记条数：钩子能做的事只有「往这些切片/映射里加东西」，条数变了就足够定位。
type registryShape struct {
	mws, names, routes, publicRoutes, onBuild, claimed int
	configCalls, toolCalls                             int
}

// shape 抓一次当前规模。
func (r *Registry) shape() registryShape {
	return registryShape{
		mws: len(r.mws), names: len(r.names), routes: len(r.routes),
		publicRoutes: len(r.publicRoutes), onBuild: len(r.onBuild),
		claimed: len(r.claimedKeys), configCalls: r.configCalls,
		toolCalls: r.toolCalls,
	}
}

// runOnBuild 依次执行 build 期校验钩子。
//
// 两条纪律：
//
//  1. **panic 转 error**：钩子里跑的是插件与
//     宿主的任意代码，而 build 用 sync.Once 缓存结果——f panic 时 Once 也会置 done，
//     于是 builtHTTP / buildErr 双双留在零值，第二次 Handlers() 返回 (nil, nil)：
//     宿主 recover 之后挂上一个 nil handler，每个请求都 nil 解引用。启动必须 fail-closed，
//     所以这里把 panic 变成启动失败，堆栈只进本地日志（它是定位钩子 bug 的唯一线索）。
//  2. **钩子不得改注册表**：前后比对 shape()，变了直接启动失败（理由见 OnBuild 注释）。
func (r *Registry) runOnBuild() error {
	before := r.shape()
	for _, fn := range r.onBuild {
		if fn == nil {
			continue
		}
		if err := callBuildHook(fn); err != nil {
			return err
		}
	}
	if after := r.shape(); after != before {
		return fmt.Errorf("build 期校验钩子改动了注册表（前 %+v 后 %+v）："+
			"钩子只能做校验与日志，此刻改注册表全是静默失效"+
			"（链还没装但已经打过 plugin chain 日志、required_plugins 与配置认领检查都已跑过）",
			before, after)
	}
	return nil
}

// callBuildHook 调用一个 build 期钩子，把 panic 转成 error。
func callBuildHook(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("[mcp] build hook panic: %v\n%s", p, debug.Stack())
			err = fmt.Errorf("build 期校验钩子 panic: %v（详细堆栈见本地日志）", p)
		}
	}()
	return fn()
}

// OnStop 注册进程退出前的清理钩子。
func (r *Registry) OnStop(fn func()) { r.onStop = append(r.onStop, fn) }

// RunStop 依次执行清理钩子。**幂等**（sync.Once）：清理只该发生一次，而现在它有两个
// 触发点——正常退出，以及启动失败时基座自己兜的那一次（见 build 的失败路径）。
// 宿主照旧 defer 一次即可，不必判断「是不是已经清过了」。
//
// 跑完即把本 Registry 标记为报废：之后 build/Start 一律拒绝（见 checkStopped）。
//
// 单个钩子 panic 不连坐其余钩子（callStopHook 逐个 recover）。这条与幂等是配套的：
// 幂等意味着「第二次调用不会补跑」，若第一个钩子 panic 就中断，后面的连接池与文件句柄
// 将永远没人关——一个坏钩子把整条清理链废掉。
func (r *Registry) RunStop(ctx context.Context) {
	r.stopOnce.Do(func() {
		r.stopped = true
		for i, fn := range r.onStop {
			if fn != nil {
				callStopHook(i, fn)
			}
		}
	})
}

// callStopHook 调用一个清理钩子，把 panic 转成一条日志。
// 清理钩子签名是 func()（本来就不返回错误），所以这里只能记日志——但绝不能让它
// 打穿 RunStop。
func callStopHook(i int, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("[mcp] error: 清理钩子[%d] panic，已跳过并继续执行其余钩子: %v\n%s",
				i, p, debug.Stack())
		}
	}()
	fn()
}

// checkStopped 在清理已经发生之后拒绝再次启动。
//
// 组合出这条路径的是两件本身都正确的事：启动失败时基座会兜一次 RunStop，而 RunStop
// 幂等。于是「listen 失败 → 换个端口用同一个 Registry 重试 Start」会真的把服务起起来，
// 但插件已经被清理过：只读工具照常返回结果，依赖外部后端的插件**永久**拒绝放行
// （连接已关），spill 的落盘文件也再也不被回收。
// 这种「带着已清理的插件继续对外服务」的降级形态必须直接拒绝——重试请新建一个
// Registry（New 出来的实例自带全新的 stopOnce/stopped，重建这条路是通的）。
func (r *Registry) checkStopped() error {
	if !r.stopped {
		return nil
	}
	return fmt.Errorf("%s", "本 Registry 的清理钩子已经执行过（连接池已关、回收协程已停），"+
		"不能再启动：带着已清理的插件对外服务会让受配额管辖的工具永久被拒、"+
		"落盘文件与待确认单不再被回收。重试请用 New 新建一个 Registry 并重新 Install 插件")
}

// validate 报出构造期错误、校验 required_plugins 全部在场，并打印生效插件链。
//
// 刻意不导出：它的判据之一（配置文件里的 required_plugins）要等 loadBase 之后才齐，
// 导出成公开 API 会让语义随调用时机变化。插件与宿主只需 Handlers/Start 的返回值。
func (r *Registry) validate() error {
	if r.err != nil {
		return r.err
	}
	installed := map[string]bool{}
	for _, n := range r.names {
		installed[n] = true
	}
	for _, need := range append(append([]string{}, r.cfg.RequiredPlugins...), r.fileRequired...) {
		if !installed[need] {
			return fmt.Errorf("required plugin %q not installed", need)
		}
	}
	log.Printf("[mcp] plugin chain (outer→inner): %v", r.names)
	return nil
}

// baseFileConfig 是基座自己那部分配置：[[tokens]]、identity_headers、
// trust_identity_header、required_plugins 与 [log]。
// 插件段落由各插件用 Registry.Config 自行解码并认领。
type baseFileConfig struct {
	Tokens              []TokenConfig `toml:"tokens"`
	IdentityHeaders     []string      `toml:"identity_headers"`
	TrustIdentityHeader bool          `toml:"trust_identity_header"`
	RequiredPlugins     []string      `toml:"required_plugins"`
	Log                 LogConfig     `toml:"log"`
}

// loadBase 读基座配置并构造 TokenAuthz。
//
// token 鉴权始终启用：ConfigPath 为空、文件读不到、或一条 [[tokens]] 都没有，
// 一律启动失败。刻意不提供开关——一个不生效的开关会让部署方以为鉴权开着，
// 比启动报错危险得多。
func (r *Registry) loadBase() error {
	if r.cfg.ConfigPath == "" {
		return fmt.Errorf("%s", "Config.ConfigPath 为空：基座 token 鉴权必须配置，"+
			"请提供含 [[tokens]] 的 TOML 文件")
	}
	var fc baseFileConfig
	md, err := toml.DecodeFile(r.cfg.ConfigPath, &fc)
	if err != nil {
		return fmt.Errorf("load base config %s: %w", r.cfg.ConfigPath, err)
	}
	r.claimKeys(md)
	if len(fc.Tokens) == 0 {
		return fmt.Errorf("%s 里没有任何 [[tokens]]：不允许启动一个无鉴权的 MCP 端点",
			r.cfg.ConfigPath)
	}
	az, err := NewTokenAuthz(TokenAuthzConfig{
		Tokens:              fc.Tokens,
		IdentityHeaders:     fc.IdentityHeaders,
		TrustIdentityHeader: fc.TrustIdentityHeader,
	})
	if err != nil {
		return fmt.Errorf("token authz %s: %w", r.cfg.ConfigPath, err)
	}
	r.az = az
	r.fileRequired = fc.RequiredPlugins
	SetLogIDHeader(fc.Log.LogIDHeader)
	return nil
}

// checkConfigKeys 让配置文件里没人认领的键成为启动失败。
//
// 必须在全部插件 Install 之后执行（插件在 Install 里调 Registry.Config 认领自己的段落）。
// 拦的是这类事故：`[qouta]`、`on_eror = "deny"`、`required_plugin = [...]` ——
// TOML 解码器对它们一声不响，结果是「插件按默认值跑」或「必需插件校验静默失效」。
//
// 两个有意如此的副作用：
//   - 配置里留着某插件的段落、但本次部署没 Install 该插件 => 启动失败。
//     「写了 [quota] 却没装 quota 插件」与「段落名拼错」在基座看来完全一样，
//     两者都意味着「部署方以为生效的东西没生效」，都该拦。
//   - 插件若用 map 兜底解码、或漏声明文档承诺的字段，本检查会失准或误伤。
//     那不是基座能修的，已写成 Registry.Config 上的契约。
func (r *Registry) checkConfigKeys() error {
	unclaimed := r.unclaimedKeys()
	if len(unclaimed) == 0 {
		return nil
	}
	return fmt.Errorf("%s 里有无人认领的配置项 %v："+
		"段落名或字段名可能拼错了（也可能是对应插件没有安装）。"+
		"配置项静默失效比启动失败危险，故此处直接拒绝启动",
		r.cfg.ConfigPath, unclaimed)
}

// build 完成启动期校验与组装，返回 MCP 端点的 http.Handler。
//
// 幂等：插件链只装一次（SDK 的 AddReceivingMiddleware 是追加语义），
// 重复调用返回首次的结果。Handlers 是给宿主组装用的公开 API，被调两次很自然，
// 而链装两遍意味着审计写两条、配额扣两次。
//
// 链序（由外到内）：
//
//	HTTPLogID → HTTPHeaders → 认证(401) → owner 路由 → MCP handler
//	                                                   └ token 准入 → 插件链 → 工具执行
//
// token 准入固定装在插件链之前：它是基座的访问控制，不能被插件顺序影响。
func (r *Registry) build() (http.Handler, error) {
	// 清理过的 Registry 不得再组装。放在 buildOnce **之外**：这条判定与「build 只跑
	// 一次」无关，而且 build 成功之后宿主才 RunStop 是正常生命周期，那时 builtHTTP
	// 已经缓存好，再调 Handlers() 拿到一个指向已清理插件的 handler 同样是坑。
	if err := r.checkStopped(); err != nil {
		return nil, err
	}
	r.buildOnce.Do(func() {
		r.builtHTTP, r.buildErr = r.buildOnceBody()
		if r.buildErr != nil {
			// 启动失败就地回收：插件在 Install 里已经起了协程、开了连接池与文件句柄
			// （如 spill 的 gcLoop、需要数据库的插件的客户端及其内部清理协程），
			// 而失败原因常常在最后一个插件之后才发现（配置认领检查、build 期钩子）。指望每个调用方在每条失败路径上记得清一次，是必漏的约定；
			// RunStop 本身幂等，宿主照旧 defer 一次即可。
			r.RunStop(context.Background())
		}
	})
	return r.builtHTTP, r.buildErr
}

func (r *Registry) buildOnceBody() (http.Handler, error) {
	if err := r.loadBase(); err != nil {
		return nil, err
	}
	if err := r.checkConfigKeys(); err != nil {
		return nil, err
	}
	if err := r.validate(); err != nil {
		return nil, err
	}
	if r.cfg.SelfAddr != "" {
		SetSelfAddr(r.cfg.SelfAddr)
	}
	if r.cfg.PublicBaseURL != "" {
		SetPublicBaseURL(r.cfg.PublicBaseURL)
	}
	if len(r.cfg.Peers) > 0 {
		SetPeers(r.cfg.Peers)
	}
	// 插件的 build 期自检排在这里：基座自身的校验已全部通过（否则真正的失败原因会被
	// 一个次要原因盖掉），全局状态（PublicBaseURL / Peers）也已定型，而链还没装、
	// 端口还没接流——正是「启动失败」这个动作还来得及的最后一刻。
	if err := r.runOnBuild(); err != nil {
		return nil, err
	}

	// 基座中间件与插件链一起交给适配层：token 准入固定在最前，插件按安装顺序排在其后。
	r.srv.AddReceivingMiddleware(asMCPMiddleware(
		append([]Middleware{r.az.Middleware()}, r.mws...)))

	// Stateless：每个请求独立成会话，逐次重读 Authorization / 身份头，
	// 从而支持「一个 agent 用同一条连接服务多人」的调用级身份透传。
	h := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return r.srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	// owner 路由在认证之内：转发前请求已通过认证，属主副本会再认证一次。
	return HTTPLogID(HTTPHeaders(r.az.HTTPMiddleware(withMCPOwnerRouting(h)))), nil
}

// Handlers 返回可挂载到既有 HTTP server 的 MCP handler 与插件注册的路由。
// 与 Start 不同，它不自己监听端口，由宿主决定挂在哪个子路径、共用哪个生命周期。
//
// 返回的 routes 里，Route 注册的已套好 token 认证，RoutePublic 注册的是裸 handler。
// 重复调用返回同一份结果（幂等）。
func (r *Registry) Handlers() (mcpHandler http.Handler, routes map[string]http.Handler, err error) {
	h, err := r.build()
	if err != nil {
		return nil, nil, err
	}
	return h, r.Routes(), nil
}
