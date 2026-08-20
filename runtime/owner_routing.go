package runtime

import "sync"

// OwnerOf 解出 owned spill id 内嵌的属主 host:port（见 spill_id.go）。
// ok=false 表示旧式/无归属 id。中性命名，供通用 owner 路由复用。
func OwnerOf(id string) (hostPort string, ok bool) { return splitOwner(id) }

var (
	ownerRoutedMu sync.RWMutex
	ownerRouted   = map[string]string{} // toolName -> routing param name
)

// RegisterOwnerRouted 声明「工具 toolName 按参数 paramName（owned id）路由」。
// 应在 init/启动期调用，早于开始处理请求。
func RegisterOwnerRouted(toolName, paramName string) {
	ownerRoutedMu.Lock()
	ownerRouted[toolName] = paramName
	ownerRoutedMu.Unlock()
}

func ownerRoutedParam(toolName string) (string, bool) {
	ownerRoutedMu.RLock()
	defer ownerRoutedMu.RUnlock()
	p, ok := ownerRouted[toolName]
	return p, ok
}
