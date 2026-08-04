package spillexplore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sysprobe.submit-x.txt")
	os.WriteFile(p, []byte("l1\nl2\nl3\nl4\n"), 0644)
	out, err := readLines(p, 1, 2, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "l2\nl3\n" || out.NextLineOffset != 3 || out.EOF {
		t.Fatalf("got %+v", out)
	}
}

func TestGrepLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t-x.txt")
	os.WriteFile(p, []byte("error a\nok b\nerror c\n"), 0644)
	lines, err := grepLines(p, "error", 100, 1<<20)
	if err != nil || len(lines) != 2 || lines[0] != "1:error a" {
		t.Fatalf("got %v err=%v", lines, err)
	}
}

func TestJQOverJSONL(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t-x.jsonl")
	os.WriteFile(p, []byte(`{"n":1}`+"\n"+`{"n":2}`+"\n"), 0644)
	res, err := runJQ(p, ".n", true)
	if err != nil || len(res) != 2 || res[0] != "1" {
		t.Fatalf("got %v err=%v", res, err)
	}
}

func TestSchemaDepthExpansion(t *testing.T) {
	// status.detail.count 三层嵌套：depth=1 只应看到 status 的类型（object 折叠），
	// depth=3 应能一路展开到 detail 里的标量类型。
	v := map[string]any{
		"status": map[string]any{
			"detail": map[string]any{"count": float64(3)},
		},
		"name": "svc",
	}

	// depth=1：顶层 key 的值折叠为类型名
	d1 := describe(v, 1)
	m1 := d1.(map[string]any)
	if m1["name"] != "string" || m1["status"] != "object" {
		t.Fatalf("depth=1 got %+v", m1)
	}

	// depth=3：展开到 status.detail.count 的标量类型
	d3 := describe(v, 3)
	m3 := d3.(map[string]any)
	status := m3["status"].(map[string]any)
	detail := status["detail"].(map[string]any)
	if detail["count"] != "number" {
		t.Fatalf("depth=3 got %+v", m3)
	}
}
