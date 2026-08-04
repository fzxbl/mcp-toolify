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
