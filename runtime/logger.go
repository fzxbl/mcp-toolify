package runtime

// Field is a single structured field in an audit log record.
type Field struct {
	Key   string
	Value string
}

// String builds a string-valued audit field.
func String(key, val string) Field { return Field{Key: key, Value: val} }

// Logger is the audit sink for MCP tool calls. Implementations receive a short
// message tag (always "mcp_call") plus structured fields (user, tool, args,
// result, cost, ...). Notice is used for successful calls, Warning for calls
// that returned an error or an IsError tool result.
//
// Injecting a Logger is optional: when none is set (see SetAuditLogger), the
// runtime falls back to the standard library log package. This keeps the
// framework free of any specific logging dependency.
type Logger interface {
	Notice(msg string, fields ...Field)
	Warning(msg string, fields ...Field)
}
