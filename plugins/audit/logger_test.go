package audit

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// recLogger 模拟宿主经 Registry.Logger 注入的 logger。
type recLogger struct {
	mu   sync.Mutex
	recs []recLine
}

type recLine struct {
	level slog.Level
	msg   string
	args  []any
}

func (l *recLogger) Log(_ context.Context, level slog.Level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, recLine{level, msg, append([]any(nil), args...)})
}

// TestDropGoesToInjectedLogger：「审计丢了事件」不能走审计自己的 Sink（说的正是 Sink
// 出了问题），只能走宿主的 Logger——必须是 Error 级别、带 logid 字段。
func TestDropGoesToInjectedLogger(t *testing.T) {
	l := &recLogger{}
	d := &dispatcher{dropAlarm: &throttledLogger{interval: time.Hour}, log: l}
	d.drop(Event{LogID: "lid-drop", Method: "tools/call", Tool: "a.read"}, "事件队列已满")

	if len(l.recs) != 1 {
		t.Fatalf("应收到 1 条日志，实际 %+v", l.recs)
	}
	rec := l.recs[0]
	if rec.level != slog.LevelError || !strings.Contains(rec.msg, "审计事件被丢弃") {
		t.Errorf("丢事件应为 Error 级别的「审计事件被丢弃」，实际 %v %q", rec.level, rec.msg)
	}
	if len(rec.args) != 2 || rec.args[0] != "logid" || rec.args[1] != "lid-drop" {
		t.Errorf("必须带 logid 字段，实际 args = %v", rec.args)
	}
}
