package case_iface

// Patch 含 interface{} 字段，模拟 SpecOperator。
type Patch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// Selector 是带方法的命名接口入参，需要 mcp:bind 绑定到具体类型。
type Selector interface {
	Sel() map[string]string
}

// Label 是 Selector 的一个具体实现。
type Label struct {
	App string `json:"app"`
}

// Sel 实现 Selector。
func (l Label) Sel() map[string]string { return map[string]string{"app": l.App} }

// ApplyPatch 应用一个含任意值的 patch。
//
// param: cluster — 集群名
// param: patch — JSON Patch 操作
//
// 返回：error 表示是否成功。
//
// mcp:tool
// mcp:tags=write
func ApplyPatch(cluster string, patch Patch) error {
	_ = cluster
	_ = patch
	return nil
}

// FilterBy 按 selector 过滤。
//
// param: cluster — 集群名
// param: label — 选择器
//
// 返回：命中数量；error。
//
// mcp:tool
// mcp:bind=label:Label
func FilterBy(cluster string, label Selector) (count int, err error) {
	_ = cluster
	_ = label
	return 0, nil
}

// Stat 返回多个非 error 值，需打包成 map。
//
// param: cluster — 集群名
//
// 返回：总数、就绪数。
//
// mcp:tool
func Stat(cluster string) (total int, ready int) {
	_ = cluster
	return 0, 0
}
