package case_dup_labels

// DupLabels 同一个 label key 写两行属真冲突，必须报错。
//
// mcp:tool
// mcp:labels=risk=low
// mcp:labels=risk=high
func DupLabels() error { return nil }
