package runtime

import (
	"log"
	"net/http"
	"time"
)

// statusRecorder 包一层 ResponseWriter，记录状态码与写出字节数，用于 access 日志。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// httpAccessLog 是内置接入层（access/service）日志中间件：每个 HTTP 请求一行，
// 字段含 logid/method/path/status/cost/user，与审计日志共享同一个 logid 以便串联。
// 仅用于独立 Run() 启动路径；挂载到宿主时不启用（宿主自有 access 日志）。
// 需先经过 HTTPLogID 注入 logid。
func httpAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		dur := time.Since(start).Round(time.Millisecond)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		logAccess(r, status, dur)
	})
}

func logAccess(r *http.Request, status int, dur time.Duration) {
	ctx := r.Context()
	if auditLogger == nil {
		log.Printf("[access] logid=%s %s %s status=%d cost=%s user=%s",
			LogIDFromContext(ctx), r.Method, r.URL.Path, status, dur, identityFromCtx(ctx))
		return
	}
	fields := []Field{
		String("logid", LogIDFromContext(ctx)),
		String("method", r.Method),
		String("path", r.URL.Path),
		String("status", itoa(status)),
		String("cost", dur.String()),
		String("user", identityFromCtx(ctx)),
	}
	if status >= 500 {
		auditLogger.Warning("access", fields...)
	} else {
		auditLogger.Notice("access", fields...)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
