package case_multiline

// MultiLine 多行 mcp:labels 应被合并成一组 label。
//
// param: target — 目标
//
// mcp:tool
// mcp:labels=capability=write
// mcp:labels=risk=high
func MultiLine(target string) error {
	_ = target
	return nil
}
