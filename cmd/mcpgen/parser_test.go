package main

import (
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
	if !hasParam(c, "cluster") || !hasParam(c, "app") {
		t.Errorf("expected cluster + app params, got %v", c.Params)
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

func TestParseCandidates_Risk(t *testing.T) {
	cands, err := parseCandidates("./testdata/case_risk")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	byName := map[string]Candidate{}
	for _, c := range cands {
		byName[c.Name] = c
	}
	if byName["DangerOp"].Risk != "high" {
		t.Errorf("DangerOp risk = %q, want high", byName["DangerOp"].Risk)
	}
	if byName["SafeRead"].Risk != "" {
		t.Errorf("SafeRead risk = %q, want empty", byName["SafeRead"].Risk)
	}
}
