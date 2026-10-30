package main

import (
	"bytes"
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
		`"case_risk", map[string]string{"capability": "write", "owner": "core", "risk": "high"})`,
		// 无 mcp:labels 的工具渲染成 nil
		`opts.Allow("case_risk", nil)`,
		`"case_risk", nil)`,
		// 加到 server 与登记 labels 走同一个入口，不再分两步
		`runtime.AddTool(s, &mcp.Tool{`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// risk / capability 不再是基座一等公民，生成代码不得再引用旧元数据 API
	for _, unwanted := range []string{"RegisterMeta", "runtime.RiskHigh", "runtime.ReadWrite", "MaybeSpill",
		"mcp.AddTool(", "RegisterTool", "func register_"} {
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

// TestRender_FunctionOnlyOutputUnchanged 只有包级函数的包，生成产物必须与
// 改动前逐字节一致（golden 由 ctx/方法支持之前的生成器产出）。
func TestRender_FunctionOnlyOutputUnchanged(t *testing.T) {
	tmp := t.TempDir()
	cands, err := parseCandidates("./testdata/case_simple")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := renderPackage(tmp, cands); err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := renderAll(tmp, []pkgInfo{newPkgInfo(cands)}); err != nil {
		t.Fatalf("renderAll: %v", err)
	}
	for gen, golden := range map[string]string{
		"case_simple_gen.go": "case_simple_gen.go.golden",
		"all_gen.go":         "all_gen.go.golden",
	} {
		got, err := os.ReadFile(filepath.Join(tmp, gen))
		if err != nil {
			t.Fatalf("read %s: %v", gen, err)
		}
		want, err := os.ReadFile(filepath.Join("testdata", "golden", golden))
		if err != nil {
			t.Fatalf("read golden %s: %v", golden, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s 与 golden 不一致（函数-only 输出必须逐字节不变）\n--- got ---\n%s\n--- want ---\n%s", gen, got, want)
		}
	}
}

func TestRender_Ctx(t *testing.T) {
	tmp := t.TempDir()
	cands, err := parseCandidates("./testdata/case_ctx")
	if err != nil {
		t.Fatal(err)
	}
	if err := renderPackage(tmp, cands); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(tmp, "*_gen.go"))
	data, _ := os.ReadFile(files[0])
	out := string(data)
	for _, want := range []string{
		"src.Lookup(ctx, in.Id)",
		"src.Ping(ctx)",
		"src.Pair(ctx, in.X)",
		"src.Blank(ctx, in.Y)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Ctx ") {
		t.Errorf("input 结构体不应含 Ctx 字段:\n%s", out)
	}
}

func TestRender_Method(t *testing.T) {
	tmp := t.TempDir()
	cands, err := parseCandidates("./testdata/case_method")
	if err != nil {
		t.Fatal(err)
	}
	if err := renderPackage(tmp, cands); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(tmp, "*_gen.go"))
	data, _ := os.ReadFile(files[0])
	out := string(data)
	for _, want := range []string{
		"func handle_case_method_Orders_Get(impl *src.Orders)",
		"impl.Get(ctx, in.Id)",
		"func Register_Case_method(s *mcp.Server, opts runtime.RegisterOptions, implCounter *src.Counter, implOrders *src.Orders, implUsers *src.Users)",
		"handle_case_method_Orders_Get(implOrders)",
		"type Case_method_Orders_GetInput struct",
		"type Case_methodOrdersGetInput struct", // 包级函数 OrdersGet，不得与方法撞名
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "(0).") {
		t.Errorf("不应再生成 T(0).Method 调用:\n%s", out)
	}
}

func TestPackageRecvs(t *testing.T) {
	cands, err := parseCandidates("./testdata/case_method")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(packageRecvs(cands), ","), "Counter,Orders,Users"; got != want {
		t.Errorf("packageRecvs = %q, want %q", got, want)
	}
	rt := recvTools(cands)
	if got, want := strings.Join(rt["Orders"], ","), "case_method.get,case_method.orders_list"; got != want {
		t.Errorf("recvTools[Orders] = %q, want %q", got, want)
	}
	if _, ok := rt[""]; ok {
		t.Errorf("recvTools 不应包含包级函数")
	}
}

func TestRenderAll_Impls(t *testing.T) {
	var pkgs []pkgInfo
	for _, d := range []string{"./testdata/case_simple", "./testdata/case_method"} {
		cands, err := parseCandidates(d)
		if err != nil {
			t.Fatalf("parse %s: %v", d, err)
		}
		pkgs = append(pkgs, newPkgInfo(cands))
	}
	tmp := t.TempDir()
	if err := renderAll(tmp, pkgs); err != nil {
		t.Fatalf("renderAll: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(tmp, "all_gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	// gofmt 会对齐字段，先折叠空白再断言。
	got := strings.Join(strings.Fields(string(b)), " ")
	for _, want := range []string{
		"Case_method_Orders *src_case_method.Orders",
		"func NewRegistrar(impls Impls) (runtime.Registrar, error)",
		"tools.Impls.Case_method_Orders 为 nil：工具 case_method.get, case_method.orders_list 需要它",
		`errors.New("tools.Impls.Case_method_Users 为 nil：工具 case_method.users_list%v 需要它")`,
		"Register_Case_simple(s, opts)",
		"Register_Case_method(s, opts, impls.Case_method_Counter, impls.Case_method_Orders, impls.Case_method_Users)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("all_gen.go 缺少 %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "func RegisterAll") {
		t.Errorf("含方法工具时不应生成 RegisterAll\n%s", got)
	}
}
