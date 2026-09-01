package runtime

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSetMetaInitializesNilMap(t *testing.T) {
	c := &Call{Method: "tools/call", Tool: "a.b"}
	if c.Meta != nil {
		t.Fatal("Meta should start nil")
	}
	c.SetMeta("k", 1)
	c.SetMeta("k2", "v")
	if c.Meta == nil {
		t.Fatal("SetMeta did not initialize Meta")
	}
	if c.Meta["k"] != 1 || c.Meta["k2"] != "v" {
		t.Errorf("Meta = %v", c.Meta)
	}
	// 再写同一个 key 应覆盖，而不是新建 map 丢掉旧值。
	c.SetMeta("k", 2)
	if c.Meta["k"] != 2 || c.Meta["k2"] != "v" {
		t.Errorf("Meta after overwrite = %v", c.Meta)
	}
}

func TestTextResult(t *testing.T) {
	r := TextResult("hello")
	if r.Tool == nil {
		t.Fatal("Tool result is nil")
	}
	if r.List != nil {
		t.Error("List should stay nil")
	}
	if r.Tool.IsError {
		t.Error("IsError = true, want false")
	}
	if len(r.Tool.Content) != 1 {
		t.Fatalf("Content len = %d, want 1", len(r.Tool.Content))
	}
	tc, ok := r.Tool.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] type = %T, want *mcp.TextContent", r.Tool.Content[0])
	}
	if tc.Text != "hello" {
		t.Errorf("Text = %q, want hello", tc.Text)
	}
}

func TestListResult(t *testing.T) {
	list := &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: "a.b"}}}
	r := ListResult(list)
	if r.List != list {
		t.Errorf("List = %v, want %v", r.List, list)
	}
	if r.Tool != nil {
		t.Error("Tool should stay nil")
	}
	if r.rawOf() != nil {
		t.Error("raw should stay nil")
	}
}

func TestDenyResult(t *testing.T) {
	c := &Call{Method: "tools/call", Tool: "orders.create"}
	r := DenyResult(c, "authz", "risk high requires approval")
	if r.Tool == nil {
		t.Fatal("Tool result is nil")
	}
	if !r.Tool.IsError {
		t.Error("IsError = false, want true")
	}
	tc, ok := r.Tool.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] type = %T, want *mcp.TextContent", r.Tool.Content[0])
	}
	if want := "permission denied: risk high requires approval"; tc.Text != want {
		t.Errorf("Text = %q, want %q", tc.Text, want)
	}
	if c.Meta["denied_by"] != "authz" {
		t.Errorf("denied_by = %v, want authz", c.Meta["denied_by"])
	}
	if c.Meta["deny_reason"] != "risk high requires approval" {
		t.Errorf("deny_reason = %v", c.Meta["deny_reason"])
	}
}

// 拒绝发生在 Meta 尚未初始化时也不能 panic。
func TestDenyResultNilMeta(t *testing.T) {
	c := &Call{Method: "tools/call"}
	DenyResult(c, "authz", "nope")
	if len(c.Meta) != 2 {
		t.Errorf("Meta = %v, want 2 keys", c.Meta)
	}
}

func TestRawResultPassthrough(t *testing.T) {
	if got := (*Result)(nil).rawOf(); got != nil {
		t.Errorf("nil Result rawOf() = %v, want nil", got)
	}
	if got := TextResult("x").rawOf(); got != nil {
		t.Errorf("TextResult rawOf() = %v, want nil", got)
	}
	want := &mcp.ListResourcesResult{}
	r := rawResult(want)
	if r.Tool != nil || r.List != nil {
		t.Error("rawResult should not populate Tool/List")
	}
	if r.rawOf() != mcp.Result(want) {
		t.Errorf("rawOf() = %v, want %v", r.rawOf(), want)
	}
}
