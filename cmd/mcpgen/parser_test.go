package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseCandidates_Simple(t *testing.T) {
	cands, err := parseCandidates("./testdata/case_simple")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(cands))
	}
	c := cands[0]
	if c.Name != "GetThing" {
		t.Errorf("name = %q", c.Name)
	}
	if c.Description != "查询某个对象。" {
		t.Errorf("desc = %q", c.Description)
	}
	if len(c.Params) != 2 {
		t.Fatalf("params len = %d", len(c.Params))
	}
	if c.Params[0].Name != "id" || c.Params[0].Doc != "对象 ID" {
		t.Errorf("param[0] = %+v", c.Params[0])
	}
}

func TestParseCandidates_Write(t *testing.T) {
	cands, err := parseCandidates("./testdata/case_write")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1, got %d", len(cands))
	}
	c := cands[0]
	// user 参数通过 mcp:bind 绑定到 authz.User（外部包由 mcp:import 声明）。
	var userBind string
	for _, p := range c.Params {
		if p.Name == "user" {
			userBind = p.BindType
		}
	}
	if userBind != "authz.User" {
		t.Errorf("user BindType = %q, want authz.User", userBind)
	}
	if !hasParam(c, "group") || !hasParam(c, "app") {
		t.Errorf("expected group + app params, got %v", c.Params)
	}
}

func TestParseCandidates_Iface(t *testing.T) {
	cands, err := parseCandidates("./testdata/case_iface")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	byName := map[string]Candidate{}
	for _, c := range cands {
		byName[c.Name] = c
	}

	// ApplyPatch: 含 interface{} 入参，bind 表中无匹配。
	ap, ok := byName["ApplyPatch"]
	if !ok {
		t.Fatal("ApplyPatch not found")
	}
	for _, p := range ap.Params {
		if p.Name == "patch" && p.BindType != "" {
			t.Errorf("patch should not be bound, got %q", p.BindType)
		}
	}

	// FilterBy: 显式 mcp:bind=label:Label。
	fb, ok := byName["FilterBy"]
	if !ok {
		t.Fatal("FilterBy not found")
	}
	var labelBind string
	for _, p := range fb.Params {
		if p.Name == "label" {
			labelBind = p.BindType
		}
	}
	if labelBind != "src.Label" {
		t.Errorf("label BindType = %q, want src.Label", labelBind)
	}

	// Stat: 两个非 error 返回值。
	st, ok := byName["Stat"]
	if !ok {
		t.Fatal("Stat not found")
	}
	if len(st.Returns) != 2 {
		t.Fatalf("Stat returns = %d, want 2", len(st.Returns))
	}
	if st.Returns[0].Name != "total" || st.Returns[1].Name != "ready" {
		t.Errorf("Stat return names = %q,%q", st.Returns[0].Name, st.Returns[1].Name)
	}
}

func TestParseCandidates_Labels(t *testing.T) {
	cands, err := parseCandidates("./testdata/case_risk")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	byName := map[string]Candidate{}
	for _, c := range cands {
		byName[c.Name] = c
	}
	got := byName["DangerOp"].Labels
	want := map[string]string{"risk": "high", "capability": "write", "owner": "core"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DangerOp Labels = %v, want %v", got, want)
	}
	if len(byName["SafeRead"].Labels) != 0 {
		t.Errorf("SafeRead Labels = %v, want empty", byName["SafeRead"].Labels)
	}
}

// TestParseLabels_Invalid 覆盖非法 mcp:labels 写法：缺 k=v、空 key/value、重复 key、
// 空项，以及 key/value 含 selector 语法字符（selector 会拒绝，故生成期就要拦住）。
func TestParseLabels_Invalid(t *testing.T) {
	for _, raw := range []string{
		"risk",                     // 缺 =
		"=high",                    // 缺 key
		"risk=",                    // 缺 value
		"risk=a,risk=b",            // 重复 key
		"risk=a=b",                 // value 含 =
		"risk=hi!gh",               // value 含 !
		"risk=(high)",              // value 含 ()
		"capability=a,b",           // 第二项缺 k=v
		"capability=write,,risk=a", // 空项
		"capability=write,",        // 尾随逗号
		"ri sk=high",               // key 含空格
		"ri\tsk=high",              // key 含制表符
	} {
		if _, err := parseLabels(raw); err == nil {
			t.Errorf("parseLabels(%q) = nil error, want error", raw)
		}
	}
}

// TestParseLabels_Valid 合法写法：多 label、允许值含空格、空串返回 nil。
func TestParseLabels_Valid(t *testing.T) {
	got, err := parseLabels(" capability=write , risk=very high ")
	if err != nil {
		t.Fatalf("parseLabels: %v", err)
	}
	want := map[string]string{"capability": "write", "risk": "very high"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got, err := parseLabels("  "); err != nil || got != nil {
		t.Errorf("parseLabels(blank) = %v, %v; want nil, nil", got, err)
	}
}

// TestParseCandidates_MultiLineLabels 多行 mcp:labels 应合并而非后者覆盖前者；
// 同 key 写两行属真冲突，必须报错。
func TestParseCandidates_MultiLineLabels(t *testing.T) {
	cands, err := parseCandidates("./testdata/case_multiline")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(cands))
	}
	want := map[string]string{"capability": "write", "risk": "high"}
	if !reflect.DeepEqual(cands[0].Labels, want) {
		t.Errorf("Labels = %v, want %v", cands[0].Labels, want)
	}

	if _, err := parseCandidates("./testdata/case_dup_labels"); err == nil {
		t.Error("duplicate label key across lines should fail")
	} else if !strings.Contains(err.Error(), "duplicate label key") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestParseCandidates_StaleMarkers 陈旧/拼错的 mcp: 标记必须报错，不能静默丢标签。
func TestParseCandidates_StaleMarkers(t *testing.T) {
	cases := []struct {
		dir      string
		wantPart string
	}{
		{"./testdata/case_stale_tags", "mcp:tags is removed, use mcp:labels=capability=write"},
		{"./testdata/case_stale_risk", "mcp:risk is removed, use mcp:labels=risk=high"},
		{"./testdata/case_typo_marker", "unknown marker mcp:label"},
		{"./testdata/case_empty_labels", "empty mcp:labels"},
	}
	for _, c := range cases {
		_, err := parseCandidates(c.dir)
		if err == nil {
			t.Errorf("%s: want error, got nil", c.dir)
			continue
		}
		if !strings.Contains(err.Error(), c.wantPart) {
			t.Errorf("%s: error = %v, want to contain %q", c.dir, err, c.wantPart)
		}
		// 错误信息应带上可点击的 file:line
		if !strings.Contains(err.Error(), "foo.go:") {
			t.Errorf("%s: error should carry file:line, got %v", c.dir, err)
		}
	}
}
