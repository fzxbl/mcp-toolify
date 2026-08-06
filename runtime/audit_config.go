package runtime

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"

	"github.com/BurntSushi/toml"
)

// AuditConfig 是审计相关配置（对应 mcp.toml 的 [audit] 段）。
type AuditConfig struct {
	// Headers 是需要额外写入 MCP 审计日志的请求头名称列表。
	// 每个 header 会以独立字段落在审计日志里，字段名为该 header 名的小写形式
	// （如 X-Tenant-Id -> x-tenant-id）。缺失或空值记为 "-"。
	Headers []string `toml:"headers"`
}

// auditFileConfig 对应整个 mcp.toml，只取其中的 [audit] 段（其余段由各自的 loader 解析）。
type auditFileConfig struct {
	Audit AuditConfig `toml:"audit"`
}

// LoadAuditConfig 从指定 TOML 文件读取 [audit] 段。
func LoadAuditConfig(path string) (AuditConfig, error) {
	var cfg auditFileConfig
	_, err := toml.DecodeFile(path, &cfg)
	return cfg.Audit, err
}

// auditHeaderNames 是进程内生效的审计 header 名单（原样保留配置顺序与大小写）。
// 用 atomic.Value 存 []string，读多写少（仅启动时 set 一次）。
var auditHeaderNames atomic.Value

func init() {
	auditHeaderNames.Store([]string(nil))
}

// SetAuditHeaders 设置需要进审计日志的 header 名单（去空白、去空项，按小写保序去重）。
func SetAuditHeaders(names []string) {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		key := strings.ToLower(n)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, n)
	}
	auditHeaderNames.Store(out)
}

// auditHeaders 返回当前生效的审计 header 名单。
func auditHeaders() []string {
	v, _ := auditHeaderNames.Load().([]string)
	return v
}

// InitAuditConfig 从 mcp.toml 加载 [audit] 段并设置审计 header 名单。
// path 为空或文件不存在（stdio / 单测场景）静默置空；其他 stat / 解析失败则告警后置空——
// 审计 header 配置不正确不应导致服务起不来。
func InitAuditConfig(path string) {
	if path == "" {
		SetAuditHeaders(nil)
		return
	}
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[mcp] stat audit config %s failed: %v, disable audit headers", path, err)
		}
		SetAuditHeaders(nil)
		return
	}
	cfg, err := LoadAuditConfig(path)
	if err != nil {
		log.Printf("[mcp] load audit config %s failed: %v, disable audit headers", path, err)
		SetAuditHeaders(nil)
		return
	}
	SetAuditHeaders(cfg.Headers)
}

// headerKV 是一条被采集的审计 header（保留配置里的原始名与取到的值）。
type headerKV struct {
	Name  string
	Value string
}

type auditHeadersKey struct{}

// withAuditHeaderValues 把本次请求采集到的审计 header 值注入 ctx。
func withAuditHeaderValues(ctx context.Context, kvs []headerKV) context.Context {
	return context.WithValue(ctx, auditHeadersKey{}, kvs)
}

// auditHeaderValuesFromCtx 取出本次请求采集到的审计 header 值（按配置顺序）。
func auditHeaderValuesFromCtx(ctx context.Context) []headerKV {
	kvs, _ := ctx.Value(auditHeadersKey{}).([]headerKV)
	return kvs
}

// HTTPAuditHeaders 是一个纯审计用的 HTTP 中间件：按配置的 header 名单从请求头取值，
// 注入 ctx，供 LoggingMiddleware 写入 MCP 审计日志。与鉴权无关，不影响放行逻辑。
// 名单为空时不做任何事，零开销透传。
func HTTPAuditHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		names := auditHeaders()
		if len(names) > 0 {
			kvs := make([]headerKV, 0, len(names))
			for _, n := range names {
				kvs = append(kvs, headerKV{Name: n, Value: r.Header.Get(n)})
			}
			r = r.WithContext(withAuditHeaderValues(r.Context(), kvs))
		}
		next.ServeHTTP(w, r)
	})
}

// auditHeaderFields 把本次请求采集到的审计 header 转成审计日志字段。
// 字段名为 header 名的小写形式，缺失或空值记为 "-"。名单为空时返回 nil。
func auditHeaderFields(ctx context.Context) []Field {
	kvs := auditHeaderValuesFromCtx(ctx)
	if len(kvs) == 0 {
		return nil
	}
	fields := make([]Field, 0, len(kvs))
	for _, kv := range kvs {
		v := kv.Value
		if v == "" {
			v = "-"
		}
		fields = append(fields, String(strings.ToLower(kv.Name), v))
	}
	return fields
}
