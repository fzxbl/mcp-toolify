package runtime

import (
	"sync"
	"time"
)

// 本文件支撑 spill 资源的跨副本访问基础设施。分布式部署下 spill 文件只在产出它的实例本地，
// 跨副本路由由 WithOwnerRouting（见 owner_routing.go）在最外层把归属兄弟副本的 tools/call
// 反代到属主实例执行。转发目标受兄弟副本白名单约束（见 SetSpillPeerProvider），共享密钥为空
// 时相关跨副本能力关闭（安全默认）。

const defaultPeerTimeout = 5000 * time.Millisecond

var (
	peerMu      sync.RWMutex
	peerToken   string
	peerTimeout = defaultPeerTimeout
	// peerProvider 实时返回兄弟副本 host:port 列表，作为代理转发目标白名单来源。
	// 通过它对接任意服务发现机制，无需静态配置。nil 表示无兄弟副本。
	peerProvider func() []string
)

// SetSpillPeer 设置副本间代理的共享密钥与超时。timeoutMS<=0 归一化为默认 5000ms。
// 转发白名单不在此设置——见 SetSpillPeerProvider / SetSpillPeers。
func SetSpillPeer(token string, timeoutMS int) {
	peerMu.Lock()
	defer peerMu.Unlock()
	peerToken = token
	if timeoutMS <= 0 {
		peerTimeout = defaultPeerTimeout
	} else {
		peerTimeout = time.Duration(timeoutMS) * time.Millisecond
	}
}

// SetSpillPeerProvider 注册动态兄弟副本发现函数：每次代理白名单校验时实时调用它，返回当前
// 可达的兄弟副本 host:port 列表。用它对接任意服务发现，无需静态配置。传 nil 清除（回退到
// 无兄弟副本，即拒绝一切远端转发）。与 SetSpillPeers 互为覆盖，后调用者生效。
func SetSpillPeerProvider(fn func() []string) {
	peerMu.Lock()
	peerProvider = fn
	peerMu.Unlock()
}

// SetSpillPeers 设置静态兄弟副本列表（通常来自配置），内部包装成返回该快照的 provider。
// 与 SetSpillPeerProvider 互为覆盖：后调用者生效。空列表等价于无兄弟副本。
func SetSpillPeers(hosts []string) {
	snapshot := append([]string{}, hosts...)
	SetSpillPeerProvider(func() []string { return snapshot })
}

// spillPeerList 实时取兄弟副本列表；未注册 provider 时返回 nil（等价无兄弟副本）。
func spillPeerList() []string {
	peerMu.RLock()
	fn := peerProvider
	peerMu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn()
}

// SpillPeerToken 返回共享密钥；空串表示跨副本代理关闭。
func SpillPeerToken() string {
	peerMu.RLock()
	defer peerMu.RUnlock()
	return peerToken
}

// SpillPeerTimeout 返回代理调用超时。
func SpillPeerTimeout() time.Duration {
	peerMu.RLock()
	defer peerMu.RUnlock()
	return peerTimeout
}

// SpillPeerAllowed 返回 hostPort 是否为当前已知的兄弟副本之一（仅这些地址允许被代理）。
// provider 未注册或返回空时返回 false（默认拒绝一切远端转发，防 SSRF/密钥外泄的安全默认）。
func SpillPeerAllowed(hostPort string) bool {
	for _, p := range spillPeerList() {
		if p == hostPort {
			return true
		}
	}
	return false
}

// NewSpillIDForOwnerTest 供其它包测试构造指定归属的 spill id。
func NewSpillIDForOwnerTest(hostPort string) string { return newSpillIDWithOwner(hostPort) }

// SpillDirForTest 返回当前全局 spill store 目录，供跨包测试落文件。
func SpillDirForTest() string { return spillStoreOrDefault().dir }
