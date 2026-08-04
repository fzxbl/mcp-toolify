package runtime

import (
	"fmt"
	"strings"
)

// RiskAuthorizer 判定某身份是否可执行到指定风险等级。
//
// 本期由 staticAuthorizer（读配置白名单）实现；未来可替换为按邮件组成员
// 每 20min 刷新的实现——仅需实现该接口，其余代码不动。
type RiskAuthorizer interface {
	CanRun(identity string, level RiskLevel) bool
}

// TokenEntry 是单个 token 的配置：审计元信息 + 分别设置读/写允许的最高风险等级。
// read/write 取 none|low|medium|high；省略某字段表示该类操作完全不允许。
type TokenEntry struct {
	Token string `toml:"token"`
	// Name 是 token 的用途标识（如 readonly-agent），会写入审计日志的 token_name 字段，
	// 用于定位一次调用走的是哪条接入通道。必填。
	Name string `toml:"name"`
	// Applicant 是申请人 uuap，仅留在配置里用于审计追溯（按 token_name 反查），不进日志。必填。
	Applicant string `toml:"applicant"`
	Read      string `toml:"read"`  // 读操作最高风险；空 => 不允许读
	Write     string `toml:"write"` // 写操作最高风险；空 => 不允许写
}

// TokenCaps 是一个 token 解析后的读/写风险上限。
// ReadOK/WriteOK 为 false 表示该类操作完全不允许。
type TokenCaps struct {
	ReadOK  bool
	Read    RiskLevel
	WriteOK bool
	Write   RiskLevel
	// Name 是 token 的用途标识，仅用于审计日志，不参与权限判定。
	Name string
}

// AuthzConfig 是 HTTP 鉴权的配置（对应 conf/mcp/mcp.toml）。
type AuthzConfig struct {
	Tokens        []TokenEntry        `toml:"tokens"`
	RiskAllowlist map[string][]string `toml:"risk_allowlist"` // level -> 身份列表
}

// Validate 校验 token 配置的基本合法性与审计元信息完整性：
// token 为空、token 重复、缺 name、缺 applicant 均返回 error，
// 启动阶段应据此失败，避免上线不可用或无法追溯来源的 token。
//
// 错误信息用配置里的序号定位条目，绝不回显 token 值（错误会进日志/终端）。
// 存在多个问题时只报第一个。
func (c AuthzConfig) Validate() error {
	seen := map[string]int{} // token -> 首次出现的序号
	for i, e := range c.Tokens {
		tk := strings.TrimSpace(e.Token)
		if tk == "" {
			return fmt.Errorf("tokens[%d] 缺少 token（必填）", i)
		}
		if first, dup := seen[tk]; dup {
			return fmt.Errorf("tokens[%d] 的 token 与 tokens[%d] 重复", i, first)
		}
		seen[tk] = i
		if strings.TrimSpace(e.Name) == "" {
			return fmt.Errorf("tokens[%d] 缺少 name（token 用途标识，必填）", i)
		}
		if strings.TrimSpace(e.Applicant) == "" {
			return fmt.Errorf("tokens[%d] (name=%s) 缺少 applicant（申请人 uuap，必填）", i, e.Name)
		}
	}
	return nil
}

// staticAuthorizer 基于静态白名单判定。none/low 对所有人放行；
// medium/high 要求身份在对应等级白名单内。
type staticAuthorizer struct {
	allow map[RiskLevel]map[string]bool
}

func newStaticAuthorizer(allowlist map[string][]string) *staticAuthorizer {
	m := map[RiskLevel]map[string]bool{}
	for levelStr, ids := range allowlist {
		level, ok := ParseRisk(levelStr)
		if !ok {
			continue
		}
		set := map[string]bool{}
		for _, id := range ids {
			set[id] = true
		}
		m[level] = set
	}
	return &staticAuthorizer{allow: m}
}

// CanRun 判定某身份能否执行到指定风险等级。none/low 对所有人放行；
// medium/high 要求身份在【不低于】该等级的任一白名单内——即权限向下覆盖：
// 被授权 high 的人自动涵盖 medium/low。
func (s *staticAuthorizer) CanRun(identity string, level RiskLevel) bool {
	if level <= RiskLow {
		return true
	}
	if identity == "" {
		return false
	}
	for l := level; l <= RiskHigh; l++ {
		if s.allow[l][identity] {
			return true
		}
	}
	return false
}

// Authz 聚合 token->读写风险上限 映射与按人风险判定。
type Authz struct {
	tokens map[string]TokenCaps
	RiskAuthorizer
}

// NewAuthz 从配置构造 Authz。
func NewAuthz(cfg AuthzConfig) *Authz {
	tokens := map[string]TokenCaps{}
	for _, e := range cfg.Tokens {
		read, readOK := parseCeiling(e.Read)
		write, writeOK := parseCeiling(e.Write)
		tokens[e.Token] = TokenCaps{
			ReadOK:  readOK,
			Read:    read,
			WriteOK: writeOK,
			Write:   write,
			Name:    e.Name,
		}
	}
	return &Authz{
		tokens:         tokens,
		RiskAuthorizer: newStaticAuthorizer(cfg.RiskAllowlist),
	}
}

// parseCeiling 解析 token 的读/写风险上限字段：空 => 该类不允许；
// 非法值同样视为不允许（保守）。
func parseCeiling(s string) (RiskLevel, bool) {
	if strings.TrimSpace(s) == "" {
		return RiskNone, false
	}
	lvl, ok := ParseRisk(s)
	if !ok {
		return RiskNone, false
	}
	return lvl, true
}

// CapsOf 返回 token 对应的读/写风险上限；token 缺失/未配置时 ok=false（应拒绝连接）。
func (a *Authz) CapsOf(token string) (TokenCaps, bool) {
	c, ok := a.tokens[token]
	return c, ok
}
