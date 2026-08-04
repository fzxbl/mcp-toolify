package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRender_Simple(t *testing.T) {
	tmp := t.TempDir()
	cands, err := parseCandidates("./testdata/case_simple")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := renderPackage(tmp, cands); err != nil {
		t.Fatalf("render: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(tmp, "*_gen.go"))
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %v", files)
	}
	data, _ := os.ReadFile(files[0])
	out := string(data)
	for _, want := range []string{
		"DO NOT EDIT",
		`Name:        "case_simple.get_thing"`,
		`type Case_simpleGetThingInput struct`,
		`Verbose bool`,
		// 成功路径统一走 MaybeSpill，实参顺序为 (toolName, raw, structured)
		`return runtime.MaybeSpill("case_simple.get_thing", out, map[string]any{"result": out})`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRender_Iface(t *testing.T) {
	tmp := t.TempDir()
	cands, err := parseCandidates("./testdata/case_iface")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := renderPackage(tmp, cands); err != nil {
		t.Fatalf("render: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(tmp, "*_gen.go"))
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %v", files)
	}
	data, _ := os.ReadFile(files[0])
	out := string(data)
	for _, want := range []string{
		// interface{} 入参触发显式半受限 schema
		"runtime.AnyInputSchema[Case_ifaceApplyPatchInput]()",
		// 显式 bind 把 label 字段类型替换为具体类型
		"Label   src.Label",
		// 多返回打包成 map，按返回变量名做 key
		`"total": r0`,
		`"ready": r1`,
		// 多返回值：MaybeSpill 的 raw 与 structured 都是按返回变量名打包的 map
		`return runtime.MaybeSpill("case_iface.stat", map[string]any{"total": r0, "ready": r1}, map[string]any{"total": r0, "ready": r1})`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Stat 无 error，不应生成 callErr 分支
	if strings.Contains(out, "func handle_case_iface_Stat") {
		idx := strings.Index(out, "func handle_case_iface_Stat")
		seg := out[idx:]
		if end := strings.Index(seg[1:], "\nfunc "); end > 0 {
			seg = seg[:end]
		}
		if strings.Contains(seg, "callErr") {
			t.Errorf("Stat handler should not reference callErr:\n%s", seg)
		}
	}
}

func TestRender_RegisterMeta(t *testing.T) {
	tmp := t.TempDir()
	cands, err := parseCandidates("./testdata/case_risk")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := renderPackage(tmp, cands); err != nil {
		t.Fatalf("render: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(tmp, "*_gen.go"))
	data, _ := os.ReadFile(files[0])
	out := string(data)
	for _, want := range []string{
		`runtime.RegisterMeta(runtime.ToolMeta{Name: "case_risk.danger_op", Capability: runtime.ReadWrite, Risk: runtime.RiskHigh})`,
		`runtime.RegisterMeta(runtime.ToolMeta{Name: "case_risk.safe_read", Capability: runtime.ReadOnly, Risk: runtime.RiskNone})`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
