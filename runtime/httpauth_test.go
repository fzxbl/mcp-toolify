package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestAuthz 构造一份最小可用的 TokenAuthz（一个只读 token）。
// trustHeader 控制是否信任客户端身份头——默认不信任，只有显式开启才采信。
func newTestAuthz(t *testing.T, trustHeader bool) *TokenAuthz {
	t.Helper()
	az, err := NewTokenAuthz(TokenAuthzConfig{
		Tokens: []TokenConfig{{
			Token: "t-ro", Name: "readonly-agent", Applicant: "tester",
			Allow: []string{"capability=read"},
		}},
		IdentityHeaders:     []string{"X-Real-User", "X-MCP-User"},
		TrustIdentityHeader: trustHeader,
	})
	if err != nil {
		t.Fatalf("NewTokenAuthz: %v", err)
	}
	return az
}

// TestHTTPMiddlewareRejectsUnknownToken：未配置的 token 与缺失 Authorization 一律 401，
// 且不得进入下游 handler（fail-closed）。
func TestHTTPMiddlewareRejectsUnknownToken(t *testing.T) {
	az := newTestAuthz(t, false)
	var reached bool
	h := az.HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	for _, hdr := range []string{"", "Bearer wrong", "Bearer ", "Basic t-ro"} {
		reached = false
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("Authorization=%q => %d, want 401", hdr, w.Code)
		}
		if reached {
			t.Errorf("Authorization=%q must not reach downstream handler", hdr)
		}
	}
}

// TestHTTPMiddlewareInjectsSubject：合法 token 放行，并把 token 用途名与身份注入 ctx；
// Subject.Token 必须是配置里的 name，绝不是 token 值。身份头只在显式开启
// trust_identity_header 时才被采信（默认不信任见 identity_test.go）。
func TestHTTPMiddlewareInjectsSubject(t *testing.T) {
	az := newTestAuthz(t, true)
	var got *Subject
	h := az.HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = SubjectFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer t-ro")
	req.Header.Set("X-MCP-User", "fallback")
	req.Header.Set("X-Real-User", "zhangsan")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("valid token => %d, want 200", w.Code)
	}
	if got == nil {
		t.Fatal("Subject must be injected into ctx")
	}
	if got.Token != "readonly-agent" {
		t.Errorf("Subject.Token = %q, want 配置里的 name readonly-agent", got.Token)
	}
	if got.ID != "zhangsan" {
		t.Errorf("Subject.ID = %q, want zhangsan (first non-empty identity header)", got.ID)
	}
}

// TestNewCallTakesSubjectFromContext：newCall 必须取 ctx 里认证阶段填好的 Subject，
// 否则 token 准入永远看到空用途名、整条 HTTP 路径等于没有鉴权判据。
func TestNewCallTakesSubjectFromContext(t *testing.T) {
	ctx := WithSubject(context.Background(), &Subject{ID: "zhangsan", Token: "ops-agent"})
	c := newCall(ctx, "tools/list", nil)
	if c.Subject == nil {
		t.Fatal("Subject must be non-nil")
	}
	if c.Subject.Token != "ops-agent" || c.Subject.ID != "zhangsan" {
		t.Errorf("Subject = %+v, want {zhangsan ops-agent}", c.Subject)
	}

	// 未注入时仍是非 nil 空 Subject：空用途名在 byName 里查不到规则，等同全拒。
	c = newCall(context.Background(), "tools/list", nil)
	if c.Subject == nil || c.Subject.Token != "" {
		t.Errorf("missing subject => %+v, want empty non-nil", c.Subject)
	}
}

// TestHTTPHeadersInjectsSnapshot：HTTP 层必须把请求头快照注入 ctx，
// 供 Call.Headers 使用。
func TestHTTPHeadersInjectsSnapshot(t *testing.T) {
	var got http.Header
	h := HTTPHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = HeadersFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Tenant-Id", "t1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.Get("X-Tenant-Id") != "t1" {
		t.Errorf("headers snapshot = %v", got)
	}
}
