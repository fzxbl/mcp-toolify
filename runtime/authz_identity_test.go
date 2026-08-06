package runtime

import (
	"reflect"
	"testing"
)

func TestAuthzIdentityHeadersDefaultAndConfigured(t *testing.T) {
	// 未配置 => 回退默认 [X-MCP-User]。
	if got := NewAuthz(AuthzConfig{}).IdentityHeaders(); !reflect.DeepEqual(got, []string{"X-MCP-User"}) {
		t.Errorf("default IdentityHeaders = %v, want [X-MCP-User]", got)
	}
	// 去空白、去空项、保序去重。
	got := NewAuthz(AuthzConfig{IdentityHeaders: []string{" X-MCP-User ", "", "X-Caller", "x-caller"}}).IdentityHeaders()
	if !reflect.DeepEqual(got, []string{"X-MCP-User", "X-Caller"}) {
		t.Errorf("normalized IdentityHeaders = %v, want [X-MCP-User X-Caller]", got)
	}
}

func TestAuthzResolveIdentityFirstNonEmpty(t *testing.T) {
	az := NewAuthz(AuthzConfig{IdentityHeaders: []string{"X-A", "X-B"}})
	// X-A 空 -> 取 X-B。
	hdr := map[string]string{"X-A": "  ", "X-B": "bob"}
	if got := az.ResolveIdentity(func(n string) string { return hdr[n] }); got != "bob" {
		t.Errorf("ResolveIdentity = %q, want %q", got, "bob")
	}
	// 都空 -> 空串。
	if got := az.ResolveIdentity(func(string) string { return "" }); got != "" {
		t.Errorf("ResolveIdentity = %q, want empty", got)
	}
	// 第一个非空即停：X-A 命中，不取 X-B。
	hdr2 := map[string]string{"X-A": "alice", "X-B": "bob"}
	if got := az.ResolveIdentity(func(n string) string { return hdr2[n] }); got != "alice" {
		t.Errorf("ResolveIdentity = %q, want %q", got, "alice")
	}
}
