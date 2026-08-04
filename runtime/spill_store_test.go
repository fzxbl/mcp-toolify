package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiskSpillCreateAndResolve(t *testing.T) {
	dir := t.TempDir()
	st := newDiskSpillStore(dir, spillTTLConfig{Generic: time.Hour, Sysprobe: time.Hour, GC: time.Hour})
	id, path := st.create("mytool", FormatJSON)
	if err := os.WriteFile(path, []byte(`{"a":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "mytool-"+id+".json" {
		t.Fatalf("unexpected filename %s", path)
	}
	got, ok := st.resolve(id)
	if !ok || got != path {
		t.Fatalf("resolve failed: %q %v", got, ok)
	}
}

func TestSpillFormatOf(t *testing.T) {
	type row struct{ A int }
	cases := []struct {
		in   any
		want SpillFormat
	}{
		{[]row{{1}}, FormatJSONL},
		{&[]row{{1}}, FormatJSONL},
		{[]byte("x"), FormatJSON},
		{map[string]int{"a": 1}, FormatJSON},
		{"s", FormatJSON},
	}
	for _, c := range cases {
		if got := spillFormatOf(c.in); got != c.want {
			t.Fatalf("in=%T got=%s want=%s", c.in, got, c.want)
		}
	}
}

func TestGCRemovesExpired(t *testing.T) {
	dir := t.TempDir()
	st := newDiskSpillStore(dir, spillTTLConfig{Generic: time.Millisecond, Sysprobe: time.Millisecond, GC: time.Hour})
	_, path := st.create("t", FormatJSON)
	os.WriteFile(path, []byte("{}"), 0644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)
	st.gcOnce()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file removed")
	}
}

func TestResolvePrefersMainOverWfSibling(t *testing.T) {
	dir := t.TempDir()
	st := newDiskSpillStore(dir, spillTTLConfig{Generic: time.Hour, Sysprobe: time.Hour, GC: time.Hour})
	// 模拟 logit 的 <base>.log 与 <base>.log.wf 兄弟文件共享同一 id
	logMain := filepath.Join(dir, "sysprobe.runlog-abc.log")
	logWf := filepath.Join(dir, "sysprobe.runlog-abc.log.wf")
	os.WriteFile(logMain, []byte("main"), 0644)
	os.WriteFile(logWf, []byte("wf"), 0644)
	got, ok := st.resolve("abc")
	if !ok || got != logMain {
		t.Fatalf("resolve should prefer main .log, got %q ok=%v", got, ok)
	}
}

func TestReconcileMarksRunningInterrupted(t *testing.T) {
	dir := t.TempDir()
	st := newDiskSpillStore(dir, spillTTLConfig{Generic: time.Hour, Sysprobe: time.Hour, GC: time.Hour})
	meta := filepath.Join(dir, "sysprobe.submit-abc.meta.json")
	os.WriteFile(meta, []byte(`{"job_id":"abc","status":"running"}`), 0644)
	st.reconcile()
	b, _ := os.ReadFile(meta)
	if !strings.Contains(string(b), `"interrupted"`) {
		t.Fatalf("status not interrupted: %s", b)
	}
}
