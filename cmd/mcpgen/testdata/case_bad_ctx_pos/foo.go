package case_bad_ctx_pos

import "context"

// Bad ctx 不在首位。
//
// mcp:tool
func Bad(x int, ctx context.Context) error { return nil }
