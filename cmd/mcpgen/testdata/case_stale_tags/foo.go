package case_stale_tags

// StaleTags 使用已废弃的 mcp:tags 标记，应报错并提示迁移到 mcp:labels。
//
// mcp:tool
// mcp:tags=write
func StaleTags() error { return nil }
