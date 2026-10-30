package runtime

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// 编译期断言：*slog.Logger 不加任何适配就满足 Logger——这是 Logger 签名的设计前提。
var _ Logger = (*slog.Logger)(nil)

// logRecord 是 recordingLogger 收到的一行。
type logRecord struct {
	level slog.Level
	msg   string
	args  []any
}

// recordingLogger 记下收到的每一行，模拟宿主注入的 logger（如 glog 适配器）。
type recordingLogger struct {
	mu   sync.Mutex
	recs []logRecord
}

func (l *recordingLogger) Log(_ context.Context, level slog.Level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, logRecord{level, msg, append([]any(nil), args...)})
}

// find 返回第一条 msg 含 sub 的记录。
func (l *recordingLogger) find(sub string) (logRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if strings.Contains(r.msg, sub) {
			return r, true
		}
	}
	return logRecord{}, false
}

// useLogger 注入一个 recordingLogger，用例结束时恢复默认。
func useLogger(t *testing.T) *recordingLogger {
	t.Helper()
	l := &recordingLogger{}
	SetLogger(l)
	t.Cleanup(func() { SetLogger(nil) })
	return l
}

// TestInjectedLoggerReceivesAuthzDeny：准入拒绝是 HTTP 200 + isError，日志是唯一信号，
// 所以它必须进宿主注入的 Logger、带 Warn 级别、带 logid 字段（与 access log 对账）。
func TestInjectedLoggerReceivesAuthzDeny(t *testing.T) {
	ResetToolsForTest()
	defer ResetToolsForTest()
	setToolLabels(ToolInfo{Name: "demo.write", Pkg: "demo",
		Labels: map[string]string{"capability": "write"}})
	l := useLogger(t)
	authzDenyAlarm = &warnThrottle{interval: 10 * time.Second}

	h := weakAuthz(t).Middleware()(func(ctx context.Context, c *Call) (*Result, error) {
		return TextResult("executed"), nil
	})
	ctx := WithLogID(context.Background(), "lid-deny")
	if _, err := h(ctx, &Call{Method: "tools/call", Tool: "demo.write",
		Subject: &Subject{Token: "weak"}}); err != nil {
		t.Fatal(err)
	}
	rec, ok := l.find("准入拒绝")
	if !ok {
		t.Fatalf("准入拒绝没有进注入的 Logger: %+v", l.recs)
	}
	if rec.level != slog.LevelWarn {
		t.Errorf("准入拒绝应为 Warn，实际 %v", rec.level)
	}
	if len(rec.args) != 2 || rec.args[0] != "logid" || rec.args[1] != "lid-deny" {
		t.Errorf("准入拒绝必须带 logid 字段，实际 args = %v", rec.args)
	}
}

// TestConfigLoggerIsUsedFromNew：Config.Logger 必须从 NewRegistry 起就生效——
// New 紧接着就打工具注册摘要，插件 Install 期也会打日志；r.Logger() 交给插件的是同一个。
func TestConfigLoggerIsUsedFromNew(t *testing.T) {
	l := &recordingLogger{}
	t.Cleanup(func() { SetLogger(nil) })
	r := New(Config{Logger: l}, nil)
	if _, ok := l.find("tools: effective match"); !ok {
		t.Errorf("New 的工具注册摘要没有进 Config.Logger: %+v", l.recs)
	}
	if r.Logger() != Logger(l) {
		t.Errorf("r.Logger() 应返回 Config.Logger 注入的实例")
	}
}

// TestNilLoggerFallsBackToSlogDefault：未注入时转给**调用当时**的 slog.Default()，
// 宿主之后再 slog.SetDefault 也能接住（不在 SetLogger 时快照）。
func TestNilLoggerFallsBackToSlogDefault(t *testing.T) {
	SetLogger(nil)
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	logf(WithLogID(context.Background(), "lid-x"), slog.LevelWarn, "hello %s", "world")
	out := buf.String()
	for _, want := range []string{"level=WARN", "hello world", "logid=lid-x"} {
		if !strings.Contains(out, want) {
			t.Errorf("默认出口缺少 %q: %q", want, out)
		}
	}
}
