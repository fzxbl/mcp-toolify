package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPLogIDUsesIncomingHeader(t *testing.T) {
	t.Cleanup(func() { SetLogIDHeader("") })
	SetLogIDHeader("X-Log-Id")

	var seen string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = LogIDFromContext(r.Context())
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("X-Log-Id", "abc123")
	rec := httptest.NewRecorder()
	HTTPLogID(next).ServeHTTP(rec, req)

	if seen != "abc123" {
		t.Errorf("ctx logid = %q, want %q", seen, "abc123")
	}
	if got := rec.Header().Get("X-Log-Id"); got != "abc123" {
		t.Errorf("response header X-Log-Id = %q, want %q", got, "abc123")
	}
}

func TestHTTPLogIDGeneratesWhenMissing(t *testing.T) {
	t.Cleanup(func() { SetLogIDHeader("") })
	SetLogIDHeader("X-Log-Id")

	var seen string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = LogIDFromContext(r.Context())
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec := httptest.NewRecorder()
	HTTPLogID(next).ServeHTTP(rec, req)

	if seen == "" {
		t.Fatal("expected a generated logid in ctx, got empty")
	}
	if got := rec.Header().Get("X-Log-Id"); got != seen {
		t.Errorf("response header X-Log-Id = %q, want ctx logid %q", got, seen)
	}
}

func TestSetLogIDHeaderDefaultsWhenEmpty(t *testing.T) {
	t.Cleanup(func() { SetLogIDHeader("") })
	SetLogIDHeader("")
	if got := logIDHeader(); got != defaultLogIDHeader {
		t.Errorf("logIDHeader() = %q, want default %q", got, defaultLogIDHeader)
	}
}
