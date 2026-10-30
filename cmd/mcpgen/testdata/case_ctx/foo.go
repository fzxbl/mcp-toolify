package case_ctx

import "context"

// Lookup 按 ID 查询。
//
// param: id — 对象 ID
//
// mcp:tool
func Lookup(ctx context.Context, id string) (string, error) {
	_ = ctx
	return id, nil
}

// Ping 仅带 ctx。
//
// mcp:tool
func Ping(ctx context.Context) error {
	return ctx.Err()
}

// Pair 多返回值。
//
// mcp:tool
func Pair(ctx context.Context, x int) (a int, b int, err error) {
	return x, x + 1, ctx.Err()
}

// Blank 首位 ctx 为 _。
//
// mcp:tool
func Blank(_ context.Context, y int) (int, error) {
	return y, nil
}
