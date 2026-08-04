package runtime

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// auditLogger 是 MCP 调用审计日志 logger。
// 由 SetAuditLogger 在启动阶段注入；为空时（如 stdio 模式）回退到标准 log。
var auditLogger Logger

// SetAuditLogger 注入 MCP 审计日志 logger。应在 server 启动前调用。
func SetAuditLogger(l Logger) { auditLogger = l }

// LoggingMiddleware 返回一个 receiving middleware，记录每次 MCP RPC。
//
// 若已通过 SetAuditLogger 注入 logger，则以结构化字段写入审计日志，字段包含：
//   - user：执行人（HTTP 头 X-MCP-User 透传）
//   - token_name：本次调用所用 token 的用途名（配置 [[tokens]].name），用于审计追溯接入通道
//   - tool / args：调用的工具名与入参 JSON
//   - result：返回结果 JSON（截断）
//   - cost：耗时；err / tool_error：错误信息
//
// 未注入 logger 时回退到标准 log（stdio 场景）。
func LoggingMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			start := time.Now()
			toolName, argsLit := extractToolCall(req)
			res, err := next(ctx, method, req)
			dur := time.Since(start).Round(time.Millisecond)
			logCall(ctx, method, toolName, argsLit, res, err, dur)
			return res, err
		}
	}
}

// logCall 输出一条调用审计日志。
func logCall(ctx context.Context, method, toolName, argsLit string, res mcp.Result, err error, dur time.Duration) {
	if auditLogger == nil {
		// 回退：标准 log（stdio 等无审计 logger 的场景）
		logCallStd(method, toolName, argsLit, res, err, dur)
		return
	}

	user := identityFromCtx(ctx)
	fields := []Field{
		String("method", method),
		String("user", user),
		String("token_name", tokenNameFromCtx(ctx)),
		String("tool", toolName),
		String("args", argsLit),
		String("result", resultSummary(res)),
		String("cost", dur.String()),
	}

	switch {
	case err != nil:
		auditLogger.Warning("mcp_call", append(fields, String("err", err.Error()))...)
	case isToolError(res):
		auditLogger.Warning("mcp_call", append(fields, String("tool_error", toolErrorText(res)))...)
	default:
		auditLogger.Notice("mcp_call", fields...)
	}
}

// logCallStd 是无审计 logger 时的标准 log 回退实现。
func logCallStd(method, toolName, argsLit string, res mcp.Result, err error, dur time.Duration) {
	if toolName != "" {
		log.Printf("[mcp] -> %s name=%s args=%s", method, toolName, argsLit)
	} else {
		log.Printf("[mcp] -> %s", method)
	}
	switch {
	case err != nil:
		log.Printf("[mcp] <- %s name=%s err=%v dur=%s", method, toolName, err, dur)
	case isToolError(res):
		log.Printf("[mcp] <- %s name=%s tool_error=%s dur=%s", method, toolName, toolErrorText(res), dur)
	default:
		log.Printf("[mcp] <- %s name=%s ok dur=%s", method, toolName, dur)
	}
}

// identityFromCtx 取执行人身份（X-MCP-User），缺失时返回 "-"。
func identityFromCtx(ctx context.Context) string {
	if ac, ok := AuthFromContext(ctx); ok && ac.Identity != "" {
		return ac.Identity
	}
	return "-"
}

// tokenNameFromCtx 取本次调用所用 token 的用途名（配置 [[tokens]].name），缺失时返回 "-"。
func tokenNameFromCtx(ctx context.Context) string {
	if ac, ok := AuthFromContext(ctx); ok && ac.TokenName != "" {
		return ac.TokenName
	}
	return "-"
}

// resultSummary 把工具返回结果序列化为 JSON 摘要（截断），用于审计日志。
func resultSummary(res mcp.Result) string {
	if res == nil {
		return ""
	}
	if r, ok := res.(*mcp.CallToolResult); ok && r != nil {
		b, err := json.Marshal(r.Content)
		if err != nil {
			return "(marshal result error: " + err.Error() + ")"
		}
		return truncate(string(b), 2048)
	}
	b, err := json.Marshal(res)
	if err != nil {
		return ""
	}
	return truncate(string(b), 2048)
}

// extractToolCall 仅在 method = "tools/call" 时返回 tool 名与 args 的 JSON 摘要。
func extractToolCall(req mcp.Request) (name, args string) {
	p, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok || p == nil {
		return "", ""
	}
	if len(p.Arguments) > 0 {
		args = truncate(string(p.Arguments), 1024)
	}
	return p.Name, args
}

func isToolError(res mcp.Result) bool {
	r, ok := res.(*mcp.CallToolResult)
	return ok && r != nil && r.IsError
}

func toolErrorText(res mcp.Result) string {
	r, ok := res.(*mcp.CallToolResult)
	if !ok || r == nil {
		return ""
	}
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok && tc.Text != "" {
			return truncate(tc.Text, 512)
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
