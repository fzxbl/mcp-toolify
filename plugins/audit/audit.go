// Package audit 是审计插件：把每次 MCP 调用的完整上下文交给使用方注册的落地函数。
// 基座不参与落地方式，日志、消息队列、数仓都由使用方决定。
//
// 安装位置：应最先安装，位于**插件链**最外层——既能记录被内层插件
// 拒绝的调用，也能看到内层插件（如 spill）改写后的最终结果。
//
// 但它不是整条链的最外层：**基座的 token 准入固定装在所有插件之前**，见第 6 条。
//
// 使用方必须知道的七条：
//
//  1. **投递是异步的、尽力而为的，永远不阻塞调用返回。** 中间件在返回路径上只把事件
//     塞进有界队列，落地由后台单 worker 做（见 async.go）。刻意**没有**「阻塞直到落地」
//     或「没审计就不返回结果」这类档位：审计判定发生在返回路径上、工具此刻已经执行完，
//     fail-closed 挡不住任何副作用，只是把结果藏起来、还得劝调用方不要重试；真正需要
//     fail-closed 的是执行**前**的门（人白名单、二次确认、配额）。
//     代价是「每次调用必有记录」降级为尽力而为：队列打满、进程被 kill -9、机器掉盘
//     都会丢事件。因此丢弃绝不静默——计数 + 限流告警 + ReadStats() 供宿主接监控。
//  2. 落地函数要在 Install 之前注册：一个都没注册时启动直接失败。
//     「审计插件在场但一条也没落」是静默失效，而 required_plugins 只校验插件在场。
//     接流之后再把 Sink 清空同样会被计成失败并告警（worker 侧兜底）。
//  3. 每个 MCP 方法都会产生事件，包括 initialize、ping 与各类 notifications。
//     远端 Sink 请按 Event.Method 自行过滤。落地慢不再影响调用方，但会挤占队列——
//     过滤放在 Sink 里，队列容量按 queue_size 调。
//  4. 事件里没有凭据。Call.Headers 已由基座剔除 Authorization/Proxy-Authorization/
//     Cookie/Set-Cookie，本插件也拒绝把这些头名配进 headers（见 normalize），且不去读
//     *http.Request 绕过这层剔除。Subject.Token 按基座契约是 token 的**用途名**。
//  5. 工具入参不得承载密文。Event.Args 是入参原文，`login(user, password)` 这类工具
//     的密文会原样进长期审计存储；基座不认识字段语义、代替不了你判断。确有需要时用
//     SetArgsRedactor 注册脱敏钩子（在截断之前执行）。这条同样适用于其它插件。
//  6. **基座 token 准入的拒绝不进审计流**（终审实测，务必知道）：那一层固定装在插件链
//     之前，`tools/call` 被它拒时直接返回，本插件根本不会被调用——越权尝试在审计流里
//     是 0 条事件、HTTP 状态还是 200。同理 `tools/list` 的按 token 过滤发生在本插件
//     记录**之后**，所以 Event.Result 里是过滤**前**的全量清单，不等于该 token 实际
//     看到的那份。两者目前的唯一信号是基座打的 `[mcp] warning: 准入拒绝 …` /
//     `tools/list 过滤 …` 日志（限流，见 runtime/tokenauthz.go）。做「谁看见/试过哪些
//     工具」这类权限审计时，必须把那两条日志一并采集，不能只看审计流。
//  7. Event.Meta 是 Call.Meta 的通用透传（拍平成字符串、键数与值长都有上限）。
//     它是外置插件把自己的上下文送进审计流的唯一通道——本插件不认识任何具体插件，
//     所以键名的含义由写入方负责，落地方按需过滤。
//
// Event 是**完全自有的值拷贝**（labels 深拷、入参/结果已截断成 string、Meta 拍平成新
// map），交给 worker goroutine 既没有 data race 也不会拖住大对象引用——异步化不需要
// 额外拷贝，但**新增字段时必须保持这条性质**（不要往 Event 里塞指针或共享 map）。
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// Name 是插件名，与 required_plugins 里的写法一致。
const Name = "audit"

// AnonymousActor 是 Subject.ID 为空时 ActorID 返回的占位标识。
//
// 基座默认不信任客户端送来的身份头，因此 Subject.ID 常态为空串。落地方若直接把空串
// 当用户名写进审计，「匿名调用」与「某个 id 为空的人」就再也分不开了，所以这里给一个
// 一眼能看出不是人名的占位值，并另配 Event.HasIdentity 供程序判定。
const AnonymousActor = "(anonymous)"

// 截断上限的默认值（字节）：配成 0 表示「用默认值」，**没有**关闭截断的写法。
// 刻意不提供关闭途径——一次几十兆的入参/结果原文进事件，内存与审计后端都会被打满，
// 而「关掉截断」这种配置一旦存在，出事时才会被发现它开着。
const (
	defaultMaxArgsBytes   = 1024
	defaultMaxResultBytes = 2048
)

// credentialHeaders 是禁止配进 [audit] headers 的凭据类头名（小写比较）。
// 基座已把它们从 Call.Headers 里剔除，这里再拦一道是为了给出明确的启动错误，
// 而不是让部署方拿到一列恒为 "-" 的审计字段、以为自己采到了。
var credentialHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
}

// Config 对应配置文件的 [audit] 段。
//
// 刻意用具名字段结构体、且字段集与文档一致（基座的配置认领制要求）：用 map 兜底解码
// 会让段内拼错的键被判为「已解码」，于是 `[audit] on_eror = ...` 被完全吞掉、插件按
// 默认值上线；少声明一个文档承诺的字段，则部署方照文档写全反而启动失败。
type Config struct {
	Audit Section `toml:"audit"`
}

// Section 是 [audit] 段的字段集，即本插件的对外配置契约。
type Section struct {
	// Headers 是需要采集进事件的请求头名；不含凭据类头（配了即启动失败）。
	Headers []string `toml:"headers"`
	// QueueSize 是异步事件队列的容量（条），省略或填 0 取 4096；不能为负。
	//
	// 刻意**没有**「无界队列」的写法：落地方卡住时无界队列就是一条通往 OOM 的路，
	// 而丢事件至少是有计数、有告警、能被监控看见的。
	QueueSize int `toml:"queue_size"`
	// FlushTimeout 是进程退出时排空队列的时间预算（duration 串，如 "3s"），
	// 省略取 3s。它直接加在服务停止耗时上，别配太长。
	FlushTimeout string `toml:"flush_timeout"`
	// MaxArgsBytes 是入参进事件前的截断上限（字节），省略或填 0 取 1024；无法关闭。
	MaxArgsBytes int `toml:"max_args_bytes"`
	// MaxResultBytes 是结果进事件前的截断上限（字节），省略或填 0 取 2048；无法关闭。
	MaxResultBytes int `toml:"max_result_bytes"`

	// flush 是 FlushTimeout 解析后的值（normalize 填写，不参与解码）。
	flush time.Duration
}

// normalize 校验并补齐默认值。任何写错的项都返回 error 让启动失败——
// 静默用默认值比启动失败危险得多（部署方会以为自己配的策略在生效）。
func (s Section) normalize() (Section, error) {
	out := s
	if out.QueueSize < 0 {
		return Section{}, fmt.Errorf("queue_size=%d 不能为负", out.QueueSize)
	}
	if out.QueueSize == 0 {
		out.QueueSize = defaultQueueSize
	}
	out.flush = defaultFlushTimeout
	if strings.TrimSpace(out.FlushTimeout) != "" {
		d, err := time.ParseDuration(out.FlushTimeout)
		if err != nil {
			return Section{}, fmt.Errorf("flush_timeout=%q 解析失败: %w", out.FlushTimeout, err)
		}
		if d <= 0 {
			return Section{}, fmt.Errorf("flush_timeout=%q 必须大于 0", out.FlushTimeout)
		}
		out.flush = d
	}
	if out.MaxArgsBytes < 0 {
		return Section{}, fmt.Errorf("max_args_bytes=%d 不能为负", out.MaxArgsBytes)
	}
	if out.MaxResultBytes < 0 {
		return Section{}, fmt.Errorf("max_result_bytes=%d 不能为负", out.MaxResultBytes)
	}
	if out.MaxArgsBytes == 0 {
		out.MaxArgsBytes = defaultMaxArgsBytes
	}
	if out.MaxResultBytes == 0 {
		out.MaxResultBytes = defaultMaxResultBytes
	}
	headers := make([]string, 0, len(out.Headers))
	for i, h := range out.Headers {
		name := strings.TrimSpace(h)
		if name == "" {
			return Section{}, fmt.Errorf("headers[%d] 是空串：请删掉它或填上真实头名", i)
		}
		if credentialHeaders[strings.ToLower(name)] {
			return Section{}, fmt.Errorf("headers[%d]=%s 是凭据类头，禁止进审计事件", i, name)
		}
		headers = append(headers, name)
	}
	out.Headers = headers
	return out, nil
}

// Event 是一次调用的审计事件。字段在进入链时快照，返回后补齐结果类字段。
type Event struct {
	// At 是调用进入 audit 的时刻。
	At time.Time
	// LogID 与 HTTP 层回写的同名响应头一致，用于和宿主 access log 对账。
	LogID string
	// Method 是 MCP 方法名，tools/call、tools/list 之外还包括 initialize、ping、
	// notifications/*，落地方可据此过滤。
	Method string
	Tool   string // 非 tools/call 时为空
	// Labels 是该工具的完整 labels（含基座投影的 name/pkg），进入时的快照。
	Labels map[string]string
	// Subject 是调用主体的**进入时快照**：audit 在最外层，返回路径上 Call.Subject
	// （指针）已可能被内层插件改过，直接读会记成被改写后的身份。
	Subject runtime.Subject
	// HasIdentity 区分「有真实身份」与「匿名/未配置身份」，见 AnonymousActor。
	HasIdentity bool
	// Args 是入参 JSON（先脱敏、再按上限截断），ArgsTruncated 标记是否截断。
	//
	// **可能含非法 UTF-8 字节**：它是调用方送来的原始 JSON 字节，审计不替它改写
	// （截断只剥掉尾部那个不完整字符，中段的坏字节原样保留）。落地前请按后端要求
	// 自行转义或替换——Postgres text、部分 JSON 序列化器会对非法 UTF-8 直接报错，
	// 那会让这条事件在你的 Sink 里落地失败（计入 ReadStats().Failed，不影响调用方）。
	Args          string
	ArgsTruncated bool
	// Result 是最终结果的摘要——**限于插件链之内**的最终结果。
	//
	// tools/list 是例外：基座的按 token 过滤在本插件记录之后才发生，所以这里是过滤
	// **前**的全量清单，不是该 token 实际看到的那份（见包注释第 6 条）。
	//
	// tools/call：JSON **片段**——截断后可能不完整（例如 `[{"type":"text",`），
	// 只供人读与全文检索，**不要 Unmarshal**。任意工具的结果形状不可控，
	// 想保证合法 JSON 就只能整份留下，那正是截断要避免的事。
	// tools/list：可见工具名列表，按**个数**裁剪，始终是合法 JSON。
	Result          string
	ResultTruncated bool
	// ResultErr 记录结果序列化失败的原因（不静默丢字段）。
	ResultErr string
	Cost      time.Duration
	// IsError 为真表示结果是错误态（业务级拒绝或工具报错）。
	IsError bool
	// Err 是链上抛出的协议级错误文案，空表示没有。
	Err string
	// DeniedBy / DenyReason 来自 Call.Meta，空表示未被任何插件拒绝。
	DeniedBy   string
	DenyReason string
	// Headers 是按配置采集的请求头，缺失的头值为 "-"。
	Headers map[string]string
	// Meta 是 Call.Meta 在链返回后的快照：任何插件写进 Meta 的内容都会到这里。
	//
	// 刻意做成通用透传而不是逐个插件开字段：audit 不认识别的插件，
	// 加特化字段等于把插件语义搬进 audit，外置插件就再也用不上这条路。
	//
	// 值一律拍平成字符串（按 fmt.Sprintf("%v")）——审计后端要能直接写库，
	// 任意嵌套结构会让落地方在序列化时才爆。单个值按 maxMetaValueBytes 截断；
	// 键数超过 maxMetaKeys 时按键名排序取前 N 个（排序保证同一次调用在不同进程里
	// 截出同一批键，否则对账时会各说各话）。
	Meta map[string]string
}

// ActorID 返回可直接写进审计的执行人标识：无身份时返回 AnonymousActor 而不是空串。
func (e Event) ActorID() string {
	if e.Subject.ID == "" {
		return AnonymousActor
	}
	return e.Subject.ID
}

// Sink 是使用方注册的审计落地函数。
//
// 返回 error 而非无返回值：落地方是唯一知道「这条有没有写成功」的一方。
// 投递是异步的，返回 error 不会影响任何调用结果，但它会被计入 ReadStats().Failed
// 并触发限流告警——不返回错误，落地失败就只有落地方自己知道。
type Sink func(Event) error

// Redactor 是入参脱敏钩子：入参进事件之前先过它一遍。
//
// 契约：返回改写后的切片，返回 nil 视为空入参。
// 传入的 args 是**本插件拷贝出来的副本**，钩子就地涂改也伤不到真实请求字节——
// 这层防御不是可省的：Call.Args 与 MCP SDK 的 CallToolParamsRaw.Arguments 共享同一
// 底层数组，而工具入参的反序列化发生在链终点（钩子之后），一个写错的钩子否则会让
// 线上工具收到被涂改的参数。脱敏本就是少数部署才开的开关，这份拷贝不影响默认路径。
type Redactor func(tool string, args []byte) []byte

var (
	mu       sync.RWMutex
	sinks    []Sink
	redactor Redactor
)

// OnEvent 注册一个审计落地函数，可注册多个（按注册顺序依次调用）。
// 必须在 Install 之前（或同时）调用，理由见包注释第 1 条。
func OnEvent(fn Sink) {
	if fn == nil {
		return
	}
	mu.Lock()
	sinks = append(sinks, fn)
	mu.Unlock()
}

// SetArgsRedactor 注册入参脱敏钩子（可选，传 nil 清除）。未注册时入参原样进事件。
func SetArgsRedactor(fn Redactor) {
	mu.Lock()
	redactor = fn
	mu.Unlock()
}

// sinkList 拷一份当前落地函数列表；argsRedactor 取当前脱敏钩子。
// 都不在锁内执行用户代码：落地函数可能写远端，持锁会把注册与其它请求一起堵住。
func sinkList() []Sink {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Sink, len(sinks))
	copy(out, sinks)
	return out
}

func argsRedactor() Redactor {
	mu.RLock()
	defer mu.RUnlock()
	return redactor
}

// current 指向当前生效的投递器，供宿主查 ReadStats()。
//
// 与 spill 插件的 current 同源的例外：插件之间不共享状态，但宿主需要一个包级入口才能
// 把运行期计数接进自己的监控。停止时置空，ReadStats() 因此返回零值而不是过期计数。
var current atomic.Pointer[dispatcher]

// ReadStats 返回审计投递的运行期计数，供宿主接监控。未安装或已停止时返回零值。
//
// 名字带 Read 而不是直接叫 Stats：包里已经有 Stats 这个类型，
// 而计数类型比访问函数更值得占用这个名字（落地方要在自己的代码里声明它）。
//
// 为什么必须给出来：异步投递会丢事件（队列满、进程被强杀），不暴露计数的话
// 「尽力而为」就只是一句免责声明，运维看不见自己到底丢了多少。
func ReadStats() Stats {
	if d := current.Load(); d != nil {
		return d.stats()
	}
	return Stats{}
}

// Install 安装审计插件。应最先安装，使其位于洋葱最外层。
func Install(r *runtime.Registry) error {
	r.Named(Name)
	var cfg Config
	if err := r.Config(&cfg); err != nil {
		return fmt.Errorf("audit 读配置: %w", err)
	}
	sec, err := cfg.Audit.normalize()
	if err != nil {
		return fmt.Errorf("[audit] 配置无效: %w", err)
	}
	d := newDispatcher(sec.QueueSize, sec.flush)
	current.Store(d)
	// 「审计插件在场但一条也没落」是静默失效，而 required_plugins 只校验插件在场，
	// 所以这里必须把「一个 sink 都没注册」判成启动失败。
	// 校验挂在基座的 build 期钩子上而不是 Install 里：落地函数允许在 Install 之后、
	// 开始接流之前才注册（宿主常把 OnEvent 与日志/DB 初始化写在一起），在 Install
	// 里判就会把这种合法用法误判成启动失败。
	// **双层不撤**：这一层堵「启动之前就能看出来」的那一半，worker 侧的空 sink 兜底
	// （计成 failed + 告警）继续管「接流之后被置回 nil」。
	r.OnBuild(func() error {
		if len(sinkList()) == 0 {
			return fmt.Errorf("[audit] 没有注册任何落地函数：" +
				"请在开始接流之前调用 audit.OnEvent(...)，否则审计插件在场却一条也不会落")
		}
		// 启动日志声明生效策略。打在 build 期而不是 Install 里，sinks 的条数才是真正
		// 接流时的条数（Install 之后注册的 sink 也算进去）。
		log.Printf("[mcp] audit: 异步投递（尽力而为，不阻塞返回）sinks=%d queue_size=%d "+
			"flush_timeout=%s max_args_bytes=%d max_result_bytes=%d headers=%v",
			len(sinkList()), sec.QueueSize, sec.flush, sec.MaxArgsBytes,
			sec.MaxResultBytes, sec.Headers)
		return nil
	})
	// 退出时限时排空：异步之后「进程停了还有事件在队列里」是常态，不排空就等于
	// 每次发布都稳定丢掉最后一批记录。
	r.OnStop(func() {
		d.stop()
		current.CompareAndSwap(d, nil)
	})
	r.Use(middleware(sec, d))
	return nil
}

// middleware 返回审计中间件：进入时快照上下文，返回后补齐结果并**异步**投递。
//
// 返回值原样透传、绝不因审计而改变：审计判定发生在返回路径上、工具此刻已经执行完，
// 拦下结果挡不住任何副作用（见包注释第 1 条）。
func middleware(cfg Section, d *dispatcher) runtime.Middleware {
	return func(next runtime.Handler) runtime.Handler {
		return func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
			e := snapshot(c, cfg)
			res, err := next(ctx, c)
			e.Cost = time.Since(e.At)
			fill(&e, c, res, err, cfg)
			// 入队不阻塞、不返回错误：队列满就丢并计数（见 dispatcher.enqueue）。
			d.enqueue(e)
			return res, err
		}
	}
}

// snapshot 在进入链之前抓取调用上下文。
//
// 必须在这里抓：Call 上的 Subject 是指针、Labels 是 map，内层插件在 next 之前改一行
// 就能让返回路径上读到的身份/标注与真实执行时的不一致。
func snapshot(c *runtime.Call, cfg Section) Event {
	e := Event{
		At:      time.Now(),
		LogID:   c.LogID,
		Method:  c.Method,
		Tool:    c.Tool,
		Labels:  copyLabels(c.Labels),
		Headers: pickHeaders(c, cfg.Headers),
	}
	if c.Subject != nil {
		e.Subject = runtime.Subject{
			ID:     c.Subject.ID,
			Token:  c.Subject.Token,
			Labels: copyLabels(c.Subject.Labels),
		}
	}
	e.HasIdentity = e.Subject.ID != ""
	args := c.Args
	if red := argsRedactor(); red != nil && len(args) > 0 {
		// 传副本：钩子就地改写不能污染真实请求字节（见 Redactor 的契约注释）。
		args = red(c.Tool, append([]byte(nil), args...))
	}
	// 在 []byte 上截断、只把留下的那一段转成 string：先 string(args) 再截，
	// 等于每次调用都把整个入参复制一遍，上限配成 16 字节也挡不住几 MB 的入参。
	e.Args, e.ArgsTruncated = truncateBytes(args, cfg.MaxArgsBytes)
	return e
}

// fill 补齐返回路径上才有的字段：结果、错误、拒绝详情。
func fill(e *Event, c *runtime.Call, res *runtime.Result, err error, cfg Section) {
	if err != nil {
		e.Err = err.Error()
	}
	switch {
	case res == nil:
	case res.Tool != nil:
		e.IsError = res.Tool.IsError
		// 先按预算把文本内容裁掉再序列化：直接 json.Marshal 整份结果，会把几 MB 的
		// 文本完整展开成 JSON 之后才截断，截断上限就只保护了下游后端。
		small, shrunk := shrinkContent(res.Tool.Content, cfg.MaxResultBytes)
		out, cut, mErr := marshalLimited(small, cfg.MaxResultBytes)
		if mErr != nil {
			e.ResultErr = mErr.Error()
		} else {
			e.Result, e.ResultTruncated = out, cut || shrunk
		}
	case res.List != nil:
		// tools/list 记注册表里的工具名。注意**不是**「这个 token 看见了哪些」：基座的
		// 按 token 过滤在本插件返回之后才做（见包注释第 6 条），此处拿到的是全量清单。
		// 这条分支的结构是可控的（一串短字符串），所以按**个数**裁剪而不是裁字节：
		// 既让 Result 始终是合法 JSON，也不用先把几千个工具名整体展开再截掉。
		names, cut := visibleToolNames(res.List.Tools, cfg.MaxResultBytes)
		out, mErr := json.Marshal(names)
		if mErr != nil {
			e.ResultErr = mErr.Error()
		} else {
			e.Result, e.ResultTruncated = string(out), cut
		}
	}
	// 拒绝详情只能在 next 之后读：写它的插件在内层。key 用基座导出的常量，
	// 避免与拒绝方的写入拼错字符串。
	e.DeniedBy, _ = c.Meta[runtime.MetaDeniedBy].(string)
	e.DenyReason, _ = c.Meta[runtime.MetaDenyReason].(string)
	// 与上面两行同源：Meta 相关的读取集中在一处，返回路径读才看得到内层插件写的内容。
	e.Meta = snapshotMeta(c.Meta)
}

// Meta 快照的两条硬上限。审计事件会进长期存储，插件想写多少键、多长的值都不该由
// 落地方去防——这两条把它变成有界的。
const (
	maxMetaKeys       = 32
	maxMetaValueBytes = 256
)

// snapshotMeta 把 Call.Meta 拍平成可直接落库的字符串 map；nil / 空入参返回 nil
// （落地方据此区分「没人写」与「写了空值」）。
func snapshotMeta(m map[string]any) map[string]string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxMetaKeys {
		keys = keys[:maxMetaKeys]
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		v, _ := truncateString(fmt.Sprintf("%v", m[k]), maxMetaValueBytes)
		out[k] = v
	}
	return out
}

// pickHeaders 按配置采集请求头，缺失的头值记 "-"（区分「没配」与「配了但没有值」）。
// 数据源只有 Call.Headers（基座已剔除凭据类头），不去读 *http.Request。
func pickHeaders(c *runtime.Call, names []string) map[string]string {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]string, len(names))
	for _, n := range names {
		v := "-"
		if c.Headers != nil {
			if got := c.Headers.Get(n); got != "" {
				v = got
			}
		}
		out[n] = v
	}
	return out
}

// copyLabels 深拷贝一份 label map；nil 入参返回 nil。
func copyLabels(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// shrinkContent 按预算裁剪结果里的文本内容，返回浅拷贝（绝不改调用方的结果——
// 那是模型真正要收到的那份）与「是否裁过」。
//
// 只处理 *mcp.TextContent：它是大结果的常见形态，也是唯一能在序列化之前按字节裁剪的
// 类型。其它内容类型（图片、嵌入资源）只能先序列化再按上限截断，见 marshalLimited。
func shrinkContent(content []mcp.Content, max int) ([]mcp.Content, bool) {
	budget := max
	out := content
	shrunk := false
	for i, item := range content {
		tc, ok := item.(*mcp.TextContent)
		if !ok {
			continue
		}
		if len(tc.Text) <= budget {
			budget -= len(tc.Text)
			continue
		}
		if !shrunk && sameSlice(out, content) {
			out = append([]mcp.Content(nil), content...)
		}
		cut := ""
		if budget > 0 {
			cut, _ = truncateString(tc.Text, budget)
		}
		clone := *tc
		clone.Text = cut
		out[i] = &clone
		budget = 0
		shrunk = true
	}
	return out, shrunk
}

// sameSlice 判断两个内容切片是否共享底层数组（决定要不要先浅拷贝一份）。
func sameSlice(a, b []mcp.Content) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// marshalLimited 序列化 v 并把结果截到 max 字节内，返回（JSON 片段、是否截断、错误）。
func marshalLimited(v any, max int) (string, bool, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", false, err
	}
	s, cut := truncateBytes(b, max)
	return s, cut, nil
}

// truncateBytes 把 b 截到最多 n 字节后转成 string，并返回是否发生了截断。
// n <= 0 视为不设限（normalize 保证配置里取不到这个值）。
func truncateBytes(b []byte, n int) (string, bool) {
	if n <= 0 || len(b) <= n {
		return string(b), false
	}
	return string(trimPartialRune(b[:n])), true
}

// truncateString 是 truncateBytes 的字符串版本，只复制留下的那一段。
func truncateString(s string, n int) (string, bool) {
	if n <= 0 || len(s) <= n {
		return s, false
	}
	return string(trimPartialRune([]byte(s[:n]))), true
}

// trimPartialRune 去掉尾部那个被切断的字符，最多丢 utf8.UTFMax-1 个字节。
//
// 刻意不写成「整串校验合法性、不合法就再退一个字节」：那种写法一旦入参中段就有非法
// 字节（Call.Args 是未校验的原始 JSON 字节，二进制/坏编码入参完全可能），会一路退到
// 空串，把一整段可读的审计内容悄悄丢掉，而且每次还多扫一遍全串。
// 中段的非法字节原样保留——它本来就是调用方送来的内容，审计不该替它改写。
func trimPartialRune(b []byte) []byte {
	for i := 0; i < utf8.UTFMax-1 && len(b) > 0; i++ {
		r, size := utf8.DecodeLastRune(b)
		if r != utf8.RuneError || size > 1 {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}

// visibleToolNames 按上限裁「工具名个数」，返回保留的名字与是否裁过。
//
// 裁个数而不是裁字节：tools/list 的结果形状可控，裁个数能让 Event.Result 始终是
// 合法 JSON（落地方可以直接 Unmarshal），也不必先把几千个工具名整体展开成 JSON
// 再截掉大半。预算按 json.Marshal 之后的字节算，见 jsonStringLen。
func visibleToolNames(tools []*mcp.Tool, max int) ([]string, bool) {
	names := make([]string, 0, len(tools))
	size := len("[]")
	for _, t := range tools {
		if t == nil {
			continue
		}
		cost := jsonStringLen(t.Name)
		if len(names) > 0 {
			cost++ // 逗号
		}
		if max > 0 && size+cost > max {
			return names, true
		}
		size += cost
		names = append(names, t.Name)
	}
	return names, false
}

// jsonStringLen 返回 s 被 encoding/json 编码成 JSON 字符串后的字节数（含两个引号）。
//
// 逐字节算而不是 marshal 一遍：后者在几千个工具名的场景下就是几千次小分配。
// 覆盖 encoding/json 默认会做的全部转义：" \ 与控制字符、HTML 转义（< > &）、
// 行分隔符 U+2028/U+2029，以及非法字节被替换成 \ufffd（转义形式，6 字节）。
func jsonStringLen(s string) int {
	n := len(`""`)
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			switch {
			case b == '"' || b == '\\' || b == '\n' || b == '\r' || b == '\t':
				n += 2
			case b < 0x20, b == '<', b == '>', b == '&':
				n += 6 // \u003c 这类形式
			default:
				n++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			n += 6 // 非法字节 -> \ufffd（encoding/json 输出的是转义形式，不是裸的 3 字节）
		case r == '\u2028' || r == '\u2029':
			n += 6
		default:
			n += size
		}
		i += size
	}
	return n
}
