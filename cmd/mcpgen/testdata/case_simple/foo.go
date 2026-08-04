package case_simple

// GetThing 查询某个对象。
//
// param: id — 对象 ID
// param: verbose — 是否返回详情
//
// mcp:tool
func GetThing(id string, verbose bool) (string, error) {
	return id, nil
}

// notExposed 不带标记，应被忽略。
func notExposed() {}
