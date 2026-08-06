package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"

	"github.com/BurntSushi/toml"
)

// defaultLogIDHeader 是解析/回写 logid 的默认 HTTP 头名。
const defaultLogIDHeader = "X-Log-Id"

// LogConfig 是日志相关配置（对应 mcp.toml 的 [log] 段）。
type LogConfig struct {
	// LogIDHeader 是读取入站 logid 的请求头名，同时作为回写响应头名。
	// 缺省用 defaultLogIDHeader（X-Log-Id）。
	LogIDHeader string `toml:"logid_header"`
}

// logFileConfig 对应整个 mcp.toml，只取其中的 [log] 段。
type logFileConfig struct {
	Log LogConfig `toml:"log"`
}

// LoadLogConfig 从指定 TOML 文件读取 [log] 段。
func LoadLogConfig(path string) (LogConfig, error) {
	var cfg logFileConfig
	_, err := toml.DecodeFile(path, &cfg)
	return cfg.Log, err
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

// InitLogConfig 从 mcp.toml 加载 [log] 段并设置 logid 头名。
// path 为空 / 文件不存在 / 解析失败时静默或告警后回退默认头名——不影响启动。
func InitLogConfig(path string) {
	if path == "" {
		SetLogIDHeader("")
		return
	}
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[mcp] stat log config %s failed: %v, use default logid header %q", path, err, defaultLogIDHeader)
		}
		SetLogIDHeader("")
		return
	}
	cfg, err := LoadLogConfig(path)
	if err != nil {
		log.Printf("[mcp] load log config %s failed: %v, use default logid header %q", path, err, defaultLogIDHeader)
		SetLogIDHeader("")
		return
	}
	SetLogIDHeader(cfg.LogIDHeader)
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

// HTTPLogID 解析或生成本次请求的 logid：优先取配置头（默认 X-Log-Id），缺失则生成；
// 注入 ctx（供审计/access 日志读取），并回写同名响应头供上游网关串联。
func HTTPLogID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := logIDHeader()
		id := strings.TrimSpace(r.Header.Get(name))
		if id == "" {
			id = generateLogID()
		}
		w.Header().Set(name, id)
		next.ServeHTTP(w, r.WithContext(WithLogID(r.Context(), id)))
	})
}
