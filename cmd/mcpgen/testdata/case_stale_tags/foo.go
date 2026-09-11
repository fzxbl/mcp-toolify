package case_stale_tags

// StaleTags 使用非当前契约的 mcp:tags 标记，应按未知标记报错。
//
// mcp:tool
// mcp:tags=write
func StaleTags() error { return nil }
