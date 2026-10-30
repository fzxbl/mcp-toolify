package case_bad_collision

// Bad 参数撞名（userID 与 UserId 的导出名相同）。
//
// mcp:tool
func Bad(userID string, UserId string) error { return nil }
