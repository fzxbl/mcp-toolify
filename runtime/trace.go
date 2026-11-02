package runtime

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// 本文件是基座的 trace context：日志 id 就是 W3C trace-id，请求进入副本时有 server span。
//
// 两种部署模式共用一条规则——「ctx 里已有本地 span 就沿用」：
//   - 外挂到带 tracing 中间件的宿主（如 gf）：宿主已经建好 server span，基座再建一层就成了
//     双层 span，所以直接沿用，日志 id 与宿主日志的 TraceId 天然一致；
//   - 独立部署，或宿主没有建 span：基座自己从请求头提取上游上下文并建 server span。
//
// 基座只用全局 TracerProvider，不配置 exporter：宿主没配时是 no-op，开销接近零，
// 但日志串联照常工作（见 HTTPTrace 的自生成兜底）。

// traceResponseHeader 是回写给调用方的响应头（W3C Trace Context Level 2），
// 让没带 traceparent 的调用方也能拿到 trace-id 去查日志。
const traceResponseHeader = "traceresponse"

// instrumentationName 是基座 span 的 instrumentation scope。
const instrumentationName = "github.com/fzxbl/mcp-toolify/runtime"

// tracer 每次从全局 provider 取：缓存成包级变量会让宿主（与测试）之后设置的 provider 不生效。
func tracer() trace.Tracer { return otel.Tracer(instrumentationName) }

// outboundPropagator 是基座提取与注入 trace 头共用的 propagator：宿主全局配置的 propagator
// 加上 W3C TraceContext（排在后面，两者都提取到时以 W3C 为准）。
//
// 为什么显式带上 TraceContext：otel 的全局 propagator 默认是 no-op，独立部署时若只用全局的，
// traceparent 会被静默忽略。为什么还带上全局的：外挂模式下属主副本由宿主按全局 propagator
// 提取，宿主若只认 B3 / Jaeger，只发 traceparent 会让跨副本的 trace 静默断开。
func outboundPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(otel.GetTextMapPropagator(), propagation.TraceContext{})
}

// LogIDFromContext 返回 ctx 的日志 id，即当前 span 的 trace-id（32 位小写十六进制），
// 没有有效 span 时返回 ""。它是「宿主日志 ↔ 审计事件 ↔ 基座日志」的 join key。
func LogIDFromContext(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}

// HTTPTrace 为每个请求确定 trace 上下文，route 是 span 名里的路径模板（用模板而不是真实路径，
// 避免把含 id 的路径当 span 名导致高基数）。
//
// 规则：
//   - ctx 已有本地 span：沿用，不建 span、不读请求头（因此套两层也只有一个 span）；
//   - 否则：已有远端上下文（宿主只做了 Extract）就以它为父，没有就从请求头提取，再建 server span；
//   - 全局 provider 是 no-op 且上游没带合法 trace 头时，span 无效，基座自生成 trace-id，
//     保证同一请求的日志仍有同一个 id。
//
// 格式不合法的 traceparent 由 propagator 丢弃，等同未携带：W3C 格式严格，
// 不存在往 key=value 日志里注入字段的余地，所以不需要额外校验或告警。
func HTTPTrace(route string, next http.Handler) http.Handler {
	return httpTraceRoute(func() string { return route }, next)
}

// httpTraceRoute 是 HTTPTrace 的内部形态：route 在请求时求值。MCP 端点要用它——
// build 时挂载前缀可能还没定（Start 不经 Mount），请求到达时前缀必然已经生效。
func httpTraceRoute(route func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() && !sc.IsRemote() {
			setTraceResponse(w, sc)
			next.ServeHTTP(w, r)
			return
		}
		if !trace.SpanContextFromContext(ctx).IsValid() {
			ctx = outboundPropagator().Extract(ctx, propagation.HeaderCarrier(r.Header))
		}
		attrs := []attribute.KeyValue{
			attribute.String("http.request.method", r.Method),
			attribute.String("url.path", r.URL.Path),
		}
		if addr := SelfHostPort(); addr != "" {
			attrs = append(attrs, attribute.String("server.address", addr))
		}
		ctx, span := tracer().Start(ctx, r.Method+" "+route(),
			trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
		defer span.End()
		if !trace.SpanContextFromContext(ctx).IsValid() {
			ctx = trace.ContextWithSpanContext(ctx, newLocalSpanContext())
		}
		// 在 next 之前写：handler 一旦写 body，响应头就发出去了。
		setTraceResponse(w, trace.SpanContextFromContext(ctx))

		sw := &traceStatusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(ctx))
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		span.SetAttributes(attribute.Int("http.response.status_code", status))
		if status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
	})
}

// setTraceResponse 按 W3C Level 2 格式回写 traceresponse：00-<trace-id>-<span-id>-<flags>。
func setTraceResponse(w http.ResponseWriter, sc trace.SpanContext) {
	w.Header().Set(traceResponseHeader,
		fmt.Sprintf("00-%s-%s-%s", sc.TraceID(), sc.SpanID(), sc.TraceFlags()))
}

// newLocalSpanContext 生成一个随机的本地 SpanContext（不置 sampled 位），
// 只用于 no-op provider 下给日志一个 id；它不会被导出。
func newLocalSpanContext() trace.SpanContext {
	var cfg trace.SpanContextConfig
	for !cfg.TraceID.IsValid() {
		rand.Read(cfg.TraceID[:])
	}
	for !cfg.SpanID.IsValid() {
		rand.Read(cfg.SpanID[:])
	}
	return trace.NewSpanContext(cfg)
}

// traceStatusWriter 记录响应状态码供 span 使用，同时保留 SSE 需要的 Flush 能力。
type traceStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *traceStatusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *traceStatusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush 保留 SSE 所需的 http.Flusher 能力。
func (w *traceStatusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 供 http.ResponseController 访问底层 writer。
func (w *traceStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
