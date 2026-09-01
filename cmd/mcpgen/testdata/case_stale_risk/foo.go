package case_stale_risk

// StaleRisk 使用已废弃的 mcp:risk 标记，应报错并提示迁移到 mcp:labels。
//
// mcp:tool
// mcp:risk=high
func StaleRisk() error { return nil }
