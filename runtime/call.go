package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Subject 是调用主体。基座只负责填充，不解释 Labels。
type Subject struct {
	ID string // 人（身份头解析所得）
	// Token 必须填 token 的**用途名**（配置 [[tokens]].name），绝不是 token 值：
	// 填了 token 值既查不到准入规则（全部请求被拒），又会把密文带进日志。
	Token  string
	Labels map[string]string // 主体自身标注，插件自定义
}

// Call 是一次 MCP 请求的上下文。插件通过它读写调用相关信息。
type Call struct {
	Method string            // tools/list | tools/call | ...
	Tool   string            // tools/call 时的工具名
	Labels map[string]string // 该工具的完整 labels（含 name/pkg 投影）
	// Args 是原始入参。**插件改它会真的生效**：链的执行终点在调用下游之前把
	// Call.Tool / Call.Args 回写进合成的请求（见 runtime/chain.go 的 terminus）。
	Args    json.RawMessage
	Headers http.Header // 请求头，插件不得修改；快照语义由注入侧保证
	Subject *Subject    // 调用主体
	LogID   string
	Meta    map[string]any    // 插件间通信通道，基座不认识任何 key
	Tools   func() []ToolInfo // 查全部工具，tools/list 过滤用；由基座在构造 Call 时填充，插件调用前判 nil
}

// Result 是一次调用的结果。tools/call 用 Tool 字段，tools/list 用 List 字段，
// 其他 MCP 方法用 raw 直通（否则会被吞成 nil）。
// 读取侧按 Tool → List → raw 取第一个非 nil；三者同时非 nil 属调用方错误，行为未定义。
// 三者同时为 nil（如插件返回 &Result{}）时 unwrapResult 返回 nil 接口，等同于没有结果。
type Result struct {
	Tool *mcp.CallToolResult
	List *mcp.ListToolsResult
	raw  mcp.Result
}

// Handler 是链上的一环。
type Handler func(ctx context.Context, c *Call) (*Result, error)

// Middleware 是唯一的插件注入点，形状与 net/http 中间件一致。
type Middleware func(next Handler) Handler

// Meta 里由 DenyResult 写入的固定 key，审计插件读这两个 key 记录拒绝详情。
//
// 纪律：凡跨插件读取的 key 必须在此处声明常量；
// 插件私有 key 用「插件名.」前缀避免撞名。
const (
	MetaDeniedBy   = "denied_by"
	MetaDenyReason = "deny_reason"
)

// SetMeta 往 Meta 写值，Meta 为 nil 时自动初始化。
func (c *Call) SetMeta(key string, val any) {
	if c.Meta == nil {
		c.Meta = map[string]any{}
	}
	c.Meta[key] = val
}

type subjectKey struct{}

// WithSubject 把认证阶段解析出的调用主体注入 ctx，是 newCall 填充 Call.Subject 的
// 唯一数据来源。由 HTTP 层（TokenAuthz.HTTPMiddleware）在进入 MCP handler 前调用。
func WithSubject(ctx context.Context, s *Subject) context.Context {
	return context.WithValue(ctx, subjectKey{}, s)
}

// SubjectFromContext 取出调用主体；未注入时返回 nil，调用方按「无身份」处理。
func SubjectFromContext(ctx context.Context) *Subject {
	s, _ := ctx.Value(subjectKey{}).(*Subject)
	return s
}

// TextResult 构造一个纯文本的 tools/call 结果，供插件短路返回使用。
func TextResult(text string) *Result {
	return &Result{Tool: &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}}
}

// ListResult 构造一个 tools/list 结果，供插件过滤工具清单后返回。
func ListResult(l *mcp.ListToolsResult) *Result { return &Result{List: l} }

// DenyResult 构造一个错误态结果，并在 Meta 里记录拒绝方与原因供审计读取。
// 结果按 MCP 约定用 IsError 表达业务级拒绝，而不是协议级错误，
// 这样模型能看到被拒的原因并自我纠正。
func DenyResult(c *Call, by, reason string) *Result {
	c.SetMeta(MetaDeniedBy, by)
	c.SetMeta(MetaDenyReason, reason)
	return &Result{Tool: &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf("permission denied: %s", reason),
		}},
	}}
}

// rawResult 包装一个非 tools/call、非 tools/list 的 MCP 结果直通链路，
// 由链适配处理 tools/call、tools/list 之外的 MCP 方法时使用。
func rawResult(r mcp.Result) *Result { return &Result{raw: r} }

// rawOf 取出直通结果，未设置时返回 nil。
func (r *Result) rawOf() mcp.Result {
	if r == nil {
		return nil
	}
	return r.raw
}
