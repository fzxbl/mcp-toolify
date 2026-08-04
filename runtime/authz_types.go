package runtime

// Capability 表示一个工具的读/写类别（由其 mcp:tags 是否含 write 派生）。
type Capability int

const (
	ReadOnly Capability = iota
	ReadWrite
)

// RiskLevel 表示单个工具的风险等级；缺省为 RiskNone。
type RiskLevel int

const (
	RiskNone RiskLevel = iota
	RiskLow
	RiskMedium
	RiskHigh
)

// ParseRisk 解析 mcp:risk 标记或配置中的风险字符串。空串视为 none。
func ParseRisk(s string) (RiskLevel, bool) {
	switch s {
	case "", "none":
		return RiskNone, true
	case "low":
		return RiskLow, true
	case "medium":
		return RiskMedium, true
	case "high":
		return RiskHigh, true
	default:
		return RiskNone, false
	}
}

// String 返回风险等级的可读名（用于错误文案）。
func (r RiskLevel) String() string {
	switch r {
	case RiskLow:
		return "low"
	case RiskMedium:
		return "medium"
	case RiskHigh:
		return "high"
	default:
		return "none"
	}
}
