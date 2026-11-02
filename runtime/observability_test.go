package runtime

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// captureLog 把标准库日志重定向到缓冲区，返回取回内容的函数。
func captureLog(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	flags := log.Flags()
	out := log.Writer()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(out); log.SetFlags(flags) })
	return buf.String
}

// weakAuthz 造一份两 token 的准入：weak 只能调 capability=confirm，
// admin 能调 capability=write。用来验证「越权被拒且留下日志」。
func weakAuthz(t *testing.T) *TokenAuthz {
	t.Helper()
	az, err := NewTokenAuthz(TokenAuthzConfig{Tokens: []TokenConfig{
		{Token: "t-weak", Name: "weak", Applicant: "tester", Allow: []string{"capability=confirm"}},
		{Token: "t-admin", Name: "admin", Applicant: "tester", Allow: []string{"capability=write"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return az
}

// TestAuthzDenyIsLogged 是终审 C1 的回归：基座准入固定装在插件链之前，它的拒绝
// **到不了 audit 插件**（终审实测越权尝试在审计流里 0 条事件、HTTP 200）。
// 那么这条日志就是全系统唯一的痕迹，不能没有；且绝不能回显 token 值。
func TestAuthzDenyIsLogged(t *testing.T) {
	ResetToolsForTest()
	defer ResetToolsForTest()
	setToolLabels(ToolInfo{Name: "demo.write", Pkg: "demo",
		Labels: map[string]string{"capability": "write"}})

	az := weakAuthz(t) // weak 只允许 capability=confirm
	authzDenyAlarm = &warnThrottle{interval: 10 * time.Second}
	logs := captureLog(t)

	h := az.Middleware()(func(ctx context.Context, c *Call) (*Result, error) {
		return TextResult("executed"), nil
	})
	ctx := WithSubject(context.Background(), &Subject{ID: "zhangsan", Token: "t-weak-value"})
	res, err := h(ctx, &Call{Method: "tools/call", Tool: "demo.write",
		Subject: &Subject{ID: "zhangsan", Token: "weak"}})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.Tool == nil || !res.Tool.IsError {
		t.Fatalf("越权调用必须被拒: %+v", res)
	}
	line := logs()
	if !strings.Contains(line, "准入拒绝") || !strings.Contains(line, "demo.write") {
		t.Errorf("准入拒绝必须留日志（含工具名），实际日志 = %q", line)
	}
	if strings.Contains(line, "t-weak-value") {
		t.Errorf("日志不得回显 token 值: %q", line)
	}
}

// TestAuthzListFilterIsLogged：tools/list 的过滤发生在 next 返回之后，链内 audit 记到的
// 是过滤**前**的全量清单。这条日志是「该 token 实际看见了几个」的唯一信号。
func TestAuthzListFilterIsLogged(t *testing.T) {
	ResetToolsForTest()
	defer ResetToolsForTest()
	setToolLabels(ToolInfo{Name: "demo.confirm", Pkg: "demo",
		Labels: map[string]string{"capability": "confirm"}})
	setToolLabels(ToolInfo{Name: "demo.write", Pkg: "demo",
		Labels: map[string]string{"capability": "write"}})

	az := weakAuthz(t)
	authzListAlarm = &warnThrottle{interval: time.Minute}
	logs := captureLog(t)

	h := az.Middleware()(func(ctx context.Context, c *Call) (*Result, error) {
		return &Result{List: &mcp.ListToolsResult{Tools: []*mcp.Tool{
			{Name: "demo.confirm"}, {Name: "demo.write"},
		}}}, nil
	})
	res, err := h(WithSubject(context.Background(), &Subject{Token: "weak"}),
		&Call{Method: "tools/list", Subject: &Subject{Token: "weak"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.List.Tools) != 1 || res.List.Tools[0].Name != "demo.confirm" {
		t.Fatalf("weak 只该看到 demo.confirm，实际 %+v", res.List.Tools)
	}
	if line := logs(); !strings.Contains(line, "隐藏 1 个") {
		t.Errorf("过滤掉工具时必须留一条可见/隐藏计数的日志，实际 = %q", line)
	}
}

// TestWarnThrottleCollapsesRepeats：这两条日志都由调用方触发，不限流会被刷满，
// 把真正要看的行冲掉。放行的那条必须带上被压制条数，否则看日志的人会低估规模。
func TestWarnThrottleCollapsesRepeats(t *testing.T) {
	logs := captureLog(t)
	w := &warnThrottle{interval: time.Hour}
	for i := 0; i < 5; i++ {
		w.warn(context.Background(), "重复告警 tool=%s", "demo.write")
	}
	if got := strings.Count(logs(), "重复告警"); got != 1 {
		t.Errorf("一小时窗口内应只打 1 条，实际 %d 条", got)
	}
	w.last = time.Now().Add(-2 * time.Hour) // 窗口过去
	w.warn(context.Background(), "重复告警 tool=%s", "demo.write")
	if line := logs(); !strings.Contains(line, "被压制 4 条") {
		t.Errorf("放行的那条必须带被压制条数，实际 = %q", line)
	}
}
