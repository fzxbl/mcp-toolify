package case_typo_marker

// TypoMarker 标记名拼错（mcp:label 漏了 s），必须报错而非静默忽略。
//
// mcp:tool
// mcp:label=capability=write
func TypoMarker() error { return nil }
