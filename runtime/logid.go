package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// defaultLogIDHeader 是解析/回写 logid 的默认 HTTP 头名。
const defaultLogIDHeader = "X-Log-Id"

// LogConfig 是日志相关配置（对应基座 TOML 的 [log] 段，由 Registry 一并解码）。
type LogConfig struct {
	// LogIDHeader 是读取入站 logid 的请求头名，同时作为回写响应头名。
	// 缺省用 defaultLogIDHeader（X-Log-Id）。
	LogIDHeader string `toml:"logid_header"`
}

// logIDHeaderName 是进程内生效的 logid 头名，读多写少（仅启动 set 一次）。
var logIDHeaderName atomic.Value

func init() {
	logIDHeaderName.Store(defaultLogIDHeader)
}

// SetLogIDHeader 设置 logid 头名；空值归一化为默认值。
func SetLogIDHeader(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = defaultLogIDHeader
	}
	logIDHeaderName.Store(name)
}

// logIDHeader 返回当前生效的 logid 头名。
func logIDHeader() string {
	if s, ok := logIDHeaderName.Load().(string); ok && s != "" {
		return s
	}
	return defaultLogIDHeader
}

type logIDKey struct{}

// WithLogID 把 logid 注入 ctx。
func WithLogID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, logIDKey{}, id)
}

// LogIDFromContext 取出 ctx 中的 logid，缺失返回 ""。
func LogIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(logIDKey{}).(string)
	return id
}

// generateLogID 生成一个随机 logid（16 位十六进制）。随机源不可用时回退到固定串，
// 不阻塞请求。
func generateLogID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// HTTPLogID 解析或生成本次请求的 logid：优先取配置头（默认 X-Log-Id），缺失或**不合规**
// 则生成；注入 ctx（供审计/access 日志读取），并回写同名响应头供上游网关串联。
//
// 为什么要校验而不是原样采信：logid 是「宿主 access log ↔ 审计事件 ↔ 框架日志」三条线
// 唯一的 join key，而它来自请求头、完全由调用方控制。终审探针实测：原样采信时
// `X-Log-Id: fake logid=deadbeef tool=greeter.greet actor=admin` 会被整串写进审计事件与
// 日志行，而框架日志是无引号的 key=value 形状，等于让调用方往日志里注入伪造字段；
// 300 字节的头也照收。校验后不合规就丢弃自生成，注入面随之消失。
//
// **仍然做不到的事**（对账时要知道）：合规的 logid 不做去重，调用方每次发同一个值，
// 审计事件与 access log 就没法一对一。需要严格一对一时请在网关侧保证唯一。
func HTTPLogID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := logIDHeader()
		id := strings.TrimSpace(r.Header.Get(name))
		if !validLogID(id) {
			if id != "" {
				logIDAlarm.warn("入站 logid 不合规（长度 %d，要求 1-%d 位 %s），已丢弃并自生成",
					len(id), maxLogIDLen, "[A-Za-z0-9._:-]")
			}
			id = generateLogID()
		}
		w.Header().Set(name, id)
		next.ServeHTTP(w, r.WithContext(WithLogID(r.Context(), id)))
	})
}

// maxLogIDLen 是入站 logid 的长度上限。取 64 是因为常见 trace id（32 位十六进制的
// W3C traceparent、UUID 带连字符 36 位）都在其内，再长只可能是塞了别的东西。
const maxLogIDLen = 64

// logIDAlarm 限流「入站 logid 不合规」告警：这条由调用方触发，不限流会被刷屏。
var logIDAlarm = &warnThrottle{interval: time.Minute}

// validLogID 判定入站 logid 是否可采信。字符集刻意收窄到日志与 JSON 里都无歧义的一组：
// 空格、引号、等号、换行都不接受——那些正是「往 key=value 日志里注入字段」的材料。
func validLogID(s string) bool {
	if s == "" || len(s) > maxLogIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c == '.' || c == '_' || c == '-' || c == ':':
		default:
			return false
		}
	}
	return true
}
