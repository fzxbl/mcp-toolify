package case_empty_labels

// EmptyLabels 写了 mcp:labels 但值为空，属于「以为标了实际没标」，应报错。
//
// mcp:tool
// mcp:labels=
func EmptyLabels() error { return nil }
