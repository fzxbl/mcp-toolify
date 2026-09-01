package spill

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// jsonStringCases 覆盖 encoding/json 的全部转义分支：控制字符、引号反斜杠、
// HTML 转义（< > &）、行分隔符 U+2028/U+2029、非法 UTF-8（被输出成 \ufffd 转义形式
// 的 6 字节，不是裸的 3 字节）、多字节字符。
var jsonStringCases = []string{
	"", "abc", `he said "hi"`, `back\slash`, "tab\there", "nl\nrn\r",
	"\x00\x01\x1f", "<script>&</script>", "\u2028\u2029", "中文测试",
	string([]byte{0xff, 0xfe, 0x41}), "混\xffb", strings.Repeat("\x01", 500),
	strings.Repeat("é", 300), string([]byte{0xe4, 0xb8}), // 半个 UTF-8 字符
}

// TestJSONStringLenMatchesMarshal：长度估算必须与 encoding/json 的实际输出一致。
// 这是阈值判定的地基——估短了就等于给超阈值结果开一条绕过落盘的路。
func TestJSONStringLenMatchesMarshal(t *testing.T) {
	for _, s := range jsonStringCases {
		want, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %q: %v", s, err)
		}
		if got := jsonStringLen(s); got != len(want) {
			t.Errorf("jsonStringLen(%.20q) = %d, want %d（实际输出 %.40s）",
				s, got, len(want), want)
		}
	}
}

// sizeCases 是各内容类型的取样，含带 Meta/Annotations 的形态。
func sizeCases() []struct {
	name string
	item mcp.Content
} {
	size := int64(4096)
	return []struct {
		name string
		item mcp.Content
	}{
		{"text", &mcp.TextContent{Text: strings.Repeat("a", 1000)}},
		{"text 转义", &mcp.TextContent{Text: strings.Repeat("\x01", 1000)}},
		{"text 非法 UTF-8", &mcp.TextContent{Text: strings.Repeat(string([]byte{0xff}), 500)}},
		{"text 带 meta", &mcp.TextContent{Text: "hi", Meta: mcp.Meta{"k": "v"},
			Annotations: &mcp.Annotations{Audience: []mcp.Role{"user"}}}},
		{"image", &mcp.ImageContent{MIMEType: "image/png",
			Data: []byte(strings.Repeat("i", 1000))}},
		{"image 空 data", &mcp.ImageContent{MIMEType: "image/png"}},
		{"audio", &mcp.AudioContent{MIMEType: "audio/wav",
			Data: []byte(strings.Repeat("w", 999))}},
		{"embedded text", &mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
			URI: "file:///a.txt", MIMEType: "text/plain",
			Text: strings.Repeat("t", 1000)}}},
		{"embedded blob", &mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
			URI: "file:///a.bin", Blob: []byte(strings.Repeat("b", 1000)),
			Meta: mcp.Meta{"k": "v"}}}},
		{"embedded nil resource", &mcp.EmbeddedResource{}},
		{"resource link", &mcp.ResourceLink{URI: "file:///x", Name: "x", Title: "标题",
			Description: strings.Repeat("d", 500), MIMEType: "text/plain", Size: &size}},
		{"tool use（default 分支）", &mcp.ToolUseContent{ID: "1", Name: "t",
			Input: map[string]any{"a": strings.Repeat("v", 300)}}},
	}
}

// TestContentBytesNeverUnderestimates：估算值必须 >= 真实 wire 字节数。
// 低估就是 fail-open：实际会超阈值的结果被判为未超、原样进上下文。
// 同时要求不能离谱地高估（否则阈值形同虚设）。
func TestContentBytesNeverUnderestimates(t *testing.T) {
	for _, c := range sizeCases() {
		t.Run(c.name, func(t *testing.T) {
			want, err := json.Marshal(c.item)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := contentBytes(c.item)
			if got < len(want) {
				t.Errorf("contentBytes = %d < 实际 wire %d（低估 = fail-open）", got, len(want))
			}
			if got > len(want)+512 {
				t.Errorf("contentBytes = %d 比实际 wire %d 高估超过 512 字节", got, len(want))
			}
		})
	}
}

// TestStructuredBytesNeverUnderestimates：structuredContent 同理，含 []byte
// （SDK 会序列化成 base64 字符串，不是原文透传）。
func TestStructuredBytesNeverUnderestimates(t *testing.T) {
	cases := []struct {
		name string
		val  any
	}{
		{"raw message", json.RawMessage(`{"a":"` + strings.Repeat("x", 500) + `"}`)},
		{"bytes", []byte(strings.Repeat("y", 500))},
		{"map", map[string]any{"rows": strings.Repeat("z", 500)}},
		{"string 带转义", strings.Repeat("\x01", 300)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want, err := json.Marshal(c.val)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if got := structuredBytes(c.val); got < len(want) {
				t.Errorf("structuredBytes = %d < 实际 wire %d", got, len(want))
			}
		})
	}
}

// TestUnmarshalableCountsAsOversized：序列化失败的值不能被当成 0 字节——
// 那等于给这类结果开一条绕过落盘的路。判定上按「体积未知即超阈值」处理。
func TestUnmarshalableCountsAsOversized(t *testing.T) {
	if got := structuredBytes(map[string]any{"fn": func() {}}); got < defaultThreshold {
		t.Errorf("无法序列化的 structuredContent 估算 = %d，应被判为超阈值", got)
	}
}

// TestSpillCatchesEscapeInflation 复现审查者的 I1：60000 个 \x01 的文本 payload
// 只有 60000 字节（< 65536 阈值），序列化后是 360039 字节。按 payload 字节判定
// 会放它原样进上下文；按 wire 字节判定必须落盘。
func TestSpillCatchesEscapeInflation(t *testing.T) {
	withBaseURL(t, "http://127.0.0.1:8011")
	st := newTestStore(t, time.Hour, time.Hour)
	text := strings.Repeat("\x01", 60000)
	wire, err := json.Marshal(&mcp.TextContent{Text: text})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(text) >= defaultThreshold || len(wire) <= defaultThreshold {
		t.Fatalf("用例前提不成立：payload=%d wire=%d threshold=%d",
			len(text), len(wire), defaultThreshold)
	}
	res, err := middleware(okOptions(t, Section{}), st)(
		handlerReturning(&runtime.Result{Tool: &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}}}}))(
		context.Background(), textCall("a.read"))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !strings.Contains(resultText(t, res), downloadPath) {
		t.Errorf("转义膨胀后已超阈值的结果没有落盘（wire=%d 阈值=%d）",
			len(wire), defaultThreshold)
	}
}

// TestSpillCatchesBase64Inflation 复现审查者的 I1 第二例：60000 字节图片 payload
// base64 膨胀后 80063 字节，超过默认阈值。
func TestSpillCatchesBase64Inflation(t *testing.T) {
	withBaseURL(t, "http://127.0.0.1:8011")
	st := newTestStore(t, time.Hour, time.Hour)
	img := &mcp.ImageContent{MIMEType: "image/png", Data: []byte(strings.Repeat("i", 60000))}
	wire, err := json.Marshal(img)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(img.Data) >= defaultThreshold || len(wire) <= defaultThreshold {
		t.Fatalf("用例前提不成立：payload=%d wire=%d", len(img.Data), len(wire))
	}
	res, err := middleware(okOptions(t, Section{}), st)(
		handlerReturning(&runtime.Result{Tool: &mcp.CallToolResult{
			Content: []mcp.Content{img}}}))(context.Background(), textCall("a.read"))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !strings.Contains(resultText(t, res), downloadPath) {
		t.Errorf("base64 膨胀后已超阈值的图片没有落盘（wire=%d）", len(wire))
	}
}

// minAllocsPerRun 连测三轮 AllocsPerRun 并取最小值，见调用处的注释。
func minAllocsPerRun(f func()) float64 {
	min := testing.AllocsPerRun(100, f)
	for i := 0; i < 2; i++ {
		if n := testing.AllocsPerRun(100, f); n < min {
			min = n
		}
	}
	return min
}

// TestSizeEstimationDoesNotAllocateOnNilFields 覆盖复审 M-1：注释宣称的「不做
// json.Marshal」原来与实测不符 —— extraBytes 对 nil 的 Meta / Annotations 各
// marshal 一次，contentBytes 实测 2 allocs/op。加了 nil 快路径之后，常见结果
// （这些可选字段全为 nil）的整条估算路径必须零分配。
//
// 只钉「nil 字段」这个常见形态：带 Annotations 的结果本来就要序列化一次小对象，
// 那不是缺陷，注释里也已经写明。
func TestSizeEstimationDoesNotAllocateOnNilFields(t *testing.T) {
	text := &mcp.TextContent{Text: strings.Repeat("t", 4096)}
	image := &mcp.ImageContent{MIMEType: "image/png", Data: make([]byte, 4096)}
	res := &mcp.CallToolResult{Content: []mcp.Content{text, image}}

	cases := []struct {
		name string
		f    func()
	}{
		{"contentBytes/text", func() { _ = contentBytes(text) }},
		{"contentBytes/image", func() { _ = contentBytes(image) }},
		{"resultBytes", func() { _ = resultBytes(res, 1<<30) }},
	}
	for _, c := range cases {
		// 三轮取最小：AllocsPerRun 数的是**进程级**的 Mallocs 增量，同进程里别的协程
		// （store 的回收协程、httptest 服务）只要在测量窗口里分配一次，got 就会变成
		// 0.01 而不是 0，断言随负载偶发变红。这类干扰只会让计数偏大、不会偏小，
		// 所以取最小值即可，杀伤力不变（真有分配时每轮都会数到，见变异验证）。
		if got := minAllocsPerRun(c.f); got != 0 {
			t.Errorf("%s = %.2f allocs/op，want 0（可选字段均为 nil 时不该序列化）",
				c.name, got)
		}
	}
}
