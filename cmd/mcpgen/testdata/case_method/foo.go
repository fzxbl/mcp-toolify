package case_method

import "context"

// Orders 订单服务（指针接收者）。
type Orders struct{ Prefix string }

// Get 按 ID 查询订单。
//
// param: id — 订单 ID
//
// mcp:tool
func (o *Orders) Get(ctx context.Context, id string) (string, error) {
	return o.Prefix + id, ctx.Err()
}

// List 列出订单。
//
// mcp:tool
// mcp:name=case_method.orders_list
func (o *Orders) List() []string {
	return []string{o.Prefix}
}

// Users 用户服务（值接收者）。
type Users struct{}

// List 列出用户。
//
// mcp:tool
// mcp:name=case_method.users_list
func (u Users) List(ctx context.Context) []string {
	_ = ctx
	return nil
}

// Ping 包级函数。
//
// mcp:tool
func Ping() string { return "pong" }

// Counter 底层为 int 的命名类型。
type Counter int

// Inc 加 n。
//
// param: n — 增量
//
// mcp:tool
func (c Counter) Inc(n int) int { return int(c) + n }
