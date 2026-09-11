package spill

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// TestDownloadURLUsesMountPrefix：Mount("/mcp") 后 URLFor 必须拼出
// /mcp/plugin/spill/<id>。不做本地固化——直接走 PublicURL/RoutePath。
func TestDownloadURLUsesMountPrefix(t *testing.T) {
	dir := t.TempDir()
	r := runtime.New(runtime.Config{
		ConfigPath: writeConfig(t, baseTokens+fmt.Sprintf(`
[spill]
dir = %q
`, filepath.Join(dir, "data"))),
		SelfAddr:      "10.189.95.145:8011",
		PublicBaseURL: "http://10.189.95.145:8011",
	}, nil)
	if err := Install(r); err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() { r.RunStop(context.Background()) })

	if err := r.Mount("/mcp", func(string, http.Handler) {}); err != nil {
		t.Fatalf("Mount: %v", err)
	}

	id, err := Put("x", FormatText, []byte("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	got := URLFor(id)
	wantPrefix := "http://10.189.95.145:8011/mcp/plugin/spill/"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("URLFor=%q，want 前缀 %q", got, wantPrefix)
	}
}

// TestDownloadEndpointIsPublic：下载只靠不可猜测的 spill id，浏览器直接打开
// 不应再要求 Authorization。
func TestDownloadEndpointIsPublic(t *testing.T) {
	dir := t.TempDir()
	r := installed(t, baseTokens+fmt.Sprintf(`
[spill]
dir = %q
`, dir))
	mux := http.NewServeMux()
	if err := r.Mount("/mcp", func(pattern string, h http.Handler) {
		mux.Handle(pattern, h)
	}); err != nil {
		t.Fatalf("Mount: %v", err)
	}

	st, err := newStore(dir, time.Hour, time.Hour, quota{})
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(st.close)
	id, _, err := st.put("a.read", bigResult("PUBLIC-DOWNLOAD-PAYLOAD"), testOwner)
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/mcp/plugin/spill/"+id, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK {
		t.Fatalf("无 token GET => %d，want 200", rec.Code)
	}
	if !strings.Contains(string(body), "PUBLIC-DOWNLOAD-PAYLOAD") {
		t.Fatalf("无 token 未取到内容: %s", body)
	}
}
