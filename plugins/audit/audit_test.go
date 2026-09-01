package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// resetSinks 清空全局落地函数，并在用例结束后再清一次：sinks 是包级状态，
// 用例之间不清会互相看到对方的事件。
func resetSinks(t *testing.T) {
	t.Helper()
	clean := func() {
		mu.Lock()
		sinks = nil
		redactor = nil
		mu.Unlock()
	}
	clean()
	t.Cleanup(clean)
}

// collect 注册一个把事件收进切片的落地函数，返回取事件的闭包。
func collect(t *testing.T) func() []Event {
	t.Helper()
	var got []Event
	OnEvent(func(e Event) error {
		got = append(got, e)
		return nil
	})
	return func() []Event { return got }
}

// okSection 返回一份规整化后的合法配置段。
func okSection(t *testing.T, s Section) Section {
	t.Helper()
	out, err := s.normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return out
}

// newMW 建一份中间件 + 它专属的投递器（用例之间不共享 worker）。
func newMW(t *testing.T, s Section) (runtime.Middleware, *dispatcher) {
	t.Helper()
	sec := okSection(t, s)
	d := newDispatcher(sec.QueueSize, sec.flush)
	t.Cleanup(d.stop)
	return middleware(sec, d), d
}

// syncMW 是 newMW 的「同步版」：每次调用返回后等到事件已被 worker 处理完，
// 用例才能像同步时代那样在调用后立刻断言事件内容。
//
// 等条件而不是 sleep 固定时长：固定 sleep 在快机器上白等、在慢机器或 -race 下假失败。
func syncMW(t *testing.T, s Section) runtime.Middleware {
	t.Helper()
	inner, d := newMW(t, s)
	return func(next runtime.Handler) runtime.Handler {
		h := inner(next)
		return func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
			res, err := h(ctx, c)
			waitDrained(t, d)
			return res, err
		}
	}
}

// waitDrained 等到已入队的事件全部被 worker 处理完（delivered+failed >= enqueued）。
//
// 读的是原子计数，与 worker 里调用 sink 的顺序有 happens-before 关系，
// 因此用例随后读 sink 收集到的切片是安全的（-race 下也不报）。
func waitDrained(t *testing.T, d *dispatcher) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s := d.stats()
		if s.Delivered+s.Failed >= s.Enqueued {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待审计事件落地超时: %+v", s)
		}
		time.Sleep(time.Millisecond)
	}
}

// syncBuffer 是并发安全的日志缓冲。
//
// 必须带锁：异步之后写日志的是 worker goroutine（落地失败、丢弃告警、退出收尾），
// 而用例在主 goroutine 读，裸 strings.Builder 在 -race 下必报，
// 且同包里别的用例遗留的 worker 也可能正往同一个全局 log 输出里写。
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLog 把标准库日志重定向到 buf，返回恢复函数。
func captureLog(buf *syncBuffer) func() {
	log.SetOutput(buf)
	return func() { log.SetOutput(os.Stderr) }
}

// TestAuditEventOnDeny：被内层插件拒掉的调用也必须留下事件，且带上拒绝方与原因。
func TestAuditEventOnDeny(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	mw := syncMW(t, Section{Headers: []string{"X-Tenant"}})
	c := &runtime.Call{
		Method:  "tools/call",
		Tool:    "a.write",
		LogID:   "lid-1",
		Args:    json.RawMessage(`{"n":1}`),
		Headers: http.Header{"X-Tenant": []string{"ps"}},
		Subject: &runtime.Subject{ID: "zhangsan", Token: "ops-agent"},
	}
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		time.Sleep(time.Millisecond)
		return runtime.DenyResult(c, "quota", "daily limit reached"), nil
	})
	if _, err := h(context.Background(), c); err != nil {
		t.Fatalf("handler: %v", err)
	}

	all := events()
	if len(all) != 1 {
		t.Fatalf("events = %d, want 1", len(all))
	}
	e := all[0]
	if e.Tool != "a.write" || e.Method != "tools/call" || e.LogID != "lid-1" {
		t.Errorf("event = %+v", e)
	}
	if e.Subject.ID != "zhangsan" || e.Subject.Token != "ops-agent" || !e.HasIdentity {
		t.Errorf("subject = %+v hasIdentity=%v", e.Subject, e.HasIdentity)
	}
	if e.DeniedBy != "quota" || e.DenyReason != "daily limit reached" {
		t.Errorf("deny info = %q/%q", e.DeniedBy, e.DenyReason)
	}
	if !e.IsError {
		t.Error("被拒的结果必须以 IsError 记录")
	}
	if e.Cost <= 0 {
		t.Errorf("cost = %v, want > 0", e.Cost)
	}
	if e.Args != `{"n":1}` {
		t.Errorf("args = %q", e.Args)
	}
	if e.Headers["X-Tenant"] != "ps" {
		t.Errorf("headers = %v", e.Headers)
	}
	if !strings.Contains(e.Result, "permission denied") {
		t.Errorf("result = %q，应是模型真正收到的内容", e.Result)
	}
}

// TestAuditSnapshotsSubjectAtEntry：audit 在链最外层，返回路径上 c.Subject（指针）
// 已可能被内层插件改过。事件必须用进入时的快照，否则审计记的是被改写后的身份。
func TestAuditSnapshotsSubjectAtEntry(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	mw := syncMW(t, Section{})
	sub := &runtime.Subject{ID: "zhangsan", Token: "ops-agent",
		Labels: map[string]string{"team": "core"}}
	c := &runtime.Call{Method: "tools/call", Tool: "a.write", Subject: sub,
		Labels: map[string]string{"risk": "high"}}
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		c.Subject.ID = "attacker"
		c.Subject.Token = "readonly-agent"
		c.Subject.Labels["team"] = "other"
		c.Labels["risk"] = "none"
		return runtime.TextResult("ok"), nil
	})
	if _, err := h(context.Background(), c); err != nil {
		t.Fatalf("handler: %v", err)
	}

	e := events()[0]
	if e.Subject.ID != "zhangsan" || e.Subject.Token != "ops-agent" {
		t.Errorf("subject 未取进入时快照: %+v", e.Subject)
	}
	if e.Subject.Labels["team"] != "core" {
		t.Errorf("subject labels 未取进入时快照: %v", e.Subject.Labels)
	}
	if e.Labels["risk"] != "high" {
		t.Errorf("工具 labels 未取进入时快照: %v", e.Labels)
	}
}

// TestAuditAnonymousSubjectIsDistinguishable：基座默认不信任客户端身份头，
// Subject.ID 常态为空。事件必须能区分「匿名/未配置身份」与真实身份，
// 不能让落地方把空串当成用户名写进审计。
func TestAuditAnonymousSubjectIsDistinguishable(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	mw := syncMW(t, Section{})
	c := &runtime.Call{Method: "tools/call", Tool: "a.read",
		Subject: &runtime.Subject{Token: "readonly-agent"}}
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("ok"), nil
	})
	if _, err := h(context.Background(), c); err != nil {
		t.Fatalf("handler: %v", err)
	}

	e := events()[0]
	if e.HasIdentity {
		t.Error("空 Subject.ID 必须记为无身份")
	}
	if e.ActorID() != AnonymousActor {
		t.Errorf("ActorID = %q, want %q", e.ActorID(), AnonymousActor)
	}
	if e.Subject.ID != "" {
		t.Errorf("Subject.ID 应保持原样为空，got %q", e.Subject.ID)
	}
}

// TestAuditTruncatesLargePayload：大 payload 必须按配置截断并标记，
// 否则一次几十兆的结果就能把审计后端（内存/磁盘）打满。
// 截断点还要落在 UTF-8 边界上，不能切出半个字符让落地方拿到非法字符串。
func TestAuditTruncatesLargePayload(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	mw := syncMW(t, Section{MaxArgsBytes: 10, MaxResultBytes: 12})
	c := &runtime.Call{Method: "tools/call", Tool: "a.read",
		// 每个「参」占 3 字节：截断点必然落在字符中间，用来验证边界处理。
		Args:    json.RawMessage(strings.Repeat("参", 20)),
		Subject: &runtime.Subject{Token: "ops-agent"}}
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult(strings.Repeat("果", 100)), nil
	})
	if _, err := h(context.Background(), c); err != nil {
		t.Fatalf("handler: %v", err)
	}

	e := events()[0]
	if !e.ArgsTruncated || !e.ResultTruncated {
		t.Errorf("truncated flags = %v/%v, want true/true", e.ArgsTruncated, e.ResultTruncated)
	}
	if len(e.Args) > 10 || len(e.Result) > 12 {
		t.Errorf("截断后长度 args=%d result=%d，超过配置上限", len(e.Args), len(e.Result))
	}
	if !utf8.ValidString(e.Args) || !utf8.ValidString(e.Result) {
		t.Errorf("截断结果必须是合法 UTF-8: args=%q result=%q", e.Args, e.Result)
	}
	if e.Args == "" || e.Result == "" {
		t.Errorf("截断不应把内容清空: args=%q result=%q", e.Args, e.Result)
	}
}

// TestSinkErrorNeverAffectsResult：落地失败不得改变调用结果——审计判定发生在返回路径上，
// 工具早已执行完，拦下结果挡不住任何副作用，只会让调用方以为要重试。
// 但失败不能静默：计入 Failed 并留日志。
func TestSinkErrorNeverAffectsResult(t *testing.T) {
	resetSinks(t)
	OnEvent(func(e Event) error { return errors.New("backend down") })

	var logged syncBuffer
	defer captureLog(&logged)()

	inner, d := newMW(t, Section{})
	h := inner(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("payload"), nil
	})
	res, err := h(context.Background(), &runtime.Call{Method: "tools/call", Tool: "a.read",
		LogID: "lid-fail", Subject: &runtime.Subject{Token: "ops-agent"}})
	if err != nil {
		t.Fatalf("落地失败不应让调用失败: %v", err)
	}
	if res == nil || res.Tool == nil {
		t.Fatal("落地失败不应吞掉结果")
	}
	waitDrained(t, d)
	if got := d.stats(); got.Failed != 1 || got.Delivered != 0 {
		t.Errorf("stats = %+v, want Failed=1 Delivered=0", got)
	}
	for _, want := range []string{"backend down", "lid-fail", "a.read"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("落地失败日志缺少 %q: %q", want, logged.String())
		}
	}
}

// TestSinkPanicNeverAffectsResult：落地函数 panic 既不能打穿请求路径、也不能吞掉，
// 且不影响其余落地函数。
func TestSinkPanicNeverAffectsResult(t *testing.T) {
	resetSinks(t)
	var second atomic.Bool
	OnEvent(func(e Event) error { panic("sink boom") })
	OnEvent(func(e Event) error { second.Store(true); return nil })

	inner, d := newMW(t, Section{})
	h := inner(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("ok"), nil
	})
	res, err := h(context.Background(), &runtime.Call{Method: "tools/call", Tool: "a.read",
		Subject: &runtime.Subject{Token: "ops-agent"}})
	if err != nil || res == nil {
		t.Fatalf("落地函数 panic 不得影响调用结果: res=%v err=%v", res, err)
	}
	waitDrained(t, d)
	if got := d.stats(); got.Failed != 1 {
		t.Errorf("panic 必须计成落地失败: %+v", got)
	}
	if !second.Load() {
		t.Error("一个落地函数 panic 不应阻止其余落地函数执行")
	}
}

// TestSlowSinkDoesNotBlockCall：这是异步化要买到的东西——落地方卡住时，
// 调用方照常拿到结果，不用等审计后端的 P99。
func TestSlowSinkDoesNotBlockCall(t *testing.T) {
	resetSinks(t)
	release := make(chan struct{})
	landed := make(chan struct{})
	OnEvent(func(e Event) error {
		<-release
		close(landed)
		return nil
	})

	inner, _ := newMW(t, Section{})
	h := inner(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("ok"), nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := h(context.Background(), &runtime.Call{Method: "tools/call", Tool: "a.read",
			Subject: &runtime.Subject{Token: "ops-agent"}}); err != nil {
			t.Errorf("handler: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("落地函数卡住时调用被一起挂住了：投递不是异步的")
	}
	close(release)
	select {
	case <-landed:
	case <-time.After(3 * time.Second):
		t.Fatal("放开落地函数后事件仍未落地")
	}
}

// TestQueueFullDropsAndCounts：队列打满时丢事件是**已知代价**，但必须可观测——
// 计数 + 告警日志，不能静默。丢的是当前这条（丢新不丢旧），已入队的时序不被打乱。
func TestQueueFullDropsAndCounts(t *testing.T) {
	resetSinks(t)
	release := make(chan struct{})
	var landed atomic.Int64
	OnEvent(func(e Event) error {
		<-release
		landed.Add(1)
		return nil
	})

	var logged syncBuffer
	defer captureLog(&logged)()

	// 队列容量 1 + 落地函数卡住：第 1 条被 worker 取走并卡在 sink 里，第 2 条占满队列，
	// 之后的必然被丢。
	inner, d := newMW(t, Section{QueueSize: 1})
	h := inner(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("ok"), nil
	})
	const calls = 20
	for i := 0; i < calls; i++ {
		res, err := h(context.Background(), &runtime.Call{Method: "tools/call", Tool: "a.read",
			LogID: fmt.Sprintf("lid-%d", i), Subject: &runtime.Subject{Token: "ops-agent"}})
		if err != nil || res == nil {
			t.Fatalf("第 %d 次调用被审计影响了: res=%v err=%v", i, res, err)
		}
	}
	got := d.stats()
	if got.Dropped == 0 {
		t.Fatalf("队列满时必须丢弃并计数: %+v", got)
	}
	if got.Enqueued+got.Dropped != calls {
		t.Errorf("每次调用都要么入队要么计成丢弃: %+v (calls=%d)", got, calls)
	}
	if !strings.Contains(logged.String(), "事件队列已满") {
		t.Errorf("丢弃必须有告警日志: %q", logged.String())
	}
	close(release)
}

// TestDropAlarmIsThrottled：丢弃告警按类限流，但必须带上累计条数——
// 持续性故障下逐条打会按 QPS 刷满磁盘，完全不打则「丢了多少」无从得知。
func TestDropAlarmIsThrottled(t *testing.T) {
	var logged syncBuffer
	defer captureLog(&logged)()

	d := &dispatcher{dropAlarm: &throttledLogger{interval: time.Hour}}
	for i := 0; i < 5; i++ {
		d.drop(Event{LogID: "lid"}, "测试丢弃")
	}
	if n := strings.Count(logged.String(), "审计事件被丢弃"); n != 1 {
		t.Errorf("限流后应只打 1 条，实际 %d 条: %q", n, logged.String())
	}
	if got := d.dropped.Load(); got != 5 {
		t.Errorf("被限流的日志也要计数: dropped=%d, want 5", got)
	}
}

// TestStopFlushesQueue：进程退出时队列里剩的事件要尽量落地——
// 不排空等于每次发布都稳定丢掉最后一批记录。
func TestStopFlushesQueue(t *testing.T) {
	resetSinks(t)
	release := make(chan struct{})
	var landed atomic.Int64
	OnEvent(func(e Event) error {
		<-release
		landed.Add(1)
		return nil
	})

	inner, d := newMW(t, Section{})
	h := inner(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("ok"), nil
	})
	const calls = 5
	for i := 0; i < calls; i++ {
		if _, err := h(context.Background(), &runtime.Call{Method: "tools/call", Tool: "a.read",
			Subject: &runtime.Subject{Token: "ops-agent"}}); err != nil {
			t.Fatalf("handler: %v", err)
		}
	}
	close(release) // 放开落地函数，stop 应能在预算内把队列排干
	d.stop()
	if got := landed.Load(); got != calls {
		t.Errorf("stop 应排空队列: 已落地 %d 条, want %d（stats=%+v）", got, calls, d.stats())
	}
}

// TestStopIsBoundedByFlushTimeout：落地函数卡死时 stop 必须在预算内返回，
// 否则一个连不上的远端就能把整个进程的停止流程挂住。
func TestStopIsBoundedByFlushTimeout(t *testing.T) {
	resetSinks(t)
	stuck := make(chan struct{})
	OnEvent(func(e Event) error { <-stuck; return nil })

	inner, d := newMW(t, Section{FlushTimeout: "50ms"})
	h := inner(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("ok"), nil
	})
	for i := 0; i < 3; i++ {
		if _, err := h(context.Background(), &runtime.Call{Method: "tools/call", Tool: "a.read",
			Subject: &runtime.Subject{Token: "ops-agent"}}); err != nil {
			t.Fatalf("handler: %v", err)
		}
	}
	start := time.Now()
	d.stop()
	if cost := time.Since(start); cost > 2*time.Second {
		t.Errorf("stop 耗时 %v：卡死的落地函数把停止流程挂住了", cost)
	}
	// 放开落地函数并等 worker 真正退出。不等的话它会在后续用例运行期间苏醒，
	// 把这批事件投给别的用例注册的 sink（同包用例共享全局 sinks），造成串扰。
	close(stuck)
	select {
	case <-d.done:
	case <-time.After(3 * time.Second):
		t.Fatal("放开落地函数后 worker 仍未退出")
	}
}

// TestStopDropsNewEvents：停止之后仍在途的调用不能阻塞、也不能 panic
// （向已关闭 channel 发送）；事件按丢弃计数。
func TestStopDropsNewEvents(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	inner, d := newMW(t, Section{})
	h := inner(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("ok"), nil
	})
	d.stop()
	if _, err := h(context.Background(), &runtime.Call{Method: "tools/call", Tool: "a.read",
		Subject: &runtime.Subject{Token: "ops-agent"}}); err != nil {
		t.Fatalf("停止之后的调用不应报错: %v", err)
	}
	if got := d.stats(); got.Dropped != 1 || got.Enqueued != 0 {
		t.Errorf("停止后的事件应计成丢弃: %+v", got)
	}
	if len(events()) != 0 {
		t.Errorf("停止之后不应再落地: %d 条", len(events()))
	}
}

// TestAuditMultipleSinks：可注册多个落地函数，全部都要收到事件。
func TestAuditMultipleSinks(t *testing.T) {
	resetSinks(t)
	var n int
	OnEvent(func(e Event) error { n++; return nil })
	OnEvent(func(e Event) error { n++; return nil })

	mw := syncMW(t, Section{})
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("ok"), nil
	})
	if _, err := h(context.Background(), &runtime.Call{Method: "tools/call",
		Subject: &runtime.Subject{Token: "ops-agent"}}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if n != 2 {
		t.Errorf("sinks invoked = %d, want 2", n)
	}
}

// TestAuditRecordsChainError：链上抛出的协议级错误也必须留下事件，
// 且原错误照常上抛（审计不改变链的语义）。
func TestAuditRecordsChainError(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	want := errors.New("tool exploded")
	mw := syncMW(t, Section{})
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return nil, want
	})
	_, err := h(context.Background(), &runtime.Call{Method: "tools/call", Tool: "a.write",
		Subject: &runtime.Subject{Token: "ops-agent"}})
	if !errors.Is(err, want) {
		t.Fatalf("原错误必须原样上抛: %v", err)
	}
	e := events()[0]
	if e.Err != want.Error() {
		t.Errorf("event.Err = %q, want %q", e.Err, want.Error())
	}
}

// TestAuditAuditsNonToolCallMethods：tools/list 等方法同样要留痕，
// 且结果必须原样透传（不能被改写成 tools/call 形状）。
func TestAuditAuditsNonToolCallMethods(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	mw := syncMW(t, Section{})
	list := runtime.ListResult(nil)
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return list, nil
	})
	res, err := h(context.Background(), &runtime.Call{Method: "tools/list",
		Subject: &runtime.Subject{Token: "ops-agent"}})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res != list {
		t.Error("非 tools/call 的结果必须原样透传")
	}
	if got := events()[0].Method; got != "tools/list" {
		t.Errorf("method = %q", got)
	}
}

// TestSectionNormalizeDefaults：默认值必须齐全且安全——队列容量/排空预算非零，
// 截断上限默认非零（0 会被当成「不截断」，等于没有防护）。
func TestSectionNormalizeDefaults(t *testing.T) {
	sec, err := Section{}.normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if sec.QueueSize != defaultQueueSize {
		t.Errorf("queue_size 默认 = %d, want %d", sec.QueueSize, defaultQueueSize)
	}
	if sec.flush != defaultFlushTimeout {
		t.Errorf("flush_timeout 默认 = %v, want %v", sec.flush, defaultFlushTimeout)
	}
	if sec.MaxArgsBytes <= 0 || sec.MaxResultBytes <= 0 {
		t.Errorf("截断上限默认值必须 > 0: %+v", sec)
	}
}

// TestSectionNormalizeParsesFlushTimeout：配了 duration 串就必须真的生效，
// 不能解析完丢掉（那会让「改小停止预算」的部署静默无效）。
func TestSectionNormalizeParsesFlushTimeout(t *testing.T) {
	sec, err := Section{FlushTimeout: "750ms"}.normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if sec.flush != 750*time.Millisecond {
		t.Errorf("flush = %v, want 750ms", sec.flush)
	}
}

// TestSectionNormalizeRejectsBadConfig：写错的配置必须启动失败，不能静默用默认值。
func TestSectionNormalizeRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		sec  Section
		want string
	}{
		{"negative queue size", Section{QueueSize: -1}, "queue_size"},
		{"bad flush timeout", Section{FlushTimeout: "soon"}, "flush_timeout"},
		{"zero flush timeout", Section{FlushTimeout: "0s"}, "flush_timeout"},
		{"negative flush timeout", Section{FlushTimeout: "-1s"}, "flush_timeout"},
		{"negative args limit", Section{MaxArgsBytes: -1}, "max_args_bytes"},
		{"negative result limit", Section{MaxResultBytes: -1}, "max_result_bytes"},
		{"credential header", Section{Headers: []string{"authorization"}}, "凭据"},
		{"cookie header", Section{Headers: []string{"Cookie"}}, "凭据"},
		{"blank header", Section{Headers: []string{"  "}}, "headers"},
	}
	for _, c := range cases {
		_, err := c.sec.normalize()
		if err == nil {
			t.Errorf("%s: 必须报错", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 错误信息 %v 里应包含 %q", c.name, err, c.want)
		}
	}
}

// writeConfig 写一份临时 TOML，返回路径。
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.toml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const baseTokens = `
[[tokens]]
token = "t-ops"
name = "ops-agent"
applicant = "tester"
allow = ["capability=read"]
`

// TestInstallClaimsFullSection：文档承诺的每个字段都必须被结构体认领，
// 否则部署方照文档把配置写全反而启动失败（基座报「无人认领的配置项」）。
func TestInstallClaimsFullSection(t *testing.T) {
	resetSinks(t)
	OnEvent(func(e Event) error { return nil }) // 接流前必须有 sink
	path := writeConfig(t, `required_plugins = ["audit"]
`+baseTokens+`
[audit]
headers = ["X-Tenant", "User-Agent"]
queue_size = 128
flush_timeout = "1s"
max_args_bytes = 512
max_result_bytes = 4096
`)
	r := runtime.New(runtime.Config{ConfigPath: path}, nil)
	if err := Install(r); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, _, err := r.Handlers(); err != nil {
		t.Fatalf("配置全字段写齐时必须能启动: %v", err)
	}
}

// TestInstallLeavesTypoUnclaimed：段内字段名拼错时基座必须启动失败——
// 「插件按默认值上线」没有任何运行期信号。
func TestInstallLeavesTypoUnclaimed(t *testing.T) {
	resetSinks(t)
	OnEvent(func(e Event) error { return nil }) // 接流前必须有 sink
	path := writeConfig(t, baseTokens+`
[audit]
queue_sze = 128
`)
	r := runtime.New(runtime.Config{ConfigPath: path}, nil)
	if err := Install(r); err != nil {
		t.Fatalf("Install: %v", err)
	}
	_, _, err := r.Handlers()
	if err == nil {
		t.Fatal("拼错的配置项必须让启动失败")
	}
	if !strings.Contains(err.Error(), "audit.queue_sze") {
		t.Errorf("错误里应点出拼错的键: %v", err)
	}
}

// TestInstallWithoutSection：没写 [audit] 段时用默认值安装，并在启动日志里声明策略。
func TestInstallWithoutSection(t *testing.T) {
	resetSinks(t)
	OnEvent(func(e Event) error { return nil }) // 接流前必须有 sink
	var logged syncBuffer
	restore := captureLog(&logged)
	defer restore()

	r := runtime.New(runtime.Config{ConfigPath: writeConfig(t, baseTokens)}, nil)
	if err := Install(r); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, _, err := r.Handlers(); err != nil {
		t.Fatalf("Handlers: %v", err)
	}
	// 「异步、不阻塞」是部署方最需要确认的一项：审计不再是留痕的强保证。
	for _, want := range []string{"异步投递", "queue_size=", "flush_timeout="} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("启动日志缺少 %q: %q", want, logged.String())
		}
	}
}

// TestInstallRejectsBadSection：坏配置在 Install 阶段就报错，宿主据此中止启动。
func TestInstallRejectsBadSection(t *testing.T) {
	resetSinks(t)
	path := writeConfig(t, baseTokens+`
[audit]
flush_timeout = "soon"
`)
	r := runtime.New(runtime.Config{ConfigPath: path}, nil)
	if err := Install(r); err == nil {
		t.Fatal("非法 flush_timeout 必须让 Install 失败")
	}
}

// TestReadStatsExposesCounters：宿主要能把投递计数接进监控——
// 异步会丢事件，不暴露计数「尽力而为」就只是一句免责声明。
// 停止后返回零值而不是一组不再增长的旧数（监控上看到的是「没在跑」）。
func TestReadStatsExposesCounters(t *testing.T) {
	resetSinks(t)
	var landed atomic.Int64
	OnEvent(func(e Event) error { landed.Add(1); return nil })
	r := runtime.New(runtime.Config{ConfigPath: writeConfig(t, baseTokens)}, nil)
	if err := Install(r); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, _, err := r.Handlers(); err != nil {
		t.Fatalf("Handlers: %v", err)
	}
	d := current.Load()
	if d == nil {
		t.Fatal("Install 之后宿主必须能拿到计数入口")
	}
	d.enqueue(Event{LogID: "lid-stats", Method: "ping"})
	waitDrained(t, d)
	if got := ReadStats(); got.Enqueued != 1 || got.Delivered != 1 {
		t.Errorf("ReadStats = %+v, want Enqueued=1 Delivered=1", got)
	}
	r.RunStop(context.Background())
	if got := ReadStats(); got != (Stats{}) {
		t.Errorf("停止后 ReadStats = %+v, want 零值", got)
	}
	if landed.Load() != 1 {
		t.Errorf("事件应已落地一次，实际 %d", landed.Load())
	}
}

// TestEventNeverCarriesCredentials：事件里不得出现凭据——Call.Headers 已被基座剔除
// Authorization/Cookie，插件也不得从别处补回来。
func TestEventNeverCarriesCredentials(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	mw := syncMW(t, Section{Headers: []string{"X-Tenant"}})
	c := &runtime.Call{Method: "tools/call", Tool: "a.read",
		Headers: http.Header{"Authorization": []string{"Bearer super-secret"}},
		Subject: &runtime.Subject{Token: "ops-agent"}}
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("ok"), nil
	})
	if _, err := h(context.Background(), c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	for k, v := range events()[0].Headers {
		if strings.Contains(v, "super-secret") {
			t.Errorf("事件里出现了凭据: %s=%s", k, v)
		}
	}
}

// TestNoSinkAtRuntimeCountsAsFailure：接流之后 sink 被清空时，事件确实没落地——
// 必须计成 Failed 并告警，而不是当作成功（启动期那道校验只堵得住启动之前的一半）。
func TestNoSinkAtRuntimeCountsAsFailure(t *testing.T) {
	resetSinks(t)

	var logged syncBuffer
	defer captureLog(&logged)()

	inner, d := newMW(t, Section{})
	h := inner(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("payload"), nil
	})
	res, err := h(context.Background(), &runtime.Call{Method: "tools/call", Tool: "a.write",
		Subject: &runtime.Subject{Token: "ops-agent"}})
	if err != nil || res == nil {
		t.Fatalf("没有 sink 也不得影响调用结果: res=%v err=%v", res, err)
	}
	waitDrained(t, d)
	if got := d.stats(); got.Failed != 1 {
		t.Errorf("无落地函数必须计成失败: %+v", got)
	}
	if !strings.Contains(logged.String(), "没有任何落地函数") {
		t.Errorf("无落地函数必须告警: %q", logged.String())
	}
}

// TestInstallRequiresSink：把「装了审计插件却一条也不落」堵在**接流之前**——
// required_plugins 只校验插件在场，注册不了 sink 这件事只有这里能拦。
//
// 判据挂在 Handlers()（基座的 build 期钩子）而不是 Install：Install 只是安装动作，
// 那时 sink 允许还没注册（见 TestSinkRegisteredAfterInstallStillStarts）。
func TestInstallRequiresSink(t *testing.T) {
	cases := []struct {
		name    string
		sink    bool
		wantErr bool
	}{
		{"without sink", false, true},
		{"with sink", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetSinks(t)
			if c.sink {
				OnEvent(func(e Event) error { return nil })
			}
			r := runtime.New(runtime.Config{ConfigPath: writeConfig(t, baseTokens)}, nil)
			if err := Install(r); err != nil {
				t.Fatalf("Install: %v", err)
			}
			_, _, err := r.Handlers()
			if c.wantErr && err == nil {
				t.Fatal("没有 sink 必须让启动失败")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("Handlers: %v", err)
			}
			if c.wantErr && !strings.Contains(err.Error(), "OnEvent") {
				t.Errorf("错误应指出修法（注册 OnEvent）: %v", err)
			}
		})
	}
}

// TestSinkRegisteredAfterInstallStillStarts：落地函数在 Install **之后**、开始接流
// **之前**注册是合法用法（宿主常把 OnEvent 与日志/DB 初始化写在一起），不得被误判成
// 启动失败——这正是把校验从 Install 挪到基座 build 期钩子要解掉的假阳性。
func TestSinkRegisteredAfterInstallStillStarts(t *testing.T) {
	resetSinks(t)
	r := runtime.New(runtime.Config{ConfigPath: writeConfig(t, baseTokens)}, nil)
	if err := Install(r); err != nil {
		t.Fatalf("Install: %v", err)
	}
	OnEvent(func(e Event) error { return nil })
	if _, _, err := r.Handlers(); err != nil {
		t.Fatalf("Install 之后注册 sink 也必须能启动: %v", err)
	}
}

// TestAuditDoesNotExpandLargePayload 覆盖 I2：截断必须发生在整串复制/整体序列化之前。
// 8MB 入参 + 8MB 结果、上限各 16 字节，一次调用的新增分配必须远小于 payload 本身；
// 否则「截断」只保护了下游后端，进程内存照样被打满。
func TestAuditDoesNotExpandLargePayload(t *testing.T) {
	resetSinks(t)
	var got Event
	OnEvent(func(e Event) error { got = e; return nil })

	const size = 8 << 20
	payload := strings.Repeat("a", size)
	c := &runtime.Call{Method: "tools/call", Tool: "a.read",
		Args:    json.RawMessage(payload),
		Subject: &runtime.Subject{Token: "ops-agent"}}
	res := runtime.TextResult(payload)
	mw := syncMW(t, Section{MaxArgsBytes: 16, MaxResultBytes: 16})
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return res, nil
	})

	var before, after goruntime.MemStats
	goruntime.GC()
	goruntime.ReadMemStats(&before)
	if _, err := h(context.Background(), c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	goruntime.ReadMemStats(&after)

	const budget = 1 << 20
	if delta := after.TotalAlloc - before.TotalAlloc; delta > budget {
		t.Errorf("单次调用新增分配 %d 字节，超过 %d：截断发生在整串展开之后", delta, budget)
	}
	if len(got.Args) > 16 || len(got.Result) > 16 {
		t.Errorf("截断上限未生效: args=%d result=%d", len(got.Args), len(got.Result))
	}
	if !got.ArgsTruncated || !got.ResultTruncated {
		t.Errorf("truncated flags = %v/%v, want true/true", got.ArgsTruncated, got.ResultTruncated)
	}
	// 原结果不得被改写：它是模型真正要收到的那份内容。
	if tc, ok := res.Tool.Content[0].(*mcp.TextContent); !ok || len(tc.Text) != size {
		t.Error("裁剪结果内容时改到了调用方的结果")
	}
}

// TestTruncateKeepsContentOnInvalidUTF8 覆盖 I3：入参含非法 UTF-8 字节时，
// 只回退尾部那个被切断的字符，不能把整段可读内容削成空串。
func TestTruncateKeepsContentOnInvalidUTF8(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	// Call.Args 是未校验的原始 JSON 字节：二进制/坏编码入参完全可能出现。
	args := append([]byte{0xff}, []byte(strings.Repeat("A", 200))...)
	mw := syncMW(t, Section{MaxArgsBytes: 64})
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("ok"), nil
	})
	if _, err := h(context.Background(), &runtime.Call{Method: "tools/call", Tool: "a.read",
		Args: args, Subject: &runtime.Subject{Token: "ops-agent"}}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	e := events()[0]
	if strings.Count(e.Args, "A") < 60 {
		t.Errorf("可读内容被削掉了: args=%q (len=%d)", e.Args, len(e.Args))
	}
	if !e.ArgsTruncated {
		t.Error("确实截断了就要标记")
	}
}

// TestTrimPartialRune 是 I3 的边界表：只丢尾部不完整字符，中段非法字节原样保留。
func TestTrimPartialRune(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"cut inside rune", strings.Repeat("参", 4), 10, "参参参"},
		{"leading invalid byte kept", "\xffABCDEF", 4, "\xffABC"},
		{"middle invalid byte kept", "AB\xffCDEF", 5, "AB\xffCD"},
		{"ascii boundary", "ABCDEF", 3, "ABC"},
	}
	for _, c := range cases {
		got, cut := truncateString(c.in, c.n)
		if got != c.want || !cut {
			t.Errorf("%s: truncateString(%q,%d) = %q,%v want %q,true",
				c.name, c.in, c.n, got, cut, c.want)
		}
	}
}

// TestSinkPanicLogsStack 覆盖 I4：panic 现场要有堆栈与定位信息进日志，
// 否则落地函数里的真实 bug 只剩一行没有出处的文本。
func TestSinkPanicLogsStack(t *testing.T) {
	resetSinks(t)
	OnEvent(func(e Event) error { panic("sink boom") })

	var logged syncBuffer
	restore := captureLog(&logged)
	defer restore()

	mw := syncMW(t, Section{})
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("ok"), nil
	})
	if _, err := h(context.Background(), &runtime.Call{Method: "tools/call", Tool: "a.read",
		LogID: "lid-panic", Subject: &runtime.Subject{Token: "ops-agent"}}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	out := logged.String()
	for _, want := range []string{"sink boom", "lid-panic", "a.read", "goroutine"} {
		if !strings.Contains(out, want) {
			t.Errorf("panic 日志缺少 %q: %q", want, out)
		}
	}
}

// TestArgsRedactor 覆盖 I5：入参脱敏钩子生效，且不得改写真实请求字节。
func TestArgsRedactor(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	var sawTool string
	SetArgsRedactor(func(tool string, args []byte) []byte {
		sawTool = tool
		return []byte(`{"password":"***"}`)
	})

	raw := []byte(`{"password":"hunter2"}`)
	c := &runtime.Call{Method: "tools/call", Tool: "auth.login",
		Args: raw, Subject: &runtime.Subject{Token: "ops-agent"}}
	mw := syncMW(t, Section{})
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("ok"), nil
	})
	if _, err := h(context.Background(), c); err != nil {
		t.Fatalf("handler: %v", err)
	}

	e := events()[0]
	if strings.Contains(e.Args, "hunter2") {
		t.Errorf("脱敏钩子未生效: %q", e.Args)
	}
	if !strings.Contains(e.Args, "***") {
		t.Errorf("事件里应是脱敏后的入参: %q", e.Args)
	}
	if sawTool != "auth.login" {
		t.Errorf("钩子应收到工具名，got %q", sawTool)
	}
	if string(raw) != `{"password":"hunter2"}` {
		t.Errorf("脱敏不得改写真实请求字节: %q", raw)
	}
}

// TestListResultRecordsToolNames 覆盖 M4：tools/list 也要记「看见了哪些工具」，
// 这是权限审计的判据；原来这条分支什么都不记。
func TestListResultRecordsToolNames(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	list := runtime.ListResult(&mcp.ListToolsResult{Tools: []*mcp.Tool{
		{Name: "a.read"}, {Name: "b.write"}, nil,
	}})
	mw := syncMW(t, Section{})
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return list, nil
	})
	if _, err := h(context.Background(), &runtime.Call{Method: "tools/list",
		Subject: &runtime.Subject{Token: "ops-agent"}}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	e := events()[0]
	for _, want := range []string{"a.read", "b.write"} {
		if !strings.Contains(e.Result, want) {
			t.Errorf("tools/list 事件应记下可见工具名 %q: %q", want, e.Result)
		}
	}
}

// TestResultErrOnMarshalFailure 覆盖 M6：结果序列化失败要留下 ResultErr，
// 不能变成一个空 Result 让人以为工具返回了空。
func TestResultErrOnMarshalFailure(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	// Meta 里放一个 json 不认识的值，让 (*TextContent).MarshalJSON 真的失败。
	bad := &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: "hi", Meta: mcp.Meta{"bad": make(chan int)}},
	}}
	mw := syncMW(t, Section{})
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return &runtime.Result{Tool: bad}, nil
	})
	if _, err := h(context.Background(), &runtime.Call{Method: "tools/call", Tool: "a.read",
		Subject: &runtime.Subject{Token: "ops-agent"}}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	e := events()[0]
	if e.ResultErr == "" {
		t.Error("序列化失败必须记进 ResultErr")
	}
	if e.Result != "" {
		t.Errorf("序列化失败时 Result 应为空，got %q", e.Result)
	}
}

// TestPickHeadersMissingHeader 覆盖 M6：配了但请求里没有的头记 "-"，
// 与「没配这个头」（字段不存在）区分开。
func TestPickHeadersMissingHeader(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	mw := syncMW(t, Section{Headers: []string{"X-Tenant", "X-Absent"}})
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("ok"), nil
	})
	c := &runtime.Call{Method: "tools/call", Tool: "a.read",
		Headers: http.Header{"X-Tenant": []string{"ps"}},
		Subject: &runtime.Subject{Token: "ops-agent"}}
	if _, err := h(context.Background(), c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	e := events()[0]
	if e.Headers["X-Absent"] != "-" {
		t.Errorf("缺失的头应记 \"-\"，got %q", e.Headers["X-Absent"])
	}
	if _, ok := e.Headers["X-Other"]; ok {
		t.Error("没配的头不应出现在事件里")
	}
}

// TestNilSubjectFallback 覆盖 M6：Call.Subject 为 nil（自定义链/未认证路径）
// 不能 panic，且按匿名处理。
func TestNilSubjectFallback(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	mw := syncMW(t, Section{})
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.TextResult("ok"), nil
	})
	if _, err := h(context.Background(), &runtime.Call{Method: "ping"}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	e := events()[0]
	if e.HasIdentity || e.ActorID() != AnonymousActor || e.Subject.Token != "" {
		t.Errorf("nil Subject 应记为匿名: %+v", e.Subject)
	}
}

// TestArgsRedactorCannotCorruptRealArgs 覆盖 N1：脱敏钩子就地涂改传入切片时，
// 既不能改到调用方持有的字节，也不能改到内层插件看到的 c.Args——
// Call.Args 与 SDK 的 CallToolParamsRaw.Arguments 共享底层数组，而工具入参的
// 反序列化发生在链终点（钩子之后），一个写错的钩子否则会让线上工具收到被涂改的参数。
func TestArgsRedactorCannotCorruptRealArgs(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	SetArgsRedactor(func(tool string, args []byte) []byte {
		for i := range args { // 恶意/写错的钩子：原地涂空格
			args[i] = ' '
		}
		return []byte(`{"redacted":true}`)
	})

	const want = `{"password":"hunter2"}`
	raw := []byte(want)
	c := &runtime.Call{Method: "tools/call", Tool: "auth.login",
		Args: raw, Subject: &runtime.Subject{Token: "ops-agent"}}
	var innerSaw string
	mw := syncMW(t, Section{})
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		innerSaw = string(c.Args) // 链终点看到的才是真正会被反序列化的字节
		return runtime.TextResult("ok"), nil
	})
	if _, err := h(context.Background(), c); err != nil {
		t.Fatalf("handler: %v", err)
	}

	if string(raw) != want {
		t.Errorf("调用方持有的字节被钩子改坏了: %q", raw)
	}
	if innerSaw != want {
		t.Errorf("内层插件/工具看到的入参被钩子改坏了: %q", innerSaw)
	}
	if got := events()[0].Args; got != `{"redacted":true}` {
		t.Errorf("事件里应是钩子返回的内容: %q", got)
	}
}

// TestListResultStaysValidJSON 覆盖 N3：tools/list 分支按个数裁剪，
// Result 必须始终是可以直接 Unmarshal 的合法 JSON，且不超过上限。
func TestListResultStaysValidJSON(t *testing.T) {
	resetSinks(t)
	events := collect(t)

	tools := make([]*mcp.Tool, 0, 5000)
	for i := 0; i < 5000; i++ {
		tools = append(tools, &mcp.Tool{Name: fmt.Sprintf("pkg%04d.%s", i, strings.Repeat("n", 50))})
	}
	const max = 200
	mw := syncMW(t, Section{MaxResultBytes: max})
	h := mw(func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return runtime.ListResult(&mcp.ListToolsResult{Tools: tools}), nil
	})
	if _, err := h(context.Background(), &runtime.Call{Method: "tools/list",
		Subject: &runtime.Subject{Token: "ops-agent"}}); err != nil {
		t.Fatalf("handler: %v", err)
	}

	e := events()[0]
	if !json.Valid([]byte(e.Result)) {
		t.Fatalf("tools/list 的 Result 必须是合法 JSON: %q", e.Result)
	}
	var names []string
	if err := json.Unmarshal([]byte(e.Result), &names); err != nil {
		t.Fatalf("落地方应能直接 Unmarshal: %v", err)
	}
	if len(names) == 0 || len(names) >= len(tools) {
		t.Errorf("应保留一部分工具名，got %d/%d", len(names), len(tools))
	}
	if !e.ResultTruncated {
		t.Error("裁掉了工具名就要标记 truncated")
	}
	if len(e.Result) > max {
		t.Errorf("Result 长度 %d 超过上限 %d", len(e.Result), max)
	}
}

// TestJSONStringLenMatchesMarshal 是 N3 的预算基准：逐字节估算必须与
// json.Marshal 的实际长度完全一致，否则按个数裁剪会溢出上限。
func TestJSONStringLenMatchesMarshal(t *testing.T) {
	cases := []string{
		"", "plain.tool", `has"quote`, `back\slash`, "tab\tnew\nret\r",
		"ctrl\x01char", "html<>&amp", "中文工具名", "\u2028\u2029",
		"invalid\xffbyte", "valid\ufffdreplacement", strings.Repeat("n", 100),
	}
	for _, s := range cases {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %q: %v", s, err)
		}
		if got := jsonStringLen(s); got != len(b) {
			t.Errorf("jsonStringLen(%q) = %d, json.Marshal 长度 = %d (%s)", s, got, len(b), b)
		}
	}
}

// metaCall 造一次带 Meta 写入的 tools/call，返回事件里的 Meta 快照。
func metaCall(t *testing.T, write func(c *runtime.Call)) Event {
	t.Helper()
	resetSinks(t)
	events := collect(t)
	h := syncMW(t, Section{})(
		func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
			write(c)
			return runtime.TextResult("ok"), nil
		})
	c := &runtime.Call{Method: "tools/call", Tool: "demo.tool"}
	if _, err := h(context.Background(), c); err != nil {
		t.Fatalf("链上报错: %v", err)
	}
	all := events()
	if len(all) != 1 {
		t.Fatalf("events = %d, want 1", len(all))
	}
	return all[0]
}

// TestEventCarriesCallMeta：内层插件写进 Call.Meta 的任意键都要原样透出。
// audit 不认识 confirm、quota 这些插件，这条通用透传是外置插件把自己的上下文
// 送进审计流的唯一通道。
func TestEventCarriesCallMeta(t *testing.T) {
	got := metaCall(t, func(c *runtime.Call) { c.SetMeta("confirm.approver", "zhangsan") })
	if got.Meta["confirm.approver"] != "zhangsan" {
		t.Errorf("Event.Meta 没有透出自定义键，实际 %v", got.Meta)
	}
}

// TestEventMetaCapsKeysDeterministically：33 个键超出 maxMetaKeys=32，必须按键名
// 排序取前 32 个。排序是硬要求——否则同一次调用在不同进程里截出不同的键，对账时各说各话。
func TestEventMetaCapsKeysDeterministically(t *testing.T) {
	write := func(c *runtime.Call) {
		for n := 0; n < 33; n++ {
			c.SetMeta(fmt.Sprintf("k%02d", n), n)
		}
	}
	first := metaCall(t, write).Meta
	second := metaCall(t, write).Meta
	if len(first) != maxMetaKeys {
		t.Errorf("键数没有被截到 %d，实际 %d", maxMetaKeys, len(first))
	}
	if _, bad := first["k32"]; bad {
		t.Error("截断没有按键名排序：k32 不该留下")
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("两次运行截出的键不同，截断不确定：%v vs %v", first, second)
	}
}

// TestEventMetaTruncatesLongValue：单个值按 maxMetaValueBytes 截断——审计后端要能
// 直接写库，一个几 MB 的 Meta 值不该靠落地方去防。
func TestEventMetaTruncatesLongValue(t *testing.T) {
	long := strings.Repeat("x", maxMetaValueBytes*2)
	got := metaCall(t, func(c *runtime.Call) { c.SetMeta("big", long) })
	if len(got.Meta["big"]) != maxMetaValueBytes {
		t.Errorf("值长 %d，期望截到 %d", len(got.Meta["big"]), maxMetaValueBytes)
	}
}

// TestEventMetaNilWhenNoPluginWrote：没有插件写 Meta 时不要造一个空 map，
// 落地方据此区分「没人写」与「写了空值」。
func TestEventMetaNilWhenNoPluginWrote(t *testing.T) {
	got := metaCall(t, func(c *runtime.Call) {})
	if got.Meta != nil {
		t.Errorf("没有插件写 Meta 时应为 nil，实际 %v", got.Meta)
	}
}
