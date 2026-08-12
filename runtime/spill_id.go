package runtime

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"net"
	"strconv"
	"strings"
)

// 分布式部署下，spill 文件只存在处理该次调用的那个实例本地磁盘上；后续 spill_explore
// 若被负载均衡路由到另一个实例，本地就找不到文件。为此把"产出该资源的实例地址（host:port）"
// 编码进 spill id：读取时若本地未命中且 id 归属为其它实例，就据此单跳代理到属主实例取回。
//
// id 形如 "<owner-seg>.<random-hex>"：owner-seg 首字符是版本标记（'4'=打包 IPv4:port，
// 'h'=base32 编码的 host:port 字符串），其后为 base32(小写,无填充)。无归属信息（旧式或
// owner 为空，如 stdio）时 id 为纯 16-hex 随机串，不含 '.'，行为与单机时完全一致。

const spillIDSep = "."

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

// newSpillIDWithOwner 生成带归属的 id；hostPort 为空或非法时退回纯 16-hex 随机（旧式）。
func newSpillIDWithOwner(hostPort string) string {
	if hostPort == "" {
		return newSpillID()
	}
	seg := encodeOwner(hostPort)
	if seg == "" {
		return newSpillID()
	}
	return seg + spillIDSep + randomHex(8)
}

// splitOwner 从 id 解出归属 host:port；ok=false 表示旧式/无归属 id。
func splitOwner(id string) (hostPort string, ok bool) {
	i := strings.Index(id, spillIDSep)
	if i <= 0 {
		return "", false
	}
	return decodeOwner(id[:i])
}
