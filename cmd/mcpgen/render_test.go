package main

import (
	"go/format"
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
		// 成功路径直接返回 structured 结果（spill 已移出基座，改由插件承接）
		`return nil, map[string]any{"result": out}, nil`,
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
		"Label src.Label",
		// 多返回打包成 map，按返回变量名做 key
		`"total": r0`,
		`"ready": r1`,
		// 多返回值：直接返回按返回变量名打包的 map
		`return nil, map[string]any{"total": r0, "ready": r1}, nil`,
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

func TestRender_Labels(t *testing.T) {
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
		// labels 按 key 字典序渲染（testdata 里刻意按非字典序书写），保证生成结果稳定
		`opts.Allow("case_risk", map[string]string{"capability": "write", "owner": "core", "risk": "high"})`,
		`Labels: map[string]string{"capability": "write", "owner": "core", "risk": "high"}`,
		// 无 mcp:labels 的工具渲染成 nil
		`opts.Allow("case_risk", nil)`,
		`Labels: nil`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// risk / capability 不再是基座一等公民，生成代码不得再引用旧元数据 API
	for _, unwanted := range []string{"RegisterMeta", "runtime.RiskHigh", "runtime.ReadWrite", "MaybeSpill"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("generated code should not reference %q:\n%s", unwanted, out)
		}
	}
}

// TestLabelsLit 直接锁 labelsLit 的输出：key 必须字典序（map 迭代随机，故重复多次），
// 且值必须走 %q 转义——生成代码能否编译全靠这一点。
func TestLabelsLit(t *testing.T) {
	// 逆字典序构造，确保「按插入顺序输出」会立刻变红。
	for i := 0; i < 20; i++ {
		got := labelsLit(map[string]string{"zzz": "1", "mmm": "3", "aaa": "2"})
		want := `map[string]string{"aaa": "2", "mmm": "3", "zzz": "1"}`
		if got != want {
			t.Fatalf("iteration %d: labelsLit = %s, want %s", i, got, want)
		}
	}
	if got := labelsLit(nil); got != "nil" {
		t.Errorf("labelsLit(nil) = %s, want nil", got)
	}

	// 含引号 / 反斜杠的值必须被转义成合法 Go 源码。
	lit := labelsLit(map[string]string{"desc": `a "quoted" \ back`})
	src := "package p\n\nvar x = " + lit + "\n"
	if _, err := format.Source([]byte(src)); err != nil {
		t.Errorf("labelsLit output is not valid Go: %v\n%s", err, src)
	}
}
