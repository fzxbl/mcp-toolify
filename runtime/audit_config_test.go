package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSetAuditHeadersDedupAndTrim(t *testing.T) {
	t.Cleanup(func() { SetAuditHeaders(nil) })

	SetAuditHeaders([]string{" X-Tenant-Id ", "X-Client-Id", "", "X-Client-Id", "x-tenant-id"})
	got := auditHeaders()
	// 去空白、去空项、按小写去重、保序：X-Tenant-Id 先出现，X-Client-Id 其次。
	want := []string{"X-Tenant-Id", "X-Client-Id"}
	if len(got) != len(want) {
		t.Fatalf("auditHeaders len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("auditHeaders[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHTTPAuditHeadersCapturesConfiguredHeaders(t *testing.T) {
	t.Cleanup(func() { SetAuditHeaders(nil) })
	SetAuditHeaders([]string{"X-Tenant-Id", "X-Client-Id"})

	var fieldsSnapshot map[string]string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		fieldsSnapshot = map[string]string{}
		for _, f := range auditHeaderFields(r.Context()) {
			fieldsSnapshot[f.Key] = f.Value
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("X-Tenant-Id", "t123")
	// X-Client-Id 故意不设置，应记为 "-"

	HTTPAuditHeaders(next).ServeHTTP(httptest.NewRecorder(), req)

	if got := fieldsSnapshot["x-tenant-id"]; got != "t123" {
		t.Errorf("field x-tenant-id = %q, want %q", got, "t123")
	}
	if got := fieldsSnapshot["x-client-id"]; got != "-" {
		t.Errorf("field x-client-id = %q, want %q (missing header)", got, "-")
	}
}

func TestHTTPAuditHeadersNoopWhenUnconfigured(t *testing.T) {
	t.Cleanup(func() { SetAuditHeaders(nil) })
	SetAuditHeaders(nil)

	var n int
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if fs := auditHeaderFields(r.Context()); fs != nil {
			t.Errorf("expected no audit fields when unconfigured, got %d", len(fs))
		}
		n++
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("X-Tenant-Id", "t123")
	HTTPAuditHeaders(next).ServeHTTP(httptest.NewRecorder(), req)

	if n != 1 {
		t.Fatalf("next handler called %d times, want 1", n)
	}
}
