package runtime

import "testing"

// TestAllowEnableFilter 包名白名单过滤：空表示不过滤。
func TestAllowEnableFilter(t *testing.T) {
	o := RegisterOptions{Enable: []string{"greeter"}}
	if !o.Allow("greeter", nil) {
		t.Error("greeter should be allowed")
	}
	if o.Allow("orders", nil) {
		t.Error("orders should be filtered out")
	}
	if !(RegisterOptions{}).Allow("orders", nil) {
		t.Error("empty options should allow everything")
	}
	// 纯空白的 Match 视同未配置：多打一个空格不能把「全放行」翻成「全拒绝」。
	if !(RegisterOptions{Match: "  "}).Allow("orders", nil) {
		t.Error("blank Match should be treated as no filter")
	}
}

// TestAllowMatchFilter label selector 过滤。
func TestAllowMatchFilter(t *testing.T) {
	labels := map[string]string{"capability": "write", "risk": "high"}
	cases := []struct {
		match string
		want  bool
	}{
		{"capability=write", true},
		{"capability=read", false},
		{"risk in (high,medium)", true},
		{"risk notin (high)", false},
		{"capability=write,risk=high", true},
		{"capability=write,risk=low", false},
		{"risk", true},   // Exists
		{"!risk", false}, // DoesNotExist
		{"!owner", true},
	}
	for _, c := range cases {
		got := RegisterOptions{Match: c.match}.Allow("orders", labels)
		if got != c.want {
			t.Errorf("Allow(match=%q) = %v, want %v", c.match, got, c.want)
		}
	}
}

// TestAllowMatchInjectsPkg 基座把内置 label pkg 注入 label 集合供 selector 匹配。
func TestAllowMatchInjectsPkg(t *testing.T) {
	if !(RegisterOptions{Match: "pkg=orders"}).Allow("orders", nil) {
		t.Error("pkg label should be matchable")
	}
	if (RegisterOptions{Match: "pkg=greeter"}).Allow("orders", nil) {
		t.Error("pkg=greeter should not match orders")
	}
	// 工具自带的 pkg label 不得覆盖基座注入的真实包名。
	if (RegisterOptions{Match: "pkg=fake"}).Allow("orders", map[string]string{"pkg": "fake"}) {
		t.Error("tool-provided pkg label must not override the real package name")
	}
}

// TestAllowMatchSyntaxErrorFailsClosed selector 语法错误时 fail-closed，不注册任何工具。
func TestAllowMatchSyntaxErrorFailsClosed(t *testing.T) {
	for _, bad := range []string{"risk==high", "risk in high", "!=high", "risk notin ("} {
		if (RegisterOptions{Match: bad}).Allow("orders", map[string]string{"risk": "high"}) {
			t.Errorf("Allow(match=%q) = true, want false (fail-closed)", bad)
		}
	}
}
