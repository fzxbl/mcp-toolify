package runtime

// ToolMeta 记录单个工具的鉴权相关元数据，由生成代码在注册时登记。
type ToolMeta struct {
	Name       string
	Capability Capability
	Risk       RiskLevel
}

// toolMetaReg 在进程启动的注册阶段被单线程写入，运行期只读，无需加锁。
var toolMetaReg = map[string]ToolMeta{}

// RegisterMeta 登记一个工具的元数据（供生成代码调用）。
func RegisterMeta(m ToolMeta) {
	toolMetaReg[m.Name] = m
}

// LookupMeta 查询工具元数据。未登记的工具（如内置 spill resource）返回 false。
func LookupMeta(name string) (ToolMeta, bool) {
	m, ok := toolMetaReg[name]
	return m, ok
}
