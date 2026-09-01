package runtime

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fzxbl/mcp-toolify/selector"
)

// TokenConfig 是单个 token 的配置（对应 [[tokens]]）。
type TokenConfig struct {
	Token     string   `toml:"token"`
	Name      string   `toml:"name"`      // 用途标识，进审计
	Applicant string   `toml:"applicant"` // 申请人，仅配置内留痕
	Allow     []string `toml:"allow"`     // selector 列表，OR
	Deny      []string `toml:"deny"`      // selector 列表，OR，优先于 allow
	// Identity 把这个 token 绑定到一个固定的调用人身份，写法为 "fixed:<id>"。
	// 配了它就不看任何请求头（也不受 trust_identity_header 影响）：
	// 服务账号类 token 的身份本来就是确定的，让请求头能改它等于白送一个冒充入口。
	Identity string `toml:"identity"`
}

// TokenAuthzConfig 是 token 准入与身份解析的配置（对应 TOML 里的 [[tokens]]、
// identity_headers 与 trust_identity_header），供配置加载器一次性解出后交给 NewTokenAuthz。
type TokenAuthzConfig struct {
	Tokens []TokenConfig `toml:"tokens"`
	// IdentityHeaders 指定取调用人身份的请求头名（有序）：按从前到后的顺序，取第一个
	// 在请求中非空的头值作为身份。缺省/为空时用 defaultIdentityHeader（X-MCP-User）。
	// 仅在 TrustIdentityHeader 为 true 时生效。
	IdentityHeaders []string `toml:"identity_headers"`
	// TrustIdentityHeader 决定是否信任客户端送来的身份头。
	//
	// 默认 false（不信任）：身份头是客户端可以随手伪造的，而 Subject.ID 是配额计数键、
	// 二次确认归属这类判据的唯一来源。默认信任的话，「部署时漏了那层会重写身份头的
	// 可信网关」不会有任何报错，只会静默变成人人可冒充——静默失效比启动失败危险。
	// 只有把这个开关显式打开（声明「我前面确实有可信网关」）才启用 IdentityHeaders。
	TrustIdentityHeader bool `toml:"trust_identity_header"`
}

// fixedIdentityPrefix 是 [[tokens]].identity 唯一支持的写法前缀。
const fixedIdentityPrefix = "fixed:"

// tokenRule 是一个 token 解析后的准入规则。构造后只读。
type tokenRule struct {
	name string
	// identity 是该 token 绑定的固定身份；空串表示未绑定。
	identity string
	allow    []selector.Selector
	deny     []selector.Selector
}

// allows 判定该规则是否允许带此 labels 的工具：
// deny 优先（命中任一 deny 即拒），其后 allow 必须命中任一，都不命中即拒（deny-by-default）。
func (r *tokenRule) allows(labels map[string]string) bool {
	if selector.MatchAny(r.deny, labels) {
		return false
	}
	return selector.MatchAny(r.allow, labels)
}

// TokenAuthz 按 token 判定工具的可见性与可执行性：判据只有工具 labels
// （对基座不透明）与配置里的 allow/deny selector。deny 优先，两者都不命中即拒绝。
//
// 不变量：NewTokenAuthz 返回后全部字段只读，可并发使用。刻意不提供任何 setter——
// ResolveIdentity 与 Middleware 都跑在请求路径上，启动后改字段就是 data race。
//
// 按人（identity）的授权不在基座：基座只把身份解析进 Subject.ID，
// 具体判定由使用方写插件实现。
type TokenAuthz struct {
	byToken map[string]*tokenRule // token 值 -> 规则
	byName  map[string]*tokenRule // token 用途名 -> 规则
	// identityHeaders 是取调用人身份的有序请求头名（来自配置，缺省 [X-MCP-User]）。
	identityHeaders []string
	// trustIdentityHeader 为 false（默认）时完全不读身份头，见 TokenAuthzConfig。
	trustIdentityHeader bool
}

// defaultIdentityHeader 是调用人身份的默认来源 HTTP 头名。
const defaultIdentityHeader = "X-MCP-User"

// NewTokenAuthz 从配置构造 TokenAuthz。缺 token/name/applicant、token 值或 name 重复、
// selector 语法错误一律返回 error，调用方应据此让启动失败（fail-fast，而非带着坏规则上线）。
//
// 错误信息只用配置里的序号与 name 定位条目，绝不回显 token 值（错误会进日志/终端）。
func NewTokenAuthz(cfg TokenAuthzConfig) (*TokenAuthz, error) {
	az := &TokenAuthz{
		byToken:             map[string]*tokenRule{},
		byName:              map[string]*tokenRule{},
		identityHeaders:     normalizeIdentityHeaders(cfg.IdentityHeaders),
		trustIdentityHeader: cfg.TrustIdentityHeader,
	}
	seen := map[string]int{} // token 值 -> 首次出现的序号
	for i, c := range cfg.Tokens {
		tk := strings.TrimSpace(c.Token)
		if tk == "" {
			return nil, fmt.Errorf("tokens[%d] 缺少 token（必填）", i)
		}
		if strings.TrimSpace(c.Name) == "" {
			return nil, fmt.Errorf("tokens[%d] 缺少 name（token 用途标识，必填）", i)
		}
		if strings.TrimSpace(c.Applicant) == "" {
			return nil, fmt.Errorf("tokens[%d] (name=%s) 缺少 applicant（申请人标识，必填）", i, c.Name)
		}
		if first, dup := seen[tk]; dup {
			return nil, fmt.Errorf("tokens[%d] (name=%s) 的 token 与 tokens[%d] 重复", i, c.Name, first)
		}
		seen[tk] = i
		if _, dup := az.byName[c.Name]; dup {
			return nil, fmt.Errorf("tokens[%d] 的 name=%s 与前面的条目重复", i, c.Name)
		}
		r := &tokenRule{name: c.Name}
		var err error
		if r.identity, err = parseFixedIdentity(c.Identity); err != nil {
			return nil, fmt.Errorf("tokens[%d] (name=%s) identity: %w", i, c.Name, err)
		}
		if r.allow, err = parseSelectors(c.Allow); err != nil {
			return nil, fmt.Errorf("tokens[%d] (name=%s) allow: %w", i, c.Name, err)
		}
		if r.deny, err = parseSelectors(c.Deny); err != nil {
			return nil, fmt.Errorf("tokens[%d] (name=%s) deny: %w", i, c.Name, err)
		}
		az.byToken[tk] = r
		az.byName[c.Name] = r
		// 打印生效规则原文，便于上线自检；只打 name 与 selector，不打 token 值。
		log.Printf("[mcp] token %s identity=%s allow=%v deny=%v",
			c.Name, describeIdentitySource(r, cfg.TrustIdentityHeader), c.Allow, c.Deny)
		if needsMissingKeyWarning(r.allow, r.deny) {
			log.Printf("[mcp] warning: token %s 的 allow 用了 !=/notin，但 deny 没有缺失 label 的兜底条件："+
				"k8s 语义下 !=/notin 对缺失该 key 的工具匹配成功，忘记标注的新工具会被放行；"+
				"建议对你用于分级的 label 加 !<key> 条件（例如 !risk）", c.Name)
		}
	}
	az.logIdentityMode()
	return az, nil
}

// parseFixedIdentity 解析 [[tokens]].identity。空串表示未绑定固定身份；
// 非空时只接受 "fixed:<非空 id>"，其余写法（含误以为支持的 "header:xxx"）一律报错——
// 静默忽略一个写错的身份绑定，等于让这个 token 的身份悄悄变空。
func parseFixedIdentity(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !strings.HasPrefix(raw, fixedIdentityPrefix) {
		return "", fmt.Errorf("%q 写法不支持，只接受 fixed:<id>", raw)
	}
	id := strings.TrimSpace(strings.TrimPrefix(raw, fixedIdentityPrefix))
	if id == "" {
		return "", fmt.Errorf("%s", "fixed: 后面缺少身份标识")
	}
	return id, nil
}

// describeIdentitySource 给出某 token 的身份来源，用于启动日志自检。
func describeIdentitySource(r *tokenRule, trustHeader bool) string {
	switch {
	case r.identity != "":
		return "fixed:" + r.identity
	case trustHeader:
		return "header"
	default:
		return "none"
	}
}

// logIdentityMode 打印当前进程的身份来源模式：这是「我以为按人授权在生效」这类
// 误解的唯一运行期信号。
func (a *TokenAuthz) logIdentityMode() {
	if a.trustIdentityHeader {
		log.Printf("[mcp] identity mode: 信任请求头 %v（trust_identity_header=true）——"+
			"务必确保这些头由可信网关重写，不能透传客户端原值", a.IdentityHeaders())
		return
	}
	fixed := 0
	for _, r := range a.byName {
		if r.identity != "" {
			fixed++
		}
	}
	log.Printf("[mcp] identity mode: 不信任请求头（trust_identity_header 未开启），"+
		"%d/%d 个 token 绑定了固定身份；其余 token 的 Subject.ID 恒为空，"+
		"依赖按人判定的插件需自行处理空身份", fixed, len(a.byName))
}

// parseSelectors 解析一组 selector 表达式，任一语法错误即返回 error。
func parseSelectors(exprs []string) ([]selector.Selector, error) {
	out := make([]selector.Selector, 0, len(exprs))
	for _, e := range exprs {
		s, err := selector.Parse(e)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// needsMissingKeyWarning 判断某 token 的规则是否存在「缺失 label 被放行」的洞：
// 只有 allow 里用了否定型操作符（!= / notin，k8s 语义下对缺失 key 匹配成功）
// 且 deny 里没有 DoesNotExist 兜底时才成立。
//
// 等值型 allow（capability=read）本来就匹配不上未标注的工具，没有洞——对这种配置报警
// 属于喊狼，很快就没人看了。判据取自解析结果而非表达式字符串：字符串包含判断会把
// "capability!=read" 误判成 DoesNotExist 兜底。
func needsMissingKeyWarning(allow, deny []selector.Selector) bool {
	return hasOp(allow, selector.OpNotEqual, selector.OpNotIn) && !hasOp(deny, selector.OpNotExist)
}

// hasOp 判断一组 selector 里是否存在使用了任一指定操作符的条件。
func hasOp(ss []selector.Selector, ops ...selector.Op) bool {
	for _, s := range ss {
		for _, r := range s.Requirements {
			for _, op := range ops {
				if r.Op == op {
					return true
				}
			}
		}
	}
	return false
}

// TokenName 按 token 值取其用途名；token 未配置时 ok=false（HTTP 层据此 401）。
// 它只做查表，不含任何「能力/风险上限」语义。
func (a *TokenAuthz) TokenName(token string) (name string, ok bool) {
	r, ok := a.byToken[token]
	if !ok {
		return "", false
	}
	return r.name, true
}

// ruleOf 按 token 用途名取规则；查不到返回 nil（调用方一律按拒绝处理）。
func (a *TokenAuthz) ruleOf(tokenName string) *tokenRule {
	return a.byName[tokenName]
}

// allowed 判定某 token 用途名能否使用带该 labels 的工具；用途名查不到规则即拒。
func (a *TokenAuthz) allowed(tokenName string, labels map[string]string) bool {
	r := a.ruleOf(tokenName)
	return r != nil && r.allows(labels)
}

// Middleware 返回基座的准入中间件：tools/call 拦截、tools/list 过滤，其余方法透传。
// 它不是插件，由基座固定装在插件链之前。
//
// 不变量（两条路径必须一致，否则「不可见」被误当成访问控制）：
//   - 判据只取自工具注册表（LookupTool + AllLabels），不信任 Call.Labels——链上任何
//     一环都能改它，而注册表是拷贝出来的权威值。
//   - 判据在**进入链之前**取一次并留在本地变量里：Call.Subject 是指针，插件在 next
//     之前改一行就能换掉规则；tools/list 的过滤发生在 next 返回之后，若那时才读
//     Subject.Token，一个中间件就能把自己提权成可见范围更大的 token。
//   - 注册表里查不到 labels 的工具（直接注册在 mcp.Server 上、或忘了登记的外部工具）
//     既不可见也不可执行（deny-by-default）；只隐藏不拦截等于没有访问控制。
func (a *TokenAuthz) Middleware() Middleware {
	return func(next Handler) Handler {
		return func(ctx context.Context, c *Call) (*Result, error) {
			// 本中间件是整条链的最外层，此刻还没有任何插件跑过，Subject.Token 仍是
			// 认证阶段填的那个值。取到本地变量之后，链内怎么改 Call 都换不掉判据。
			rule := a.ruleOf(subjectToken(c))
			switch c.Method {
			case "tools/call":
				if ok, reason := checkCall(rule, c); !ok {
					// 必须留一条日志：本中间件固定装在**插件链之前**，因此它的拒绝
					// 根本不会到达 audit 插件（终审实测：只读 token 的三次越权尝试在
					// 审计流里 0 条事件，HTTP 状态还是 200）。少了这行，基座自己的
					// 访问控制就成了全系统唯一不可观测的管控动作。
					// 只记 token 用途名不记 token 值（理由见 checkCall 注释）。
					authzDenyAlarm.warn("准入拒绝: token=%s tool=%s reason=%s",
						ruleName(rule), c.Tool, reason)
					return DenyResult(c, "token_authz", reason), nil
				}
				return next(ctx, c)
			case "tools/list":
				res, err := next(ctx, c)
				if err != nil || res == nil || res.List == nil {
					return res, err
				}
				before := len(res.List.Tools)
				res.List.Tools = visibleTools(rule, res.List.Tools)
				// 过滤发生在 next **返回之后**，所以链内的 audit 记到的是过滤**前**的
				// 全量清单（终审实测：只读 token 的审计事件里有它根本看不到的写工具）。
				// 这一行是「这个 token 实际看见了几个」的唯一信号；缺了它，复盘时会
				// 按审计记录得出与事实相反的结论。
				if hidden := before - len(res.List.Tools); hidden > 0 {
					authzListAlarm.warn("tools/list 过滤: token=%s 可见 %d 个、隐藏 %d 个"+
						"（审计事件记的是过滤前的全量清单）",
						ruleName(rule), len(res.List.Tools), hidden)
				}
				return res, nil
			default:
				return next(ctx, c)
			}
		}
	}
}

// authzDenyAlarm / authzListAlarm 限流基座准入的两条可观测性日志。
// 这两条都由调用方触发（一个跑错配置的客户端能刷满日志），必须限流。
var (
	authzDenyAlarm = &warnThrottle{interval: 10 * time.Second}
	authzListAlarm = &warnThrottle{interval: time.Minute}
)

// ruleName 取规则的用途名；rule 为 nil（未知 token 用途名）时给一个可辨识的占位，
// 绝不回退成打印传入的 Subject.Token —— 那是唯一可能泄漏 token 值的路径。
func ruleName(rule *tokenRule) string {
	if rule == nil {
		return "<unknown>"
	}
	return rule.name
}

// checkCall 判定一次 tools/call 是否放行，不放行时给出原因。
// rule 由 Middleware 在进入链之前捕获，不再从 Call 上重新解析。
//
// 原因文案绝不回显传入的 Subject.Token：它本该是配置里的用途名，但上游填错时可能是
// token 值，而拒绝原因会同时进日志与返回给调用方（这是唯一可能泄漏 token 值的路径）。
func checkCall(rule *tokenRule, c *Call) (bool, string) {
	if rule == nil {
		// 这类故障的表现（工具全没了）与「selector 拼错导致 0 匹配」完全一样，
		// 必须留一条日志才能区分；同样不回显传入值。
		log.Printf("[mcp] warning: 收到未知 token 用途名的调用（tool=%s），"+
			"请检查 HTTP 层是否把 token 用途名而非 token 值填进了 Subject.Token", c.Tool)
		return false, "unknown token name"
	}
	info, ok := LookupTool(c.Tool)
	if !ok {
		return false, fmt.Sprintf("工具 %s 未登记 labels", c.Tool)
	}
	if !rule.allows(info.AllLabels()) {
		return false, fmt.Sprintf("token %s 无权执行 %s", rule.name, c.Tool)
	}
	return true, ""
}

// visibleTools 过滤 tools/list 的结果，返回该规则可见的工具。
// 返回新切片而不是原地复用底层数组：上游（SDK / 链上其它环节 / 插件）可能仍持有
// 那个切片，原地改写会让一个 token 的过滤结果污染另一个 token 看到的清单。
func visibleTools(rule *tokenRule, all []*mcp.Tool) []*mcp.Tool {
	visible := make([]*mcp.Tool, 0, len(all))
	if rule == nil {
		log.Printf("[mcp] warning: 收到未知 token 用途名的 tools/list，" +
			"请检查 HTTP 层是否把 token 用途名而非 token 值填进了 Subject.Token")
		return visible
	}
	for _, t := range all {
		info, ok := LookupTool(t.Name)
		if !ok {
			continue // 未登记 labels 的工具不可见（deny-by-default，与 call 路径一致）
		}
		if rule.allows(info.AllLabels()) {
			visible = append(visible, t)
		}
	}
	return visible
}

// subjectToken 取 Call 上的 token 用途名；Subject 缺失时返回空串，
// 空串在 byName 里查不到规则，等同于全拒（fail-closed）。
//
// 判断标准（勿按「上次删了某处 nil 防御」的先例删它）：nil 防御只在能带来
// fail-closed 语义时才写——这里能；只能掩盖 bug、让错误更晚暴露的 nil 防御不写。
func subjectToken(c *Call) string {
	if c.Subject == nil {
		return ""
	}
	return c.Subject.Token
}

// normalizeIdentityHeaders 去空白、去空项、保序去重（大小写不敏感）。
func normalizeIdentityHeaders(names []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		key := strings.ToLower(n)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, n)
	}
	return out
}

// IdentityHeaders 返回取调用人身份的有序请求头名；未配置时回退 [X-MCP-User]。
func (a *TokenAuthz) IdentityHeaders() []string {
	if len(a.identityHeaders) > 0 {
		return a.identityHeaders
	}
	return []string{defaultIdentityHeader}
}

// ResolveIdentity 按配置的头名顺序取第一个非空值。它只是「读头」这一步，
// 不含信任判定——是否采用这个值由 SubjectOf 按 trust_identity_header 决定。
// get 通常为 http.Header.Get。
func (a *TokenAuthz) ResolveIdentity(get func(name string) string) string {
	for _, h := range a.IdentityHeaders() {
		if v := strings.TrimSpace(get(h)); v != "" {
			return v
		}
	}
	return ""
}

// SubjectOf 按 token 值构造调用主体；token 未配置时 ok=false（HTTP 层据此 401）。
//
// 身份（Subject.ID）的来源优先级，默认不信任客户端：
//  1. token 上绑定的 identity = "fixed:<id>"：确定值，完全不看请求头；
//  2. trust_identity_header=true 时才读 identity_headers；
//  3. 都没有则为空串——由需要按人判定的插件自己决定怎么处理空身份。
//
// 之所以默认取空而不是取头值：Subject.ID 是配额计数键与二次确认归属的唯一判据，
// 默认信任会让「部署时漏了重写身份头的网关」变成静默的人人可冒充。
func (a *TokenAuthz) SubjectOf(token string, get func(name string) string) (*Subject, bool) {
	r, ok := a.byToken[token]
	if !ok {
		return nil, false
	}
	id := r.identity
	if id == "" && a.trustIdentityHeader {
		id = a.ResolveIdentity(get)
	}
	return &Subject{ID: id, Token: r.name}, true
}

// HTTPMiddleware 是基座的 HTTP 认证层，装在 MCP handler 与插件路由之外：
// 解析 Authorization 里的 token → 查用途名 → 未登记的 token 一律 401（fail-closed），
// 合法则把 Subject{Token: 用途名, ID: 身份} 注入 ctx，供 newCall 投影进 Call。
//
// 只注入不判定：能不能看见/执行某个工具由 Middleware() 在 MCP 层按 labels 判。
// 401 文案刻意不回显收到的 token，避免密文进日志或返回给调用方。
func (a *TokenAuthz) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, ok := a.SubjectOf(bearerToken(r.Header.Get("Authorization")), r.Header.Get)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
			http.Error(w, "invalid or missing token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), sub)))
	})
}

// bearerToken 取 Authorization 头里的 token：带 "Bearer " 前缀时剥掉前缀，
// 否则原样返回（容许客户端直接填裸 token）。空串在 byToken 里查不到，即 401。
func bearerToken(h string) string {
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return strings.TrimSpace(h)
}
