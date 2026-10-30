package case_bad_variadic

// Bad 可变参数。
//
// mcp:tool
func Bad(xs ...int) error { return nil }
