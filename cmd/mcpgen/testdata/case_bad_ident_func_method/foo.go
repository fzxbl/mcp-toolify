package case_bad_ident_func_method

// Orders 订单服务。
type Orders struct{}

// Orders_Get 导出函数，名字合法但与方法 Get 的标识符冲突。
//
// mcp:tool
// mcp:name=ident.orders_get_fn
func Orders_Get() string { return "" }

// Get 方法工具。
//
// mcp:tool
// mcp:name=ident.orders_get_method
func (o *Orders) Get() string { return "" }
