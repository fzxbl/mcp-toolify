package runtime

import (
	"strconv"
	"sync"
	"testing"
)

func TestRegisterAndLookupTool(t *testing.T) {
	ResetToolsForTest()
	RegisterTool(ToolInfo{Name: "orders.create", Pkg: "orders",
		Labels: map[string]string{"capability": "write", "risk": "high"}})
	info, ok := LookupTool("orders.create")
	if !ok {
		t.Fatal("tool not found")
	}
	if info.Labels["risk"] != "high" {
		t.Errorf("labels = %v", info.Labels)
	}
	if all := Tools(); len(all) != 1 {
		t.Errorf("Tools() len = %d, want 1", len(all))
	}
}

// 工具自带的 name/pkg label 不得覆盖真实的工具名与包名，
// 否则插件可以靠伪造 label 绕过 selector 匹配。
func TestAllLabelsBuiltinProjectionWins(t *testing.T) {
	info := ToolInfo{Name: "orders.create", Pkg: "orders",
		Labels: map[string]string{"name": "fake.tool", "pkg": "fakepkg", "risk": "high"}}
	got := info.AllLabels()
	if got["name"] != "orders.create" {
		t.Errorf("name = %q, want orders.create", got["name"])
	}
	if got["pkg"] != "orders" {
		t.Errorf("pkg = %q, want orders", got["pkg"])
	}
	if got["risk"] != "high" {
		t.Errorf("risk = %q, want high", got["risk"])
	}
	// AllLabels 必须是拷贝，改返回值不能污染原始 Labels。
	got["risk"] = "none"
	if info.Labels["risk"] != "high" {
		t.Errorf("AllLabels leaked into ToolInfo.Labels: %v", info.Labels)
	}
}

// 注册表必须与调用方的 map 解耦：注册后改传入的 map 不能影响已登记的元数据。
func TestRegisterToolCopiesLabels(t *testing.T) {
	ResetToolsForTest()
	labels := map[string]string{"risk": "high"}
	RegisterTool(ToolInfo{Name: "a.b", Pkg: "a", Labels: labels})
	labels["risk"] = "none"
	info, ok := LookupTool("a.b")
	if !ok {
		t.Fatal("tool not found")
	}
	if info.Labels["risk"] != "high" {
		t.Errorf("risk = %q, want high (registry aliased caller map)", info.Labels["risk"])
	}
}

func TestRegisterToolOverwritesSameName(t *testing.T) {
	ResetToolsForTest()
	RegisterTool(ToolInfo{Name: "a.b", Pkg: "a", Labels: map[string]string{"risk": "low"}})
	RegisterTool(ToolInfo{Name: "a.b", Pkg: "a", Labels: map[string]string{"risk": "high"}})
	if all := Tools(); len(all) != 1 {
		t.Fatalf("Tools() len = %d, want 1", len(all))
	}
	info, _ := LookupTool("a.b")
	if info.Labels["risk"] != "high" {
		t.Errorf("risk = %q, want high", info.Labels["risk"])
	}
}

func TestLookupToolMissing(t *testing.T) {
	ResetToolsForTest()
	if _, ok := LookupTool("nope"); ok {
		t.Error("LookupTool(nope) ok = true, want false")
	}
	if all := Tools(); len(all) != 0 {
		t.Errorf("Tools() len = %d, want 0", len(all))
	}
}

// 空 Labels 的工具也要能安全取 AllLabels（不能 panic，且带上投影）。
func TestAllLabelsNilLabels(t *testing.T) {
	got := ToolInfo{Name: "a.b", Pkg: "a"}.AllLabels()
	if len(got) != 2 || got["name"] != "a.b" || got["pkg"] != "a" {
		t.Errorf("AllLabels() = %v", got)
	}
}

// 出口也必须封死：改 LookupTool 返回的 Labels 不能污染注册表，
// 否则任何插件都能永久改写 authz 的判据。
func TestLookupToolReturnsLabelCopy(t *testing.T) {
	ResetToolsForTest()
	RegisterTool(ToolInfo{Name: "a.b", Pkg: "a", Labels: map[string]string{"risk": "high"}})
	got, _ := LookupTool("a.b")
	got.Labels["risk"] = "none"
	got.Labels["injected"] = "yes"
	again, _ := LookupTool("a.b")
	if again.Labels["risk"] != "high" {
		t.Errorf("risk = %q, want high (registry mutated via LookupTool)", again.Labels["risk"])
	}
	if _, ok := again.Labels["injected"]; ok {
		t.Error("caller injected a label into the registry via LookupTool")
	}
}

func TestToolsReturnsLabelCopy(t *testing.T) {
	ResetToolsForTest()
	RegisterTool(ToolInfo{Name: "a.b", Pkg: "a", Labels: map[string]string{"risk": "high"}})
	all := Tools()
	if len(all) != 1 {
		t.Fatalf("Tools() len = %d, want 1", len(all))
	}
	all[0].Labels["risk"] = "none"
	again, _ := LookupTool("a.b")
	if again.Labels["risk"] != "high" {
		t.Errorf("risk = %q, want high (registry mutated via Tools)", again.Labels["risk"])
	}
}

// 并发下读写混跑：一组 goroutine 反复 LookupTool 并写回返回的 Labels，
// 一组反复 Tools 并写返回值，一组反复 RegisterTool。
// 只要出口不深拷贝，-race 下必然报 concurrent map read and map write。
func TestToolRegistryConcurrentAccess(t *testing.T) {
	ResetToolsForTest()
	const names = 4
	for i := 0; i < names; i++ {
		RegisterTool(ToolInfo{Name: "pkg.t" + strconv.Itoa(i), Pkg: "pkg",
			Labels: map[string]string{"risk": "high", "capability": "write"}})
	}

	const iterations = 200
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			name := "pkg.t" + strconv.Itoa(g%names)
			for i := 0; i < iterations; i++ {
				if info, ok := LookupTool(name); ok {
					info.Labels["risk"] = "none"
					_ = info.AllLabels()
				}
			}
		}(g)
	}
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				for _, info := range Tools() {
					if info.Labels != nil {
						info.Labels["capability"] = "read"
					}
				}
			}
		}()
	}
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				RegisterTool(ToolInfo{Name: "pkg.t" + strconv.Itoa(g%names), Pkg: "pkg",
					Labels: map[string]string{"risk": "high", "capability": "write"}})
			}
		}(g)
	}
	wg.Wait()

	// 全程被并发写返回值，注册表里的判据仍须原封不动。
	for i := 0; i < names; i++ {
		info, ok := LookupTool("pkg.t" + strconv.Itoa(i))
		if !ok {
			t.Fatalf("pkg.t%d missing", i)
		}
		if info.Labels["risk"] != "high" || info.Labels["capability"] != "write" {
			t.Errorf("pkg.t%d labels = %v, want risk=high capability=write", i, info.Labels)
		}
	}
}
