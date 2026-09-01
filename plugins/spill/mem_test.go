package spill

import (
	"bufio"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxWriteRecorder 记录下层收到的**最大一次写**、写入次数与总字节数。
type maxWriteRecorder struct {
	max   int
	calls int
	total int64
}

func (r *maxWriteRecorder) Write(p []byte) (int, error) {
	if len(p) > r.max {
		r.max = len(p)
	}
	r.calls++
	r.total += int64(len(p))
	return len(p), nil
}

// segmentedResult 造一个总量固定、分成 segments 段的结果。
func segmentedResult(total, segments int) *mcp.CallToolResult {
	each := total / segments
	content := make([]mcp.Content, 0, segments)
	for i := 0; i < segments; i++ {
		content = append(content, &mcp.TextContent{Text: strings.Repeat("m", each)})
	}
	return &mcp.CallToolResult{Content: content}
}

// TestSpillWritesResultInSegments 固定住 put 的内存宣称：内容是**逐段**交出去的，
// 用量随「最大的那一段」增长，而不是先在内存里拼成整串。判据是「下层收到的最大一次
// 写」——它就等于最大那一段序列化后的长度，与段数无关。
//
// 为什么不用分配量比值（本用例的前一版）：那一版用 TotalAlloc 增量比较「单段 32MiB
// vs 16 段共 32MiB」，有两个致命问题。
//
//  1. **对负载敏感**：runtime.MemStats 是进程级的，整包并行 + -race 跑全量时别的协程
//     的分配会被算进测量区间。实测 flaky 一次：单段侧 5.01x 涨到 7.01x、16 段侧
//     2.3~2.6x 涨到 5.28x，比值被顶到 0.753，刚好越过 0.75 的界。
//  2. **多轮取最小压噪声也不成立**（复审建议的方向，我实测否掉了）：encoding/json 内部
//     有 encodeState 缓冲池，第一次 Marshal 一个 32MiB 字符串要把缓冲扩到 33MB，之后
//     几轮直接复用那个池化缓冲。于是重复测量时单段侧从 5.01x 掉到 2.00x，而 16 段侧
//     仍是 2.52x —— 比值反过来变成 1.256，取最小之后这条断言恒红。池化把「同时驻留
//     一整份拷贝」这件事从**分配计数**里藏掉了，所以分配量根本不是这条宣称的合格判据。
//
// 改成量「最大单次写」之后，判据是确定性的：没有采样、没有 GC 时机、没有跨协程串扰，
// 因此对负载完全不敏感；而「把 Content 整串 json.Marshal 再一次写出」这个变异会产生
// 一次约 8MiB 的写，必然越界变红（杀伤力比原来的 0.75 界更强：原来只差 0.09）。
func TestSpillWritesResultInSegments(t *testing.T) {
	const total = 8 << 20
	const segments = 16
	const segSize = total / segments

	rec := &maxWriteRecorder{}
	// bufio 缓冲刻意取到最小（16 字节）：超过缓冲的写会被 bufio 直接透传给下层，
	// 于是 rec 看到的就是 writeSpillJSON 交出来的原始分段大小，不被缓冲重新切分。
	w := &countingWriter{w: bufio.NewWriterSize(rec, 16)}
	if err := writeSpillJSON(w, "a.read", segmentedResult(total, segments), testOwner); err != nil {
		t.Fatalf("writeSpillJSON: %v", err)
	}
	if err := w.w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	t.Logf("内容体积 %d 字节 / %d 段（每段 %d）：最大单次写 %d 字节（%.2f 段），"+
		"写入 %d 次，共 %d 字节", total, segments, segSize, rec.max,
		float64(rec.max)/float64(segSize), rec.calls, rec.total)

	// 界：最大一次写不超过两段。留一倍余量给 JSON 转义与骨架，同时远低于整串（16 段）。
	if rec.max > 2*segSize {
		t.Errorf("最大单次写 %d 字节，超过两段（%d）：内容被拼成整串再写，"+
			"内存用量会随段数增长", rec.max, 2*segSize)
	}
	// 至少每段一次写：证明真的是逐段交出去的，而不是「一次写完 + 若干小写」。
	if rec.calls < segments {
		t.Errorf("只写了 %d 次，少于段数 %d：内容没有逐段交出", rec.calls, segments)
	}
	// 内容确实全写出去了：否则「写少了」也能满足上面两条。
	if w.n < int64(total) {
		t.Errorf("只写出 %d 字节，少于内容体积 %d：结果被截断了", w.n, total)
	}
}
