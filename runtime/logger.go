package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
)

// Logger 是基座与插件输出运维日志的唯一出口，由宿主经 Config.Logger 注入。
//
// 签名刻意与 (*slog.Logger).Log 一字不差：用 slog 的宿主直接传 *slog.Logger；
// zap / zerolog / logrus 这类自带 slog.Handler 的，包一层 slog.New(h)；其它任意 logger
// （如 gf 的 glog）实现这一个方法即可。不直接收 slog.Handler：它有四个方法，
// WithAttrs / WithGroup 的语义宿主自己适配时很容易写错。
//
// 约定（适配方需要知道的）：
//   - msg 是已经格式化好的整句，人可直接读；非结构化 logger 只打 msg 也不丢信息。
//   - args 是 slog 风格的 key/value 交替列表，目前只放跨系统对账用的字段
//     （logid）。glog 这类把 args 拼进行尾即可。
//   - ctx 总是非 nil；请求路径上的日志带的是该请求的 ctx。
type Logger interface {
	Log(ctx context.Context, level slog.Level, msg string, args ...any)
}

// defaultLogger 在宿主未注入时使用：每次都转给**当时的** slog.Default()，
// 宿主之后再 slog.SetDefault 也能生效。slog 未被设置时它写到标准库 log，
// 与改造前的输出位置相同（行内多了级别）。
type defaultLogger struct{}

func (defaultLogger) Log(ctx context.Context, level slog.Level, msg string, args ...any) {
	slog.Default().Log(ctx, level, msg, args...)
}

// loggerBox 让 atomic.Pointer 能装接口值。
type loggerBox struct{ l Logger }

// currentLog 是进程级生效的 Logger。与 SelfAddr / logid 头名一样是进程级状态：
// HTTPTrace、owner 路由这些包级 HTTP 包装拿不到 Registry，只能从这里取。
var currentLog atomic.Pointer[loggerBox]

func init() { currentLog.Store(&loggerBox{defaultLogger{}}) }

// SetLogger 设置进程级 Logger；nil 恢复默认（转给 slog.Default()）。
// 通常不必直接调用：NewRegistry 会按 Config.Logger 设置。
func SetLogger(l Logger) {
	if l == nil {
		l = defaultLogger{}
	}
	currentLog.Store(&loggerBox{l})
}

// logger 返回当前生效的 Logger。
func logger() Logger { return currentLog.Load().l }

// logf 是基座内部的日志入口：msg 按 format 格式化，ctx 里有 logid 时自动带上
// "logid" 字段——它是「宿主 access log ↔ 审计事件 ↔ 框架日志」的 join key。
func logf(ctx context.Context, level slog.Level, format string, args ...any) {
	if ctx == nil {
		ctx = context.Background()
	}
	msg := fmt.Sprintf(format, args...)
	if id := LogIDFromContext(ctx); id != "" {
		logger().Log(ctx, level, msg, "logid", id)
		return
	}
	logger().Log(ctx, level, msg)
}
