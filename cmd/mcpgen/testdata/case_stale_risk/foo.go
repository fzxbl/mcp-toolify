package case_stale_risk

// StaleRisk 使用非当前契约的 mcp:risk 标记，应按未知标记报错。
//
// mcp:tool
// mcp:risk=high
func StaleRisk() error { return nil }
