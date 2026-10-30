package spill

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/fzxbl/mcp-toolify/runtime"
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

// TestPutErrorGoesToInjectedLogger：on_error=warn 时超阈值结果会原样返回，这条日志是
// 唯一记录，必须进宿主的 Logger、Error 级别、带 logid 字段。
func TestPutErrorGoesToInjectedLogger(t *testing.T) {
	l := &recLogger{}
	res := runtime.TextResult("big")
	got, err := onPutError(l, res, nil, &runtime.Call{Tool: "demo.big", LogID: "lid-put"},
		options{OnError: OnErrorWarn}, 1<<20, errors.New("disk full"))
	if err != nil || got != res {
		t.Fatalf("on_error=warn 应原样返回结果: res=%v err=%v", got, err)
	}
	if len(l.recs) != 1 {
		t.Fatalf("应收到 1 条日志，实际 %+v", l.recs)
	}
	rec := l.recs[0]
	if rec.level != slog.LevelError || !strings.Contains(rec.msg, "落盘失败") ||
		!strings.Contains(rec.msg, "disk full") {
		t.Errorf("落盘失败应为 Error 且含原因，实际 %v %q", rec.level, rec.msg)
	}
	if len(rec.args) != 2 || rec.args[0] != "logid" || rec.args[1] != "lid-put" {
		t.Errorf("必须带 logid 字段，实际 args = %v", rec.args)
	}
}
