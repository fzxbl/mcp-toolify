package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// 上游 traceparent：trace-id 4bf9…4736，父 span-id 00f0…02b7，sampled。
const (
	upTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	upTraceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
	upSpanID      = "00f067aa0ba902b7"
)

var traceIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// installTestProvider 把全局 TracerProvider 换成带内存 exporter 的 SDK provider，
// 测试结束恢复原 provider。改的是全局状态，调用它的测试不能并行。
func installTestProvider(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
	return exp
}

// useTestPropagator 把全局 propagator 换成 testPropagator，测试结束恢复。
func useTestPropagator(t *testing.T) {
	t.Helper()
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(testPropagator{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })
}

// ctxWithTraceID 构造一个带本地有效 SpanContext 的 ctx，供需要「已有 trace-id」的测试使用。
func ctxWithTraceID(id string) context.Context {
	tid, err := trace.TraceIDFromHex(id)
	if err != nil {
		panic(err)
	}
	sid, _ := trace.SpanIDFromHex("0102030405060708")
	return trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid}))
}

// testPropagator 模拟宿主配置的非 W3C propagator：只认 X-Test-Trace 头（32 位 hex trace-id）。
type testPropagator struct{}

const testTraceHeader = "X-Test-Trace"

func (testPropagator) Inject(ctx context.Context, c propagation.TextMapCarrier) {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		c.Set(testTraceHeader, sc.TraceID().String())
	}
}

func (testPropagator) Extract(ctx context.Context, c propagation.TextMapCarrier) context.Context {
	tid, err := trace.TraceIDFromHex(c.Get(testTraceHeader))
	if err != nil {
		return ctx
	}
	sid, _ := trace.SpanIDFromHex("0a0b0c0d0e0f1011")
	return trace.ContextWithRemoteSpanContext(ctx,
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, Remote: true}))
}

func (testPropagator) Fields() []string { return []string{testTraceHeader} }

// serveTrace 用 HTTPTrace 包住 h 处理一次请求，返回 handler 内看到的 trace-id 与响应。
func serveTrace(t *testing.T, req *http.Request, h http.Handler) (string, *httptest.ResponseRecorder) {
	t.Helper()
	var seen string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = LogIDFromContext(r.Context())
		if h != nil {
			h.ServeHTTP(w, r)
		}
	})
	rec := httptest.NewRecorder()
	HTTPTrace("/mcp", inner).ServeHTTP(rec, req)
	return seen, rec
}

func TestHTTPTraceNoopAdoptsUpstream(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("traceparent", upTraceparent)
	if got, _ := serveTrace(t, req, nil); got != upTraceID {
		t.Fatalf("trace-id = %q, want upstream %q", got, upTraceID)
	}
}

func TestHTTPTraceNoopGeneratesWhenMissing(t *testing.T) {
	got, _ := serveTrace(t, httptest.NewRequest(http.MethodGet, "/mcp", nil), nil)
	if !traceIDRe.MatchString(got) || got == "00000000000000000000000000000000" {
		t.Fatalf("trace-id = %q, want generated 32-hex", got)
	}
}

func TestHTTPTraceInvalidTraceparent(t *testing.T) {
	cases := map[string]string{
		"zero trace-id": "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"uppercase":     "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01",
		"version ff":    "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"short":         "00-4bf92f3577b34da6a3ce929d0e0e473-00f067aa0ba902b7-01",
	}
	for name, tp := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
			req.Header.Set("traceparent", tp)
			got, _ := serveTrace(t, req, nil)
			if !traceIDRe.MatchString(got) || got == upTraceID || got == "00000000000000000000000000000000" {
				t.Fatalf("trace-id = %q, want fresh generated id", got)
			}
		})
	}
}

func TestHTTPTraceWritesTraceresponse(t *testing.T) {
	got, rec := serveTrace(t, httptest.NewRequest(http.MethodGet, "/mcp", nil),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "x") }))
	re := regexp.MustCompile(`^00-` + got + `-[0-9a-f]{16}-0[01]$`)
	if h := rec.Header().Get(traceResponseHeader); !re.MatchString(h) {
		t.Fatalf("traceresponse = %q, want 00-%s-<span>-<flags>", h, got)
	}
}

func TestHTTPTraceCreatesServerSpan(t *testing.T) {
	exp := installTestProvider(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp?x=1", nil)
	req.Header.Set("traceparent", upTraceparent)
	if got, _ := serveTrace(t, req, nil); got != upTraceID {
		t.Fatalf("trace-id = %q, want %q", got, upTraceID)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.SpanKind != trace.SpanKindServer || s.Name != "GET /mcp" {
		t.Fatalf("span kind=%v name=%q, want server \"GET /mcp\"", s.SpanKind, s.Name)
	}
	if s.Parent.SpanID().String() != upSpanID {
		t.Fatalf("parent span-id = %s, want %s", s.Parent.SpanID(), upSpanID)
	}
	attrs := map[string]string{}
	for _, kv := range s.Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	want := map[string]string{"http.request.method": "GET", "url.path": "/mcp", "http.response.status_code": "200"}
	for k, v := range want {
		if attrs[k] != v {
			t.Errorf("attr %s = %q, want %q", k, attrs[k], v)
		}
	}
}

func TestHTTPTraceMarks5xxError(t *testing.T) {
	exp := installTestProvider(t)
	serveTrace(t, httptest.NewRequest(http.MethodGet, "/mcp", nil),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Status.Code != codes.Error {
		t.Fatalf("spans=%d status=%v, want 1 span with Error", len(spans), spans)
	}
}

func TestHTTPTraceAdoptsLocalSpan(t *testing.T) {
	exp := installTestProvider(t)
	ctx, host := otel.Tracer("host").Start(context.Background(), "host")
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(ctx)
	got, rec := serveTrace(t, req, nil)
	host.End()
	if want := host.SpanContext().TraceID().String(); got != want {
		t.Fatalf("trace-id = %q, want host %q", got, want)
	}
	if spans := exp.GetSpans(); len(spans) != 1 || spans[0].Name != "host" {
		t.Fatalf("got %d spans, want only the host span", len(spans))
	}
	if rec.Header().Get(traceResponseHeader) == "" {
		t.Fatal("traceresponse missing when adopting host span")
	}
}

func TestHTTPTraceRemoteOnlyCreatesSpan(t *testing.T) {
	exp := installTestProvider(t)
	tid, _ := trace.TraceIDFromHex(upTraceID)
	sid, _ := trace.SpanIDFromHex(upSpanID)
	ctx := trace.ContextWithRemoteSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid,
			TraceFlags: trace.FlagsSampled, Remote: true}))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(ctx)
	if got, _ := serveTrace(t, req, nil); got != upTraceID {
		t.Fatalf("trace-id = %q, want %q", got, upTraceID)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Parent.SpanID().String() != upSpanID {
		t.Fatalf("got %d spans, want 1 child of remote %s", len(spans), upSpanID)
	}
}

func TestHTTPTraceNestedCreatesOneSpan(t *testing.T) {
	exp := installTestProvider(t)
	h := HTTPTrace("/a", HTTPTrace("/a", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/a", nil))
	if n := len(exp.GetSpans()); n != 1 {
		t.Fatalf("got %d spans, want 1", n)
	}
}

func TestHTTPTraceKeepsFlusher(t *testing.T) {
	serveTrace(t, httptest.NewRequest(http.MethodGet, "/mcp", nil),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := w.(http.Flusher); !ok {
				t.Error("writer lost http.Flusher")
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("ResponseController.Flush: %v", err)
			}
		}))
}

func TestHTTPTraceHonorsGlobalPropagator(t *testing.T) {
	installTestProvider(t)
	useTestPropagator(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(testTraceHeader, upTraceID)
	if got, _ := serveTrace(t, req, nil); got != upTraceID {
		t.Fatalf("trace-id = %q, want %q from global propagator", got, upTraceID)
	}
}
