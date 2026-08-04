package case_risk

// DangerOp 高危写操作。
//
// param: target — 目标
//
// mcp:tool
// mcp:tags=write
// mcp:risk=high
func DangerOp(target string) error {
	_ = target
	return nil
}

// SafeRead 只读查询。
//
// param: id — 对象 ID
//
// mcp:tool
func SafeRead(id string) (string, error) {
	return id, nil
}
