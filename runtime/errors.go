// Package runtime provides the hand-written runtime substrate for the
// auto-generated MCP tool registrations under mcp/tools.
package runtime

import "github.com/modelcontextprotocol/go-sdk/mcp"

// ToolError converts a Go error into an IsError CallToolResult so callers can
// distinguish tool failures from protocol-level errors.
func ToolError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}

// ToolText 构造一个成功态的纯文本工具结果，供插件自注册的 MCP 工具使用
// （TextResult 是链上的 *Result 形式，这里要的是 handler 直接返回的 SDK 结果）。
func ToolText(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}
