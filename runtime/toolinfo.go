package runtime

import "sync"

// ToolInfo 是一个工具的元数据。Labels 对基座不透明：基座只做投影与匹配，
// 不解释任何 key 的语义（risk / capability 等均由配置与插件解释）。
type ToolInfo struct {
	Name   string
	Pkg    string
	Labels map[string]string
}

// AllLabels 返回含内置投影 name/pkg 的完整 label 集合，供 selector 匹配。
// name/pkg 在拷贝之后覆写：工具自带的同名 label 不得覆盖真实值，
// 否则可以靠伪造 label 骗过基于 selector 的过滤。
func (t ToolInfo) AllLabels() map[string]string {
	out := make(map[string]string, len(t.Labels)+2)
	for k, v := range t.Labels {
		out[k] = v
	}
	out["name"] = t.Name
	out["pkg"] = t.Pkg
	return out
}

var (
	toolsMu sync.RWMutex
	tools   = map[string]ToolInfo{}
)

// copyLabels 深拷贝一份 label map；nil 入参返回 nil。
// 注册表的进出两侧都必须走它：ToolInfo 是值类型，但 Labels 只是 map 头，
// 不拷贝就等于把注册表内部数据交给调用方读写。
func copyLabels(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// RegisterTool 登记一个工具的元数据，由生成代码在注册工具时调用。
// Labels 会被拷贝一份，避免注册表与调用方共享同一个 map。
func RegisterTool(t ToolInfo) {
	cp := ToolInfo{Name: t.Name, Pkg: t.Pkg, Labels: copyLabels(t.Labels)}
	toolsMu.Lock()
	defer toolsMu.Unlock()
	tools[t.Name] = cp
}

// LookupTool 查单个工具。返回的 Labels 是拷贝：调用方改它既不会与并发的
// LookupTool 撞成 concurrent map read/write，也改不动注册表里的 authz 判据。
func LookupTool(name string) (ToolInfo, bool) {
	toolsMu.RLock()
	defer toolsMu.RUnlock()
	t, ok := tools[name]
	if !ok {
		return ToolInfo{}, false
	}
	t.Labels = copyLabels(t.Labels)
	return t, true
}

// Tools 返回全部已注册工具，Labels 同样是拷贝（理由见 LookupTool）。
func Tools() []ToolInfo {
	toolsMu.RLock()
	defer toolsMu.RUnlock()
	out := make([]ToolInfo, 0, len(tools))
	for _, t := range tools {
		t.Labels = copyLabels(t.Labels)
		out = append(out, t)
	}
	return out
}

// ResetToolsForTest 清空注册表，仅测试使用。
// 线上调用会清空全部工具元数据，并因此影响 authz 对未登记工具的判定。
func ResetToolsForTest() {
	toolsMu.Lock()
	defer toolsMu.Unlock()
	tools = map[string]ToolInfo{}
}
