package runtime

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// 本文件支撑 spill 资源的跨副本访问。分布式部署下 spill 文件只在产出它的实例本地，
// spill_explore 若被路由到别的实例会本地未命中；此时据 id 里编码的归属地址（见 spill_id.go）
// 单跳代理到属主实例的内部 /spill-explore 端点取回小结果。端点用共享密钥鉴权，转发目标
// 受兄弟副本白名单约束（见 SetSpillPeerProvider），共享密钥为空则整套代理关闭（安全默认）。

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

// SpillOwner 从 id 解出归属 host:port；ok=false 表示旧式/无归属 id。
func SpillOwner(id string) (hostPort string, ok bool) { return splitOwner(id) }

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

// LocalSpillExplore 是“本地-only” explore 执行器，由 spillexplore 包在 init 注入，
// 避免 runtime 反向依赖工具包。内部端点只调用它、不再转发，从结构上保证单跳。
var LocalSpillExplore func(id, op string, lineOffset, limit int, pattern, jqExpr string, depth, maxBytes int) (map[string]any, error)

// spillExploreReq 是 /spill-explore 的请求体（对齐 SpillExplore 参数）。
type spillExploreReq struct {
	ID         string `json:"id"`
	Op         string `json:"op"`
	LineOffset int    `json:"lineOffset"`
	Limit      int    `json:"limit"`
	Pattern    string `json:"pattern"`
	JQExpr     string `json:"jqExpr"`
	Depth      int    `json:"depth"`
	MaxBytes   int    `json:"maxBytes"`
}

// SpillExploreEndpoint 返回副本间内部 explore 端点：校验共享密钥后在本地执行 explore
// （LocalSpillExplore），只回传小结果。共享密钥未配置时整端点禁用（404）。请求体设 1MB 上限。
func SpillExploreEndpoint() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := SpillPeerToken()
		if token == "" { // 特性未开启
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		got := r.Header.Get("X-Spill-Peer-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req spillExploreReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if LocalSpillExplore == nil {
			http.Error(w, "local explore not wired", http.StatusInternalServerError)
			return
		}
		res, err := LocalSpillExplore(req.ID, req.Op, req.LineOffset, req.Limit, req.Pattern, req.JQExpr, req.Depth, req.MaxBytes)
		if err != nil {
			http.Error(w, "spill resource not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})
}

// NewSpillIDForOwnerTest 供其它包测试构造指定归属的 spill id。
func NewSpillIDForOwnerTest(hostPort string) string { return newSpillIDWithOwner(hostPort) }

// SpillDirForTest 返回当前全局 spill store 目录，供跨包测试落文件。
func SpillDirForTest() string { return spillStoreOrDefault().dir }
