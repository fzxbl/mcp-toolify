package spill

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// newTestStore 造一个临时目录上的 store，用例结束时停 GC。
func newTestStore(t *testing.T, ttl, gcEvery time.Duration) *store {
	t.Helper()
	st, err := newStore(t.TempDir(), ttl, gcEvery, quota{})
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(st.close)
	return st
}

// withBaseURL 设置对外基础地址（进程级全局），用例结束后恢复。
// 下载 URL 必须在**每次**落盘时现取：PublicBaseURL 由基座在 Start 里设置，
// 那时插件的 Install 早就跑完了。
func withBaseURL(t *testing.T, base string) {
	t.Helper()
	old := runtime.PublicBaseURL()
	runtime.SetPublicBaseURL(base)
	t.Cleanup(func() { runtime.SetPublicBaseURL(old) })
}

// captureLog 把标准库日志重定向到 buf，返回恢复函数。
func captureLog(buf *strings.Builder) func() {
	log.SetOutput(buf)
	return func() { log.SetOutput(os.Stderr) }
}

// okOptions 返回规整化后的合法配置。
func okOptions(t *testing.T, s Section) options {
	t.Helper()
	o, err := s.normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return o
}

// testOwner 是单测里统一的属主：token 用途名 ops-agent、无身份
// （基座默认不信任身份头，这就是常态形状）。
var testOwner = fileOwner{Token: "ops-agent"}

// textCall 造一条 tools/call 的 Call，带上调用主体（落盘要记属主）。
func textCall(tool string) *runtime.Call {
	return &runtime.Call{Method: "tools/call", Tool: tool,
		Subject: &runtime.Subject{Token: testOwner.Token}}
}

// ownedRequest 造一条带调用主体的下载请求。线上是基座的 HTTP 认证层注入 Subject，
// 直接调 handler 的用例要自己注入，否则请求主体为空、属主判定必然拒绝。
func ownedRequest(id string, own fileOwner) *http.Request {
	req := httptest.NewRequest(http.MethodGet, downloadPath+id, nil)
	return req.WithContext(runtime.WithSubject(req.Context(),
		&runtime.Subject{Token: own.Token, ID: own.Subject}))
}

// resultText 取结果里第一段文本，非文本或空结果返回空串。
func resultText(t *testing.T, res *runtime.Result) string {
	t.Helper()
	if res == nil || res.Tool == nil || len(res.Tool.Content) == 0 {
		t.Fatalf("结果为空: %+v", res)
	}
	tc, ok := res.Tool.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("首段内容不是文本: %T", res.Tool.Content[0])
	}
	return tc.Text
}

// handlerReturning 构造一个固定返回 res 的链终点。
func handlerReturning(res *runtime.Result) runtime.Handler {
	return func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
		return res, nil
	}
}

// TestSpillRewritesLargeResult：超过阈值的结果必须被改写成摘要 + 下载链接，
// 且原文不能再出现在返回值里（否则模型照样把几十兆吃进上下文）。
func TestSpillRewritesLargeResult(t *testing.T) {
	withBaseURL(t, "http://127.0.0.1:8011")
	st := newTestStore(t, time.Hour, time.Hour)
	big := strings.Repeat("x", 4096)
	mw := middleware(okOptions(t, Section{ThresholdBytes: 32}), st)
	h := mw(handlerReturning(&runtime.Result{Tool: &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: big}},
	}}))

	res, err := h(context.Background(), textCall("a.read"))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	text := resultText(t, res)
	if strings.Contains(text, big) {
		t.Errorf("返回值里仍有大结果原文（len=%d）", len(text))
	}
	if !strings.Contains(text, "http://127.0.0.1:8011/spill/") {
		t.Errorf("摘要里没有下载 URL: %s", text)
	}
	if len(res.Tool.Content) != 1 {
		t.Errorf("改写后的结果应只有一段摘要文本，实际 %d 段", len(res.Tool.Content))
	}
}

// TestSpillKeepsSmallResult：阈值以内的结果必须原样透传（同一个指针，一个字节都不改）。
func TestSpillKeepsSmallResult(t *testing.T) {
	st := newTestStore(t, time.Hour, time.Hour)
	orig := runtime.TextResult("small")
	mw := middleware(okOptions(t, Section{ThresholdBytes: 1024}), st)
	res, err := mw(handlerReturning(orig))(context.Background(), textCall("a.read"))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res != orig {
		t.Fatalf("小结果必须原样透传，实际被改写: %+v", res)
	}
	if resultText(t, res) != "small" {
		t.Errorf("内容被改动: %q", resultText(t, res))
	}
}

// TestSpillCountsNonTextContent：判定必须按**结果总字节**算，覆盖图片与嵌入资源。
// 只看 *mcp.TextContent 的实现会让一张 8MB base64 图片整份流进上下文——
// 那正是 audit 的预裁剪拦不住、必须由 spill 兜住的那一类。
func TestSpillCountsNonTextContent(t *testing.T) {
	withBaseURL(t, "http://127.0.0.1:8011")
	blob := []byte(strings.Repeat("i", 4096))
	cases := []struct {
		name    string
		content mcp.Content
	}{
		{"image", &mcp.ImageContent{MIMEType: "image/png", Data: blob}},
		{"audio", &mcp.AudioContent{MIMEType: "audio/wav", Data: blob}},
		{"embedded blob", &mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
			URI: "file:///big.bin", MIMEType: "application/octet-stream", Blob: blob}}},
		{"embedded text", &mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
			URI: "file:///big.txt", MIMEType: "text/plain", Text: string(blob)}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newTestStore(t, time.Hour, time.Hour)
			mw := middleware(okOptions(t, Section{ThresholdBytes: 1024}), st)
			res, err := mw(handlerReturning(&runtime.Result{Tool: &mcp.CallToolResult{
				Content: []mcp.Content{c.content},
			}}))(context.Background(), textCall("a.read"))
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			if len(res.Tool.Content) != 1 {
				t.Fatalf("应被改写成一段摘要，实际 %d 段", len(res.Tool.Content))
			}
			if _, ok := res.Tool.Content[0].(*mcp.TextContent); !ok {
				t.Fatalf("非文本大结果没有落盘，仍是 %T", res.Tool.Content[0])
			}
			if !strings.Contains(resultText(t, res), "/spill/") {
				t.Errorf("摘要里没有 spill 链接: %s", resultText(t, res))
			}
		})
	}
}

// TestSpillClearsStructuredContent：生成的工具把同一份大 payload 同时放进
// Content 与 StructuredContent（SDK 行为）。只改 Content 等于没落盘——
// structuredContent 会原样过线进模型。
func TestSpillClearsStructuredContent(t *testing.T) {
	withBaseURL(t, "http://127.0.0.1:8011")
	st := newTestStore(t, time.Hour, time.Hour)
	payload := json.RawMessage(`{"result":"` + strings.Repeat("s", 4096) + `"}`)
	mw := middleware(okOptions(t, Section{ThresholdBytes: 1024}), st)
	res, err := mw(handlerReturning(&runtime.Result{Tool: &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(payload)}},
		StructuredContent: payload,
	}}))(context.Background(), textCall("a.read"))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Tool.StructuredContent != nil {
		t.Errorf("落盘后 structuredContent 必须清空，实际 %v", res.Tool.StructuredContent)
	}
}

// TestSpillMeasuresStructuredOnly：payload 只在 StructuredContent 里（手写工具
// 自行设置 Content 时会这样）也必须被算进总字节。
func TestSpillMeasuresStructuredOnly(t *testing.T) {
	withBaseURL(t, "http://127.0.0.1:8011")
	st := newTestStore(t, time.Hour, time.Hour)
	mw := middleware(okOptions(t, Section{ThresholdBytes: 1024}), st)
	res, err := mw(handlerReturning(&runtime.Result{Tool: &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: "见 structuredContent"}},
		StructuredContent: map[string]any{"rows": strings.Repeat("r", 4096)},
	}}))(context.Background(), textCall("a.read"))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Tool.StructuredContent != nil || !strings.Contains(resultText(t, res), "/spill/") {
		t.Errorf("只在 structuredContent 里的大结果没有落盘: %+v", res.Tool)
	}
}

// TestSpillSkipsListResults：非 tools/call 的结果不落盘——把 tools/list
// 换成下载链接等于把工具清单藏起来。
func TestSpillSkipsListResults(t *testing.T) {
	st := newTestStore(t, time.Hour, time.Hour)
	mw := middleware(okOptions(t, Section{ThresholdBytes: 8}), st)
	listRes := runtime.ListResult(&mcp.ListToolsResult{})
	got, err := mw(handlerReturning(listRes))(context.Background(),
		&runtime.Call{Method: "tools/list"})
	if err != nil || got != listRes {
		t.Errorf("tools/list 结果必须原样透传: got=%p err=%v", got, err)
	}
}

// TestSpillsErrorResults 覆盖 I4：错误态的大结果也必须落盘。
// 曾经把 IsError 当例外放过，于是「5MiB 的错误文案原样进上下文」成了一条静默的
// 超大路径，与「没有静默这一档」的承诺自相矛盾。改写后仍要保留 IsError。
func TestSpillsErrorResults(t *testing.T) {
	withBaseURL(t, "http://127.0.0.1:8011")
	st := newTestStore(t, time.Hour, time.Hour)
	huge := strings.Repeat("e", 5<<20)
	res, err := middleware(okOptions(t, Section{}), st)(
		handlerReturning(&runtime.Result{Tool: &mcp.CallToolResult{IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: huge}}}}))(
		context.Background(), textCall("a.read"))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	text := resultText(t, res)
	if strings.Contains(text, huge) {
		t.Error("错误态的大结果原样返回了（静默的超大路径）")
	}
	if !strings.Contains(text, downloadPath) {
		t.Errorf("错误态大结果没有落盘: %.200s", text)
	}
	if !res.Tool.IsError {
		t.Error("改写后丢了 IsError：模型会把落盘摘要当成成功结果")
	}
}

// TestSpillWriteFailureIsExplicit：落盘失败不得静默把超大结果原样返回。
// deny（默认）拒绝返回未落盘的大结果，warn 保留结果但必然打告警日志。
func TestSpillWriteFailureIsExplicit(t *testing.T) {
	big := &runtime.Result{Tool: &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", 4096)}}}}

	t.Run("deny", func(t *testing.T) {
		st := newTestStore(t, time.Hour, time.Hour)
		breakStore(t, st)
		var logged strings.Builder
		defer captureLog(&logged)()

		res, err := middleware(okOptions(t, Section{ThresholdBytes: 32, OnError: OnErrorDeny}), st)(
			handlerReturning(big))(context.Background(), textCall("a.read"))
		if err == nil {
			t.Fatalf("deny 下落盘失败必须报错，实际 res=%+v", res)
		}
		if res != nil {
			t.Errorf("deny 下不得把未落盘的大结果交出去: %+v", res)
		}
		if !strings.Contains(logged.String(), "spill") {
			t.Errorf("落盘失败必须留日志: %q", logged.String())
		}
	})

	t.Run("warn", func(t *testing.T) {
		st := newTestStore(t, time.Hour, time.Hour)
		breakStore(t, st)
		var logged strings.Builder
		defer captureLog(&logged)()

		res, err := middleware(okOptions(t, Section{ThresholdBytes: 32, OnError: OnErrorWarn}), st)(
			handlerReturning(big))(context.Background(), textCall("a.read"))
		if err != nil || res != big {
			t.Fatalf("warn 下应保留原结果: res=%p err=%v", res, err)
		}
		if !strings.Contains(logged.String(), "spill") {
			t.Errorf("warn 下落盘失败必须打告警日志: %q", logged.String())
		}
	})
}

// breakStore 制造「建文件」阶段的落盘失败：关掉目录句柄，之后任何 root 操作都会
// 明确失败。改成关句柄而不是改 st.dir —— 文件操作已经不走绝对路径了（复审 I-1）。
func breakStore(t *testing.T, st *store) {
	t.Helper()
	if err := st.root.Close(); err != nil {
		t.Fatalf("close root: %v", err)
	}
}
