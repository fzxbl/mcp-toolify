package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestSpillResultWritesDiskAndReadBack 验证 SpillResult 会把结果落到磁盘，
// 且返回的 spill:// URI 能通过 store 定位到实际文件；数组类型应落成 .jsonl。
func TestSpillResultWritesDiskAndReadBack(t *testing.T) {
	dir := t.TempDir()
	// 直接注入 store，绕过 sync.Once，避免测试间相互影响
	setSpillStoreForTest(newDiskSpillStore(dir, spillTTLConfig{Generic: time.Hour, Sysprobe: time.Hour, GC: time.Hour}))
	res, err := SpillResult("mytool", []map[string]int{{"a": 1}, {"b": 2}})
	if err != nil {
		t.Fatal(err)
	}
	var uri string
	for _, c := range res.Content {
		if link, ok := c.(*mcp.ResourceLink); ok {
			uri = link.URI
		}
	}
	if !strings.HasPrefix(uri, "spill://") {
		t.Fatalf("no spill uri: %q", uri)
	}
	id := strings.TrimPrefix(uri, "spill://")
	p, ok := spillStoreOrDefault().resolve(id)
	if !ok || !strings.HasSuffix(p, ".jsonl") {
		t.Fatalf("expected jsonl file, got %q ok=%v", p, ok)
	}
}

// newTestSpillStore 注入临时目录 store，返回该目录；测试结束后恢复原 store，
// 避免 globalSpillStore 长期指向已被清理的 t.TempDir()。
func newTestSpillStore(t *testing.T) string {
	t.Helper()
	old := globalSpillStore
	t.Cleanup(func() { setSpillStoreForTest(old) })
	dir := t.TempDir()
	setSpillStoreForTest(newDiskSpillStore(dir, spillTTLConfig{Generic: time.Hour, Sysprobe: time.Hour, GC: time.Hour}))
	return dir
}

// countFiles 返回目录下的文件个数。
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// TestMaybeSpillSmallResultInline 小结果直接返回 structuredContent，不落盘。
func TestMaybeSpillSmallResultInline(t *testing.T) {
	dir := newTestSpillStore(t)
	SetSpillThreshold(defaultMaxResultTokens)
	t.Cleanup(func() { SetSpillThreshold(defaultMaxResultTokens) })

	out := []string{"a", "b"}
	res, structured, err := MaybeSpill("mytool", out, map[string]any{"result": out})
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Errorf("res = %+v, want nil", res)
	}
	m, ok := structured.(map[string]any)
	if !ok || m["result"] == nil {
		t.Errorf("structured = %#v, want map with result", structured)
	}
	if n := countFiles(t, dir); n != 0 {
		t.Errorf("spill dir has %d files, want 0", n)
	}
}

// TestMaybeSpillWriteFailureFallsBackToInline 落盘失败时降级为内联返回，不让工具调用失败。
// 造错方式：把 store 目录指向一个「实际是普通文件」的路径，MkdirAll 与 WriteFile 必然失败，
// 对 root 用户同样稳定（不依赖权限位）。
func TestMaybeSpillWriteFailureFallsBackToInline(t *testing.T) {
	old := globalSpillStore
	t.Cleanup(func() { setSpillStoreForTest(old) })
	notADir := filepath.Join(t.TempDir(), "file-not-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	setSpillStoreForTest(newDiskSpillStore(notADir, spillTTLConfig{Generic: time.Hour, Sysprobe: time.Hour, GC: time.Hour}))
	SetSpillThreshold(1)
	t.Cleanup(func() { SetSpillThreshold(defaultMaxResultTokens) })

	out := []string{"aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb"}
	res, structured, err := MaybeSpill("mytool", out, map[string]any{"result": out})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if res != nil || structured == nil {
		t.Errorf("res = %+v structured nil = %v, want inline result", res, structured == nil)
	}
}

// TestMaybeSpillLargeResultSpills 超阈值时落盘并返回 ResourceLink，structured 为 nil。
func TestMaybeSpillLargeResultSpills(t *testing.T) {
	newTestSpillStore(t)
	SetSpillThreshold(10)
	t.Cleanup(func() { SetSpillThreshold(defaultMaxResultTokens) })

	out := []string{"aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccc"}
	res, structured, err := MaybeSpill("mytool", out, map[string]any{"result": out})
	if err != nil {
		t.Fatal(err)
	}
	if structured != nil {
		t.Errorf("structured = %#v, want nil", structured)
	}
	var uri string
	for _, c := range res.Content {
		if link, ok := c.(*mcp.ResourceLink); ok {
			uri = link.URI
		}
	}
	id := strings.TrimPrefix(uri, "spill://")
	p, ok := spillStoreOrDefault().resolve(id)
	if !ok || !strings.HasSuffix(p, ".jsonl") {
		t.Fatalf("expected jsonl spill file, got %q ok=%v", p, ok)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(data), "\n"); lines != 3 {
		t.Errorf("spill file has %d lines, want 3", lines)
	}
}

// TestMaybeSpillDisabled 阈值为负时永不落盘。
func TestMaybeSpillDisabled(t *testing.T) {
	dir := newTestSpillStore(t)
	SetSpillThreshold(-1)
	t.Cleanup(func() { SetSpillThreshold(defaultMaxResultTokens) })

	big := make([]string, 1000)
	for i := range big {
		big[i] = "0123456789012345678901234567890123456789"
	}
	res, structured, err := MaybeSpill("mytool", big, map[string]any{"result": big})
	if err != nil {
		t.Fatal(err)
	}
	if res != nil || structured == nil {
		t.Errorf("res = %+v structured nil = %v, want inline result", res, structured == nil)
	}
	if n := countFiles(t, dir); n != 0 {
		t.Errorf("spill dir has %d files, want 0", n)
	}
}
