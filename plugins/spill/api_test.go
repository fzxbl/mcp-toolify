package spill

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// hostStore 造一个只给宿主 API 用的 store，并把它挂成 current（Install 会做同样的事，
// 但这里不想连带装中间件与路由）。
func hostStore(t *testing.T) *store {
	t.Helper()
	st, err := newStore(t.TempDir(), time.Hour, time.Hour,
		quota{MaxFileBytes: 1 << 20, MaxTotalBytes: 8 << 20})
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	current.Store(st)
	t.Cleanup(func() { current.Store(nil); st.close() })
	return st
}

// TestPutOpenRoundTrip：宿主写进去的就是宿主读出来的——内部文件头不能漏进 payload。
func TestPutOpenRoundTrip(t *testing.T) {
	hostStore(t)
	want := "line-1\nline-2\n"
	id, err := Put("probe.result", FormatText, []byte(want))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, info, err := Open(id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != want {
		t.Errorf("payload = %q, want %q（文件头不该被读出来）", got, want)
	}
	if info.Format != FormatText || info.Size != int64(len(want)) || !info.Shared {
		t.Errorf("info = %+v，want text/%d/shared", info, len(want))
	}
	if info.Name != "probe.result" {
		t.Errorf("name = %q，写入时给的名字应原样留在元信息里", info.Name)
	}
}

// TestCreateReadableWhileWriting：写一半就能读到已写内容。异步任务（探测、导出）
// 的整套用法都靠这一条：先给 id、后台继续写、agent 边读。
func TestCreateReadableWhileWriting(t *testing.T) {
	hostStore(t)
	w, err := Create("probe.log", FormatJSONL)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte(`{"n":1}` + "\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := explore(exploreInput{ID: w.ID(), Op: opRead})
	if err != nil {
		t.Fatalf("explore(read): %v", err)
	}
	if content, _ := out["content"].(string); !strings.Contains(content, `{"n":1}`) {
		t.Errorf("写入未关闭时就该能读到已写的行，实际 = %v", out)
	}
	if _, err := w.Write([]byte(`{"n":2}` + "\n")); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close 应幂等，第二次返回 %v", err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("关闭之后继续写必须报错，否则调用方以为写进去了")
	}
}

// TestWriterRefusesOverFileLimit：单文件上限必须在写入路径上生效，
// 否则一个跑飞的异步任务能把磁盘写满。
func TestWriterRefusesOverFileLimit(t *testing.T) {
	st := hostStore(t)
	st.q.MaxFileBytes = 16
	w, err := Create("probe.log", FormatText)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer w.Close()
	if _, err := w.Write([]byte(strings.Repeat("a", 32))); err == nil {
		t.Fatal("超过单文件上限必须报错")
	}
}

// TestPutRejectsUnknownFormat：格式只有三种，第四种要在写之前就被拒绝
// （否则文件头里会留下一个探索工具不认识的格式）。
func TestPutRejectsUnknownFormat(t *testing.T) {
	hostStore(t)
	if _, err := Put("x", Format("csv"), []byte("a,b")); err == nil {
		t.Fatal("未知格式必须报错")
	}
}

// TestHostAPIWithoutInstall：没装插件时写入要给出明确错误，而不是悄悄往
// 临时目录写业务数据（那样 spill_explore 还会找不到）。
func TestHostAPIWithoutInstall(t *testing.T) {
	current.Store(nil)
	if _, err := Put("x", FormatText, []byte("a")); err != ErrNotInstalled {
		t.Fatalf("err = %v, want ErrNotInstalled", err)
	}
	if _, err := Create("x", FormatText); err != ErrNotInstalled {
		t.Fatalf("Create err = %v, want ErrNotInstalled", err)
	}
	if url := URLFor("deadbeef"); url != "" {
		t.Errorf("未安装时 URLFor 应返回空串，实际 %q", url)
	}
}

// TestSharedContentDownloadable：Put 写的内容是共享的，任何通过 token 认证的
// 调用方都能下载（Put 的注释把这条放宽写清楚了，用例把它钉住）。
func TestSharedContentDownloadable(t *testing.T) {
	st := hostStore(t)
	id, err := Put("export.csv", FormatText, []byte("a,b\n1,2\n"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, downloadPath+id, nil)
	st.downloadHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "a,b\n1,2\n" {
		t.Errorf("下载内容 = %q，只应包含 payload", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q，text 内容应按 text/plain 返回", ct)
	}
}

// TestOwnedContentStillPublicDownload：PutFor 仍可记录主体（供审计/排查），
// 但公开下载端点不再按 Subject 拦截——拿到 id 就能下。
func TestOwnedContentStillPublicDownload(t *testing.T) {
	st := hostStore(t)
	id, err := PutFor(&runtime.Subject{Token: "ops-agent", ID: "zhangsan"},
		"secret.txt", FormatText, []byte("only-mine"))
	if err != nil {
		t.Fatalf("PutFor: %v", err)
	}
	get := func(sub *runtime.Subject) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, downloadPath+id, nil)
		if sub != nil {
			req = req.WithContext(runtime.WithSubject(req.Context(), sub))
		}
		st.downloadHandler().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := get(&runtime.Subject{Token: "ops-agent", ID: "zhangsan"}); code != http.StatusOK {
		t.Errorf("属主本人下载状态码 = %d，want 200", code)
	}
	if code := get(&runtime.Subject{Token: "ops-agent", ID: "lisi"}); code != http.StatusOK {
		t.Errorf("同 token 不同人下载状态码 = %d，want 200（公开下载）", code)
	}
	if code := get(nil); code != http.StatusOK {
		t.Errorf("无主体下载状态码 = %d，want 200（公开下载）", code)
	}
}

// TestSetDefaultDir：宿主的落盘目录常常只有运行期才知道（框架算出的应用根目录、
// 容器里挂进来的卷），静态配置文件写不出来；同时配置里显式写了 dir 就必须赢。
func TestSetDefaultDir(t *testing.T) {
	hostDir := t.TempDir()
	SetDefaultDir(hostDir)
	t.Cleanup(func() { SetDefaultDir("") })

	o, err := Section{}.normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if o.Dir != hostDir {
		t.Errorf("dir = %q，未配 dir 时应取宿主给的默认目录 %q", o.Dir, hostDir)
	}
	confDir := t.TempDir()
	o, err = Section{Dir: confDir}.normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if o.Dir != confDir {
		t.Errorf("dir = %q，配置里显式写的目录应优先于宿主默认值", o.Dir)
	}
}
