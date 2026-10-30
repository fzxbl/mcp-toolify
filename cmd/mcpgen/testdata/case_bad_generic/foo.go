package case_bad_generic

// Bad 泛型函数。
//
// mcp:tool
func Bad[T any](x T) error { return nil }
