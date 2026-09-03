package runtime

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// 本文件是基座的「有归属 id」设施：多副本部署下，有状态插件产出的资源（如 spill 的落盘
// 文件、外置审批插件的待确认回执）只存在处理该次调用的那个副本本地，后续请求若被
// 负载均衡打到别的副本就找不到。为此把「产出该资源的副本地址（host:port）」编码进 id，
// 由 owner 路由（见 owner_routing.go）把归属兄弟副本的 tools/call 整条反代到属主副本。
//
// id 形如 "<owner-seg>.<random-hex>"：owner-seg 首字符是版本标记（'4'=打包 IPv4:port，
// 'h'=base32 编码的 host:port 字符串），其后为 base32(小写,无填充)。无归属信息（未配置
// 对外地址时）的 id 为纯 32-hex 随机串，不含 '.'，行为与单机时完全一致。

const ownedIDSep = "."

// b32 为小写、无填充的 base32 编码器（字符集 a-z2-7，对文件名与 URL 均安全）。
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func b32encodeLower(b []byte) string { return strings.ToLower(b32.EncodeToString(b)) }

func b32decodeLower(s string) ([]byte, bool) {
	b, err := b32.DecodeString(strings.ToUpper(s))
	if err != nil {
		return nil, false
	}
	return b, true
}

// encodeOwner 把 "host:port" 编码为 owner-seg。IPv4 打包成 6 字节（4+2），
// 否则退回 "host:port" 字符串的 base32。返回空串表示 hostPort 非法。
func encodeOwner(hostPort string) string {
	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		return ""
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return ""
	}
	if ip := net.ParseIP(host).To4(); ip != nil {
		var buf [6]byte
		copy(buf[:4], ip)
		binary.BigEndian.PutUint16(buf[4:], uint16(port))
		return "4" + b32encodeLower(buf[:])
	}
	return "h" + b32encodeLower([]byte(hostPort))
}

// decodeOwner 是 encodeOwner 的逆操作。ok=false 表示无法解码为合法 host:port。
func decodeOwner(seg string) (hostPort string, ok bool) {
	if len(seg) < 2 {
		return "", false
	}
	body, dok := b32decodeLower(seg[1:])
	if !dok {
		return "", false
	}
	switch seg[0] {
	case '4':
		if len(body) != 6 {
			return "", false
		}
		ip := net.IPv4(body[0], body[1], body[2], body[3])
		port := binary.BigEndian.Uint16(body[4:])
		return net.JoinHostPort(ip.String(), strconv.Itoa(int(port))), true
	case 'h':
		hp := string(body)
		if _, _, err := net.SplitHostPort(hp); err != nil {
			return "", false
		}
		return hp, true
	default:
		return "", false
	}
}

// randomHex 返回 n 字节随机数的十六进制串。
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newIDForOwner 生成带指定归属的 id；hostPort 为空或非法时退回纯 32-hex 随机串。
func newIDForOwner(hostPort string) string {
	if hostPort == "" {
		return randomHex(16)
	}
	seg := encodeOwner(hostPort)
	if seg == "" {
		return randomHex(16)
	}
	return seg + ownedIDSep + randomHex(8)
}

// NewOwnedID 生成内嵌本副本 host:port 的不透明 id，供有状态插件使用。
// 未配置对外地址（未调用 SetPublicBaseURL）时退回纯随机 id，行为与单机一致。
func NewOwnedID() string { return newIDForOwner(SelfHostPort()) }

// OwnerOf 解出 id 内嵌的属主 host:port；ok=false 表示无归属 id。
func OwnerOf(id string) (hostPort string, ok bool) {
	i := strings.Index(id, ownedIDSep)
	if i <= 0 {
		return "", false
	}
	return decodeOwner(id[:i])
}

var (
	baseMu       sync.RWMutex
	publicBase   string // 对外可直连的基础地址（不含末尾斜杠），形如 http://host:8011
	selfHostPort string // 从 base 解析出的 host:port，用于判断某 id 是否归属本副本
)

// SetPublicBaseURL 设置本副本对外可直连的基础地址；插件拼下载 URL、基座判 id 归属
// 都以它为准。传空串表示没有对外地址（此时生成的 id 不带归属信息）。
func SetPublicBaseURL(base string) {
	baseMu.Lock()
	defer baseMu.Unlock()
	publicBase = strings.TrimRight(base, "/")
	selfHostPort = hostPortFromBase(publicBase)
}

// PublicBaseURL 返回本副本对外基础地址；未设置时为空串。
func PublicBaseURL() string {
	baseMu.RLock()
	defer baseMu.RUnlock()
	return publicBase
}

// SelfHostPort 返回本副本对外可达的 host:port；未设置对外地址时为空串。
func SelfHostPort() string {
	baseMu.RLock()
	defer baseMu.RUnlock()
	return selfHostPort
}

// hostPortFromBase 从 "http(s)://host:port[/...]" 提取 "host:port"；无法解析返回空。
func hostPortFromBase(base string) string {
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

var (
	peerMu sync.RWMutex
	// peerProvider 实时返回兄弟副本 host:port 列表，作为 owner 路由的转发白名单来源。
	// 通过它对接任意服务发现机制，无需静态配置。nil 表示无兄弟副本。
	peerProvider func() []string
)

// SetPeerProvider 注册动态兄弟副本发现函数：每次白名单校验时实时调用它，返回当前
// 可达的兄弟副本 host:port 列表。传 nil 清除（回退到拒绝一切远端转发）。
// 与 SetPeers 互为覆盖，后调用者生效。
func SetPeerProvider(fn func() []string) {
	peerMu.Lock()
	peerProvider = fn
	peerMu.Unlock()
}

// SetPeers 设置静态兄弟副本列表（通常来自配置），内部包装成返回该快照的 provider。
// 与 SetPeerProvider 互为覆盖：后调用者生效。空列表等价于无兄弟副本。
func SetPeers(hosts []string) {
	snapshot := append([]string{}, hosts...)
	SetPeerProvider(func() []string { return snapshot })
}

// peerList 实时取兄弟副本列表；未注册 provider 时返回 nil（等价无兄弟副本）。
func peerList() []string {
	peerMu.RLock()
	fn := peerProvider
	peerMu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn()
}

// PeerAllowed 返回 hostPort 是否为当前已知的兄弟副本之一（仅这些地址允许被反代）。
// provider 未注册或返回空时返回 false：默认拒绝一切远端转发，防 SSRF。
func PeerAllowed(hostPort string) bool {
	for _, p := range peerList() {
		if p == hostPort {
			return true
		}
	}
	return false
}
