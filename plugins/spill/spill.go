// Package spill 是大结果落盘插件：超过阈值的工具返回值写进本地文件，返回值改写为
// 摘要 + 可下载的 spill id/URL，避免把几十兆内容塞进模型上下文。
//
// 安装位置：应**最后**安装，位于洋葱最内层——返回时第一个被唤醒，拿到的是未经改写的
// 原始结果，才能按真实大小判断是否落盘。装在 audit 之外会让审计记到大结果原文。
//
// 使用方必须知道的七条：
//
//  1. 判定按**序列化后（wire）总字节**算，不是 payload 字节：JSON 转义会让文本膨胀、
//     base64 会让二进制膨胀 4/3，按 payload 判定是**低估**（fail-open）。估算走零分配
//     的长度计算，覆盖 TextContent 之外的 ImageContent / AudioContent /
//     EmbeddedResource / ResourceLink 与 structuredContent；无法序列化的值按「超大」
//     处理。只看文本的实现挡不住一张 8MB base64 图片。
//  2. 错误态结果（IsError）**同样落盘**：一条几十兆的错误堆栈对上下文的伤害和成功结果
//     没有区别。改写后的结果保持 IsError 不变。
//  3. 落盘失败绝不静默。默认 on_error = "deny"（fail-closed）：宁可让这次调用失败，
//     也不把超过阈值的结果原样返回——那正是本插件要防的事。显式配成 "warn" 才降级为
//     「保留原结果 + 打告警日志」，此时上下文会被大结果撑一次。
//  4. 磁盘有硬上限：max_file_mib（单文件）与 max_total_mib（目录总量），单位是 MiB。超单文件
//     上限直接失败（走 on_error），超总量上限先按最旧优先淘汰，腾不出空间才失败。
//     落盘文件按 ttl 到期回收（gc_interval 为扫描周期），进程退出时只停回收协程、
//     不删文件（正在下载的结果不该因一次重启消失），残留文件由下次启动的首轮扫描清掉。
//     回收只动「本进程创建的 / id 属主是本副本的 / 无属主信息且已超 ttl+24h 的」文件，
//     多个部署共享同一 dir 时不会互删；仍建议每个部署用独立 dir。
//  5. 下载端点 /spill/<id> 由基座套 token 鉴权（Registry.Route），并在此之上做**属主
//     校验**：落盘时记下 Subject.Token（用途名）与有身份时的 Subject.ID，非属主一律
//     404（不是 403，也不回显属主地址——避免存在性与拓扑泄漏）。
//  6. 多副本部署：id 内嵌产出该文件的副本地址，请求打到别的副本时，若该地址在 peer
//     白名单内则反向代理到属主副本（带环路保护头，Authorization 原样透传），否则 404。
//     摘要里的绝对 URL 依赖 PublicBaseURL，它必须是**本副本可直连的地址**，配成负载
//     均衡入口会让下载随机落到非属主副本。
//     **已知风险（复审 M-3，与基座 owner_routing 的既有约定一致，本插件不单独改）**：
//     副本间转发走明文 http://，且把调用方的 Bearer token 原样带给属主副本 ——
//     内网抓包即得该 token。副本跨机部署时请给 peer 之间加 TLS，或改用副本间的
//     内部凭据替代透传调用方 token。
//  7. 落盘内容是工具结果原文，会以文件形式留在磁盘上 ttl 那么久。工具返回值里若有
//     敏感数据，落盘目录的权限（启动时收紧为 0700，目录不属于本进程或收紧失败则拒绝
//     启动）、属主校验与 ttl 就是它的全部保护。目录在校验通过后被以句柄形式持有
//     （os.OpenRoot），启动之后把路径换成软链既改不了写入位置、也读不出别处的文件。
package spill

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// Name 是插件名，与 required_plugins 里的写法一致。
const Name = "spill"

// 落盘失败时的处置策略（对应 [spill] on_error）。
const (
	// OnErrorDeny 为默认值：落盘失败即返回错误，不把超大结果交给模型。
	OnErrorDeny = "deny"
	// OnErrorWarn 保留原结果，但必然打出告警日志（不存在「静默」这一档）。
	OnErrorWarn = "warn"
)

// MetaID 是本插件写进 Call.Meta 的键：本次调用落盘后的 spill id（未落盘时不写）。
//
// 关于「跨插件 key 该由谁声明」的澄清（基座 runtime/call.go 里的纪律要求跨插件读取的
// key 在基座声明，而核心不允许 import 插件，两者看似冲突）：那条纪律管的是**基座自己
// 参与的** key（如 DenyResult 写的 denied_by / deny_reason，基座写、插件读，必须由基座
// 声明常量）。插件自己产生的 key 属于插件私有命名空间，由插件导出常量、并按
// 「插件名.」前缀避免撞名；读取方是插件或宿主，它们**可以** import 本插件拿到这个常量
// （只有核心不允许 import 插件）。不愿意 import 的读取方按字符串约定 "spill.id" 读。
const MetaID = "spill.id"

// 各项默认值。0 一律表示「取默认值」，**没有**关闭落盘或关闭截断的写法：
// 一个「关掉 spill」的开关一旦存在，只会在上下文被打满时才被发现它开着。
const (
	defaultThreshold    = 64 << 10
	defaultTTL          = 30 * time.Minute
	defaultGCInterval   = 5 * time.Minute
	defaultPreviewBytes = 2048
	// 磁盘上限的默认值以 MiB 计（配置项也是 MiB，见 Section）。
	defaultMaxFileMiB  = 64  // 单个 spill 文件 64MiB
	defaultMaxTotalMiB = 512 // 落盘目录总量 512MiB
	// mibShift 是 MiB → 字节的位移。maxQuotaMiB 是配置能写的上限：
	// 再大换算成字节就溢出 int64 变成负数，而负的上限会被 store 当成「不限」。
	mibShift    = 20
	maxQuotaMiB = math.MaxInt64 >> mibShift
)

// Config 对应配置文件的 [spill] 段。
//
// 用具名字段结构体、且字段集与文档一致（基座的配置认领制要求）：用 map 兜底解码会让
// 段内拼错的键被判为「已解码」，`[spill] threshhold_bytes = ...` 于是被完全吞掉、插件按默认
// 阈值上线；少声明一个文档承诺的字段，则部署方照文档写全反而启动失败。
type Config struct {
	Spill Section `toml:"spill"`
}

// Section 是 [spill] 段的字段集，即本插件的对外配置契约。
//
// 字段名自带单位，因为这些数字的量级差得很远：阈值/预览是「一份结果多大」（KiB 级，
// 用字节），磁盘上限是「这个部署最多占多少盘」（MiB 级，用 MiB）。写成统一的裸字节
// 会让 67108864 这类值必须心算，改的时候还容易多打或少打一个 0。
//
// ttl / gc_interval 是**字符串**（"30m"、"2h"）：TOML 没有 duration 类型，
// 而 time.Duration 是 int64，写 ttl = "30m" 会直接解码失败、写 ttl = 1800 又会被
// 当成 1800 纳秒。字符串 + time.ParseDuration 是唯一不会被误读的写法。
type Section struct {
	// Dir 是落盘目录；省略则用 <系统临时目录>/mcp-toolify/spill。
	Dir string `toml:"dir"`
	// ThresholdBytes 是落盘阈值（字节）：结果总字节超过它即落盘。省略或 0 取 64KiB。
	ThresholdBytes int `toml:"threshold_bytes"`
	// TTL 是落盘文件的保留时长（duration 字符串）；省略取 30m。
	TTL string `toml:"ttl"`
	// GCInterval 是回收扫描周期（duration 字符串）；省略取 5m。
	GCInterval string `toml:"gc_interval"`
	// PreviewBytes 是摘要里保留的预览字节数；省略或 0 取 2048。
	PreviewBytes int `toml:"preview_bytes"`
	// MaxFileMiB 是单个 spill 文件的上限（**MiB**）；省略或 0 取 64。
	MaxFileMiB int64 `toml:"max_file_mib"`
	// MaxTotalMiB 是落盘目录的总量上限（**MiB**）；省略或 0 取 512。
	MaxTotalMiB int64 `toml:"max_total_mib"`
	// OnError 是落盘失败时的处置策略：deny（默认，fail-closed）| warn。
	OnError string `toml:"on_error"`
}

// options 是规整化后的生效配置（duration 已解析、默认值已补齐）。
type options struct {
	Dir          string
	Threshold    int
	TTL          time.Duration
	GCInterval   time.Duration
	PreviewBytes int
	Quota        quota
	OnError      string
}

// normalize 校验并补齐默认值。任何写错的项都返回 error 让启动失败——
// 静默用默认值比启动失败危险得多（部署方会以为自己配的阈值/策略在生效）。
func (s Section) normalize() (options, error) {
	o := options{
		Dir: s.Dir, Threshold: s.ThresholdBytes,
		PreviewBytes: s.PreviewBytes, OnError: s.OnError,
	}
	switch o.OnError {
	case "":
		o.OnError = OnErrorDeny
	case OnErrorDeny, OnErrorWarn:
	default:
		return options{}, fmt.Errorf("on_error=%q 不支持，只接受 %q 或 %q",
			o.OnError, OnErrorDeny, OnErrorWarn)
	}
	if o.Threshold < 0 {
		return options{}, fmt.Errorf("threshold_bytes=%d 不能为负（0 表示取默认值 %d）",
			o.Threshold, defaultThreshold)
	}
	if o.PreviewBytes < 0 {
		return options{}, fmt.Errorf("preview_bytes=%d 不能为负（0 表示取默认值 %d）",
			o.PreviewBytes, defaultPreviewBytes)
	}
	if o.Threshold == 0 {
		o.Threshold = defaultThreshold
	}
	if o.PreviewBytes == 0 {
		o.PreviewBytes = defaultPreviewBytes
	}
	var err error
	if o.Quota, err = s.quota(); err != nil {
		return options{}, err
	}
	if o.TTL, err = parsePositiveDuration("ttl", s.TTL, defaultTTL); err != nil {
		return options{}, err
	}
	if o.GCInterval, err = parsePositiveDuration("gc_interval", s.GCInterval,
		defaultGCInterval); err != nil {
		return options{}, err
	}
	if o.Dir == "" {
		if d := defaultDir.Load(); d != nil {
			o.Dir = *d
		}
	}
	if o.Dir == "" {
		o.Dir = filepath.Join(os.TempDir(), "mcp-toolify", "spill")
	}
	return o, nil
}

// quota 校验两个 MiB 上限并换算成字节（store 内部一律按字节记账）。
func (s Section) quota() (quota, error) {
	if s.MaxFileMiB < 0 || s.MaxTotalMiB < 0 {
		return quota{}, fmt.Errorf("max_file_mib=%d / max_total_mib=%d 不能为负"+
			"（0 表示取默认值）", s.MaxFileMiB, s.MaxTotalMiB)
	}
	// 上界不是洁癖：MiB 左移 20 位后若溢出 int64 会变成**负数**，而负的上限被 store
	// 当成「不限」——一个写错的巨大值就会静默关掉磁盘保护。
	if s.MaxFileMiB > maxQuotaMiB || s.MaxTotalMiB > maxQuotaMiB {
		return quota{}, fmt.Errorf("max_file_mib=%d / max_total_mib=%d 太大，"+
			"换算成字节会溢出（上限 %d MiB）", s.MaxFileMiB, s.MaxTotalMiB, int64(maxQuotaMiB))
	}
	fileMiB, totalMiB := s.MaxFileMiB, s.MaxTotalMiB
	if fileMiB == 0 {
		fileMiB = defaultMaxFileMiB
	}
	if totalMiB == 0 {
		totalMiB = defaultMaxTotalMiB
	}
	if fileMiB > totalMiB {
		return quota{}, fmt.Errorf("max_file_mib=%d 大于 max_total_mib=%d："+
			"这样单个文件就能把总量上限顶穿，任何一次大结果都落不下来", fileMiB, totalMiB)
	}
	return quota{MaxFileBytes: fileMiB << mibShift, MaxTotalBytes: totalMiB << mibShift}, nil
}

// defaultDir 是宿主给的落盘目录兜底值，仅在配置没写 dir 时生效。
// 与 current 同属「给宿主的入口」这类包级状态例外（理由见 api.go 的 current）。
var defaultDir atomic.Pointer[string]

// SetDefaultDir 设置「配置里没写 dir 时」使用的落盘目录，必须在 Install 之前调用。
//
// 为什么需要它：落盘目录常常只有**运行期**才知道——GDP 之类的框架把它算成
// `<应用根目录>/data/spill`，容器里又可能是挂进来的卷。静态配置文件写不出这个值，
// 而缺了它就会退回系统临时目录：那是可预测路径、重启即清，工具结果原文不该落在那。
//
// 配置里显式写的 dir 优先：部署方在配置文件里写下的东西必须赢过代码里的默认值，
// 否则「我明明配了却不生效」。传空串清除。
func SetDefaultDir(dir string) { defaultDir.Store(&dir) }

// parsePositiveDuration 解析 duration 字符串；空串取 def，非法或非正数报错。
// 0 也报错：TTL 为 0 意味着文件刚落盘就算过期，等于结果永远取不回来。
func parsePositiveDuration(name, raw string, def time.Duration) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q 不是合法的时长（如 \"30m\"、\"2h\"）: %w", name, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s=%q 必须为正数", name, raw)
	}
	return d, nil
}

// Install 安装 spill 插件。应**最后**安装，使其位于洋葱最内层——返回时第一个被唤醒，
// 拿到未经改写的原始结果才能按真实大小判断是否落盘。
func Install(r *runtime.Registry) error {
	r.Named(Name)
	var cfg Config
	if err := r.Config(&cfg); err != nil {
		return fmt.Errorf("spill 读配置: %w", err)
	}
	o, err := cfg.Spill.normalize()
	if err != nil {
		return fmt.Errorf("[spill] 配置无效: %w", err)
	}
	st, err := newStore(o.Dir, o.TTL, o.GCInterval, o.Quota)
	if err != nil {
		return err
	}
	// 先登记清理钩子再起协程：万一后面的插件 Install 失败、宿主中止启动，
	// 至少这条回收协程是能被 RunStop 收走的。
	r.OnStop(func() {
		// 先摘掉宿主入口再关目录句柄：宿主的后台协程可能正在 Put/Create，
		// 让它拿到 ErrNotInstalled 比让它写进一个已关闭的句柄清楚得多。
		current.Store(nil)
		st.close()
	})
	current.Store(st)
	st.startGC()
	// 把 spill_explore 注册成 MCP 工具，并声明按 id 做 owner 路由：内容只在产出它的
	// 副本本地，探索请求落到别的副本时由基座反代回属主（与 /spill/ 下载同一套约定）。
	r.Tool(addExploreTool)
	runtime.RegisterOwnerRouted(exploreToolName, "id")
	// 启动日志声明生效策略：on_error 决定「落盘失败时这次调用还算不算成功」，
	// 阈值/TTL/上限决定「什么会被落盘、能取回多久、最多占多少盘」，
	// 最后一行声明**下载鉴权粒度** —— 它取决于基座是否给出了真实身份，
	// 部署方必须知道当前生效的是哪一档。
	log.Printf("[mcp] spill: on_error=%s threshold_bytes=%d(序列化后字节) ttl=%s gc_interval=%s "+
		"preview_bytes=%d max_file_mib=%d max_total_mib=%d dir=%s",
		o.OnError, o.Threshold, o.TTL, o.GCInterval, o.PreviewBytes,
		o.Quota.MaxFileBytes>>mibShift, o.Quota.MaxTotalBytes>>mibShift, o.Dir)
	log.Printf("[mcp] spill: 下载鉴权粒度=token 用途名 + 属主有身份时再比对 Subject.ID；"+
		"基座默认不信任身份头（Subject.ID 为空），此时同一 token 用途名的调用方之间"+
		"可以互相下载 %s 结果", downloadPath)
	r.Route(downloadPath, st.downloadHandler())
	r.Use(middleware(o, st))
	return nil
}

// middleware 返回落盘中间件：拿到原始结果，按总字节判定，超阈值即落盘并改写返回值。
func middleware(o options, st *store) runtime.Middleware {
	return func(next runtime.Handler) runtime.Handler {
		return func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
			res, err := next(ctx, c)
			if !spillable(res, err) {
				return res, err
			}
			size := resultBytes(res.Tool, o.Threshold)
			if size <= o.Threshold {
				return res, err
			}
			id, written, perr := st.put(c.Tool, res.Tool, ownerOfSubject(c.Subject))
			if perr != nil {
				return onPutError(res, err, c, o, size, perr)
			}
			c.SetMeta(MetaID, id)
			return rewrite(res.Tool, st, o, id, size, written), err
		}
	}
}

// spillable 判断这份结果是否进入落盘判定。
//
// 只跳过两类：链上出错（没有结果可落）、非 tools/call 的结果（把 tools/list 换成
// 下载链接等于把工具清单藏起来）。
//
// **错误态结果（IsError）也要落盘**：曾经把它当例外放过，结果是「5MiB 的错误文案
// 原样进上下文」——一条静默的超大路径，与本插件「没有静默这一档」的承诺自相矛盾
// （审查 I4）。改写后的结果保留 IsError，模型仍然知道这次调用失败了。
func spillable(res *runtime.Result, err error) bool {
	return err == nil && res != nil && res.Tool != nil
}

// onPutError 按 on_error 策略处置落盘失败。两条路径都必然留日志——
// 「静默把超大结果原样返回」是本插件唯一不能做的事。
func onPutError(res *runtime.Result, err error, c *runtime.Call, o options,
	size int, perr error) (*runtime.Result, error) {
	log.Printf("[mcp] spill error: 落盘失败 on_error=%s logid=%s tool=%s size=%d: %v",
		o.OnError, c.LogID, c.Tool, size, perr)
	if o.OnError == OnErrorDeny {
		// fail-closed：工具已经执行完了（落盘发生在返回路径），所以文案要劝退重试——
		// 目标工具集里可能有删除、覆盖写这类不可逆动作。
		// 刻意不把 perr 原文回给调用方：它带落盘目录等部署细节，已进本地日志。
		return nil, fmt.Errorf("结果过大（%d 字节）且落盘失败，已拒绝返回未落盘的大结果"+
			"（logid=%s）；该工具可能已经执行，请勿直接重试", size, c.LogID)
	}
	return res, err
}

// rewrite 把原结果换成「摘要 + 下载地址 + 预览」。
//
// 新造一个 CallToolResult 而不是改原对象：原结果可能被外层插件（如 audit 的进入时
// 快照语义之外的读取）持有，就地改写等于改别人手里的数据。
// structuredContent 必然被丢掉——生成的工具会把同一份 payload 同时放进 Content 与
// structuredContent（SDK 行为），只改 Content 的话大结果照样过线进模型。
func rewrite(orig *mcp.CallToolResult, st *store, o options,
	id string, size int, written int64) *runtime.Result {
	var b strings.Builder
	fmt.Fprintf(&b, "结果过大（约 %d 字节，超过阈值 %d），已落盘为 spill 资源 %s（保留 %s）。\n",
		size, o.Threshold, id, o.TTL)
	if url := st.url(id); url != "" {
		fmt.Fprintf(&b, "完整内容（%d 字节 JSON）下载：%s\n"+
			"（需带与本次调用相同的 Authorization；该地址指向产出它的副本）\n", written, url)
	} else {
		fmt.Fprintf(&b, "完整内容已写入产出副本本地的 %s（%d 字节）；"+
			"本副本未配置对外地址（PublicBaseURL），故没有下载 URL。\n",
			filepath.Join(o.Dir, id+fileExt), written)
	}
	preview, cut := previewOf(orig.Content, o.PreviewBytes)
	if cut {
		fmt.Fprintf(&b, "预览（前 %d 字节，非完整内容，不要 Unmarshal）：\n", o.PreviewBytes)
	} else {
		b.WriteString("预览：\n")
	}
	b.WriteString(preview)
	return &runtime.Result{Tool: &mcp.CallToolResult{
		Meta: orig.Meta,
		// 保留错误态：错误态的大结果也会落盘（见 spillable），但「这次调用失败了」
		// 这个事实不能在改写中丢掉，否则模型会把一段落盘摘要当成成功结果。
		IsError: orig.IsError,
		Content: []mcp.Content{&mcp.TextContent{Text: b.String()}},
	}}
}

// previewOf 在预算内拼出人可读的预览：文本段取前若干字节，非文本段只写一行描述
// （类型 + MIME + 字节数）。返回是否发生了截断。
//
// 只复制留下的那一段：先把几十兆文本拼成一个 string 再截，等于每次落盘都额外复制一份
// 完整结果——那正是本插件要消除的开销。
func previewOf(content []mcp.Content, budget int) (string, bool) {
	var b strings.Builder
	cut := false
	for _, item := range content {
		if budget <= 0 {
			cut = true
			break
		}
		tc, ok := item.(*mcp.TextContent)
		if !ok {
			line := describe(item)
			b.WriteString(line)
			b.WriteByte('\n')
			budget -= len(line) + 1
			continue
		}
		seg, segCut := truncateString(tc.Text, budget)
		b.WriteString(seg)
		budget -= len(seg)
		if segCut {
			cut = true
			break
		}
	}
	return b.String(), cut
}

// describe 给非文本内容一行描述：预览里不该出现 base64 原文，但「有一张多大的图」
// 是模型判断要不要下载的依据。
func describe(item mcp.Content) string {
	switch c := item.(type) {
	case *mcp.ImageContent:
		return fmt.Sprintf("[图片 %s，%d 字节]", c.MIMEType, len(c.Data))
	case *mcp.AudioContent:
		return fmt.Sprintf("[音频 %s，%d 字节]", c.MIMEType, len(c.Data))
	case *mcp.EmbeddedResource:
		if c.Resource == nil {
			return "[嵌入资源]"
		}
		return fmt.Sprintf("[嵌入资源 %s %s，%d 字节]", c.Resource.URI, c.Resource.MIMEType,
			len(c.Resource.Text)+len(c.Resource.Blob))
	case *mcp.ResourceLink:
		return fmt.Sprintf("[资源链接 %s]", c.URI)
	default:
		return fmt.Sprintf("[%T]", item)
	}
}

// truncateString 把 s 截到最多 n 字节，只复制留下的那一段，并返回是否截断。
func truncateString(s string, n int) (string, bool) {
	if n <= 0 {
		return "", len(s) > 0
	}
	if len(s) <= n {
		return s, false
	}
	return string(trimPartialRune([]byte(s[:n]))), true
}

// trimPartialRune 去掉尾部那个被切断的字符，最多丢 utf8.UTFMax-1 个字节。
// 不整串校验合法性：工具结果里中段就有非法字节完全可能（二进制混进文本），
// 那种写法会一路退到空串，把一段可读的预览整个丢掉。
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

// resultBytes 估算一份结果**序列化后（wire）**的字节数上界，用于阈值判定。
//
// 四条刻意的设计：
//   - 量的是 wire 字节，不是 payload 字节。payload 字节是 wire 的**下界**，按它判定
//     属于 fail-open：60000 个 \x01 的文本 payload 只有 60000 字节（小于默认阈值），
//     JSON 转义后是 360039 字节；60000 字节的图片 base64 后是 80063 字节。两者按
//     payload 判定都会原样进上下文，正是本插件要防的事。
//   - 大字段不做 json.Marshal：文本按「JSON 字符串编码后长度」逐字节算
//     （jsonStringLen），二进制按 base64.StdEncoding.EncodedLen 算，都不产生中间副本。
//     Meta / Annotations / Icons / Size 这类小字段**非 nil 时**才真的序列化一次
//     （nil 走快路径按 "null" 记 4 字节）；未知内容类型也只能序列化后量。
//     常见结果里这些字段都是 nil，此时整条估算路径零分配（见
//     TestSizeEstimationDoesNotAllocateOnNilFields）—— 但请不要把它当成
//     「任何输入都零分配」：带 Annotations 的结果每段会多一次小对象序列化。
//   - 估算恒 >= 真实 wire 字节（骨架用宽松上界 itemOverhead），偏大的一侧：
//     宁可让略低于阈值的结果也落盘，也不让超阈值的结果漏出去。
//   - 一旦已经超过阈值就立刻返回：后面量多少都不改变结论（尤其能省掉对
//     structuredContent 的那次序列化）。
func resultBytes(res *mcp.CallToolResult, threshold int) int {
	total := len(`{"content":[],"structuredContent":}`)
	for i, item := range res.Content {
		if i > 0 {
			total++ // 逗号
		}
		total = addSize(total, contentBytes(item))
		if total > threshold {
			return total
		}
	}
	return addSize(total, structuredBytes(res.StructuredContent))
}

// unknownSize 表示「体积无法预估」（序列化失败）。刻意取一个大值而不是 0：
// 按 0 处理等于给这类结果开一条绕过落盘的路，而落盘时同样的序列化会再失败一次、
// 走 on_error 策略——那是显式的失败，比静默放过一份体积未知的结果好。
const unknownSize = math.MaxInt32

// itemOverhead 是单段内容 JSON 骨架（type/mimeType 等键名、括号、逗号）的宽松上界。
// 取宽松值是为了保证整体估算不低估；代价是每段最多高估约一百多字节。
const itemOverhead = 128

// addSize 加法并在 unknownSize 处饱和，避免多段未知体积累加溢出。
func addSize(a, b int) int {
	if a > unknownSize-b {
		return unknownSize
	}
	return a + b
}

// contentBytes 返回单段内容序列化后的字节数上界。
func contentBytes(item mcp.Content) int {
	switch c := item.(type) {
	case *mcp.TextContent:
		return itemOverhead + jsonStringLen(c.Text) + extraBytes(c.Meta) + extraBytes(c.Annotations)
	case *mcp.ImageContent:
		return itemOverhead + jsonStringLen(c.MIMEType) + base64Len(len(c.Data)) +
			extraBytes(c.Meta) + extraBytes(c.Annotations)
	case *mcp.AudioContent:
		return itemOverhead + jsonStringLen(c.MIMEType) + base64Len(len(c.Data)) +
			extraBytes(c.Meta) + extraBytes(c.Annotations)
	case *mcp.EmbeddedResource:
		n := itemOverhead + extraBytes(c.Meta) + extraBytes(c.Annotations)
		if c.Resource != nil {
			n += jsonStringLen(c.Resource.URI) + jsonStringLen(c.Resource.MIMEType) +
				jsonStringLen(c.Resource.Text) + base64Len(len(c.Resource.Blob)) +
				extraBytes(c.Resource.Meta)
		}
		return n
	case *mcp.ResourceLink:
		return itemOverhead + jsonStringLen(c.URI) + jsonStringLen(c.Name) +
			jsonStringLen(c.Title) + jsonStringLen(c.Description) +
			jsonStringLen(c.MIMEType) +
			extraBytes(c.Meta) + extraBytes(c.Annotations) +
			extraBytes(c.Icons) + extraBytes(c.Size)
	default:
		// 未知内容类型（SDK 的 ToolUseContent 等，或后续新增的形态）只能序列化后量：
		// 漏量一种类型就等于给它开一条绕过落盘的路。这类内容按协议都很小。
		b, err := json.Marshal(item)
		if err != nil {
			return unknownSize
		}
		return len(b)
	}
}

// structuredBytes 返回 structuredContent 序列化后的字节数。
//
// 生成的工具给的是 json.RawMessage（SDK 直接透传原始字节），这条最常见的路径量它
// 只是取长度。其余类型（含 []byte —— SDK 会把它编码成 base64 字符串）无从预估，
// 只能序列化一次后量；由 resultBytes 的短路保证「Content 已经超阈值」时不会走到这里。
func structuredBytes(v any) int {
	if v == nil {
		return 0
	}
	if raw, ok := v.(json.RawMessage); ok {
		return len(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return unknownSize
	}
	return len(b)
}

// extraBytes 返回一个可选小字段（Meta / Annotations / Icons / Size）序列化后的长度。
//
// nil 走快路径直接记 4 字节（"null"），不进 json.Marshal —— 常见结果这几个字段全是
// nil，原来的写法对每段内容都要为 nil 白白 marshal 两次（实测 2 allocs/op，
// 复审 M-1）。非 nil 时才真序列化：按 MCP 协议它们都是小对象，代价可以忽略。
// 一个参数一个调用（不用可变参数）：可变参数会为 []any 分配一次，
// 那会让「nil 字段零分配」这条根本达不到。序列化失败即认为体积未知。
func extraBytes(v any) int {
	if isNilField(v) {
		return len(`null`)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return unknownSize
	}
	return len(b)
}

// isNilField 判断一个可选字段是否为 nil。
//
// 不能只写 v == nil：把 nil 的 map / 指针 / 切片装进 any 之后，接口本身非 nil。
// 逐类型判而不用 reflect：这是阈值判定的热路径，且这几个类型由 SDK 的内容结构固定。
func isNilField(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case mcp.Meta:
		return t == nil
	case *mcp.Annotations:
		return t == nil
	case []mcp.Icon:
		return t == nil
	case *int64:
		return t == nil
	}
	return false
}

// base64Len 返回 n 字节二进制被编码成 JSON 字符串后的长度（含两个引号）。
// SDK 把 ImageContent.Data / ResourceContents.Blob 这类 []byte 按标准 base64
// 输出，膨胀 4/3 —— 按原始字节数判定阈值会漏掉这部分。
func base64Len(n int) int { return base64.StdEncoding.EncodedLen(n) + len(`""`) }

// jsonStringLen 返回 s 被 encoding/json 编码成 JSON 字符串后的字节数（含两个引号）。
//
// 逐字节算而不是 marshal 一遍：本函数用在阈值判定的热路径上，几十兆文本 marshal 一次
// 就是一次完整的内存副本，而我们只要长度。
// 覆盖 encoding/json 默认会做的全部转义：" \ 与控制字符、HTML 转义（< > &）、
// 行分隔符 U+2028/U+2029，以及非法字节被替换成 \ufffd（**转义形式，6 字节**，
// 不是裸的 3 字节 —— audit 插件在这里踩过一次）。
//
// 与 plugins/audit 里的同名函数是有意的重复实现：插件之间不互相 import
// （装一个插件不该把另一个拖进编译产物），这点重复换来的是插件间零耦合。
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
			n += 6 // 非法字节 -> \ufffd（转义形式）
		case r == '\u2028' || r == '\u2029':
			n += 6
		default:
			n += size
		}
		i += size
	}
	return n
}
