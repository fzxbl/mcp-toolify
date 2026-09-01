package spill

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCreatePathThirdPartyWriter：CreatePath 交出的路径要能被「自己 os.Create
// 覆盖写」的第三方写入方直接用——那正是 logit / 批量执行框架的行为，也是这条 API
// 与 Create 的唯一区别。写进去的内容必须能被探索与下载。
func TestCreatePathThirdPartyWriter(t *testing.T) {
	st := hostStore(t)
	id, path, err := CreatePath(FormatText)
	if err != nil {
		t.Fatalf("CreatePath: %v", err)
	}
	if filepath.Dir(path) != st.dir {
		t.Fatalf("path = %q，应落在 spill 目录 %q 内", path, st.dir)
	}
	// 第三方写入方的典型行为：os.Create 直接截断重写（所以这类文件不能有文件头）。
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("第三方写入方 os.Create 失败: %v", err)
	}
	if _, err := f.WriteString("row-1\nrow-2\n"); err != nil {
		t.Fatalf("写入: %v", err)
	}
	_ = f.Close()

	out, err := explore(exploreInput{ID: id, Op: opRead})
	if err != nil {
		t.Fatalf("explore(read): %v", err)
	}
	if content, _ := out["content"].(string); !strings.Contains(content, "row-2") {
		t.Errorf("探索结果 = %v，应能读到第三方写进去的内容", out)
	}

	rec := httptest.NewRecorder()
	st.downloadHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, downloadPath+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("下载状态码 = %d，want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "row-1\nrow-2\n" {
		t.Errorf("下载内容 = %q，应是文件原文（无文件头）", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q，text 应按 text/plain 返回", ct)
	}
}

// TestExternSiblingFilesAreCollected：第三方写入方常顺带产生兄弟文件
// （logit 的 .wf、batchrun 的 .debug）。它们必须算进同一个 id 的账，
// 否则永远没人回收，落盘目录只增不减。
func TestExternSiblingFilesAreCollected(t *testing.T) {
	st := hostStore(t)
	id, path, err := CreatePath(FormatText)
	if err != nil {
		t.Fatalf("CreatePath: %v", err)
	}
	if err := os.WriteFile(path+".wf", []byte("warning\n"), 0o600); err != nil {
		t.Fatalf("造兄弟文件: %v", err)
	}
	base := filepath.Base(path)
	if got, ok := idOfFileName(base); !ok || got != id {
		t.Fatalf("idOfFileName(%q) = %q,%v，want %q,true", base, got, ok, id)
	}
	if got, ok := idOfFileName(base + ".wf"); !ok || got != id {
		t.Fatalf("idOfFileName(%q) = %q,%v，兄弟文件应归到同一个 id", base+".wf", got, ok)
	}
	// TTL 归零后一轮 GC 应该把主文件与兄弟文件一起删掉。
	st.ttl = 0
	st.gcOnce()
	if n := countFiles(t, st.dir); n != 0 {
		t.Errorf("GC 后剩 %d 个文件，主文件与兄弟文件都该被回收", n)
	}
}

// TestCreatePathValidation：格式与安装状态要在给出路径之前就校验，
// 否则调用方拿到一个没人能探索的路径。
func TestCreatePathValidation(t *testing.T) {
	hostStore(t)
	if _, _, err := CreatePath(Format("csv")); err == nil {
		t.Error("未知格式必须报错")
	}
	current.Store(nil)
	if _, _, err := CreatePath(FormatText); err != ErrNotInstalled {
		t.Errorf("err = %v, want ErrNotInstalled", err)
	}
}
