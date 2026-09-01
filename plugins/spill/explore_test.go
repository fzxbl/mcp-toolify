package spill

import (
	"strings"
	"testing"
)

// jsonlID 写一份 jsonl 内容并返回 id。
func jsonlID(t *testing.T, lines ...string) string {
	t.Helper()
	id, err := Put("batch.result", FormatJSONL, []byte(strings.Join(lines, "\n")+"\n"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return id
}

// TestExploreStat：先问「多大」是模型该有的第一步，stat 必须给出规模与下载入口。
func TestExploreStat(t *testing.T) {
	hostStore(t)
	id := jsonlID(t, `{"app":"a","cpu":1}`)
	out, err := explore(exploreInput{ID: id, Op: opStat})
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if out["format"] != string(FormatJSONL) {
		t.Errorf("format = %v，want jsonl", out["format"])
	}
	if size, _ := out["size"].(int64); size <= 0 {
		t.Errorf("size = %v，应是 payload 字节数", out["size"])
	}
}

// TestExploreReadPagesByLine：read 按行分页，next_line_offset 必须能直接用于下一页。
func TestExploreReadPagesByLine(t *testing.T) {
	hostStore(t)
	id := jsonlID(t, `{"n":1}`, `{"n":2}`, `{"n":3}`)
	first, err := explore(exploreInput{ID: id, Op: opRead, Limit: 2})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, _ := first["content"].(string); got != `{"n":1}`+"\n"+`{"n":2}`+"\n" {
		t.Fatalf("第一页 = %q", got)
	}
	if eof, _ := first["eof"].(bool); eof {
		t.Error("还有第三行时 eof 不能为真，否则调用方会停在半路")
	}
	next, _ := first["next_line_offset"].(int)
	second, err := explore(exploreInput{ID: id, Op: opRead, LineOffset: next})
	if err != nil {
		t.Fatalf("read 第二页: %v", err)
	}
	if got, _ := second["content"].(string); got != `{"n":3}`+"\n" {
		t.Errorf("第二页 = %q，next_line_offset 应能直接续读", got)
	}
	if eof, _ := second["eof"].(bool); !eof {
		t.Error("读到末尾必须置 eof")
	}
}

// TestExploreReadBudgetTruncates：单次返回有字节上界，否则一次探索就把大内容
// 整份搬进模型上下文——那正是落盘要避免的事。
func TestExploreReadBudgetTruncates(t *testing.T) {
	hostStore(t)
	id, err := Put("big.txt", FormatText, []byte(strings.Repeat("0123456789\n", 100)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	out, err := explore(exploreInput{ID: id, Op: opRead, MaxBytes: 33})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if trunc, _ := out["truncated"].(bool); !trunc {
		t.Error("超出 max_bytes 必须标记 truncated")
	}
	if content, _ := out["content"].(string); len(content) > 33 {
		t.Errorf("返回 %d 字节，超过了 max_bytes=33", len(content))
	}
}

// TestExploreGrep：grep 返回「行号:内容」，并把扫过多少行告诉调用方。
func TestExploreGrep(t *testing.T) {
	hostStore(t)
	id, err := Put("run.log", FormatText,
		[]byte("ok host-1\nERROR host-2 timeout\nok host-3\n"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	out, err := explore(exploreInput{ID: id, Op: opGrep, Pattern: "^ERROR"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	matches, _ := out["matches"].([]string)
	if len(matches) != 1 || !strings.HasPrefix(matches[0], "2:") {
		t.Errorf("matches = %v，want 第 2 行命中", matches)
	}
	if n, _ := out["scanned_lines"].(int); n != 3 {
		t.Errorf("scanned_lines = %v，want 3", out["scanned_lines"])
	}
}

// TestExploreGrepNeedsPattern：缺参数要报错，不能当成「匹配一切」。
func TestExploreGrepNeedsPattern(t *testing.T) {
	hostStore(t)
	id := jsonlID(t, `{"n":1}`)
	if _, err := explore(exploreInput{ID: id, Op: opGrep}); err == nil {
		t.Fatal("op=grep 缺 pattern 必须报错")
	}
	if _, err := explore(exploreInput{ID: id, Op: opGrep, Pattern: "("}); err == nil {
		t.Fatal("非法正则必须报错")
	}
}

// TestExploreSchemaUsesFirstLineForJSONL：jsonl 的结构取首行样本，
// 且 depth 要真的控制展开层数。
func TestExploreSchemaUsesFirstLineForJSONL(t *testing.T) {
	hostStore(t)
	id := jsonlID(t, `{"app":"a","status":{"detail":{"n":1}}}`)
	shallow, err := explore(exploreInput{ID: id, Op: opSchema, Depth: 1})
	if err != nil {
		t.Fatalf("schema depth=1: %v", err)
	}
	top, _ := shallow["element_type"].(map[string]any)
	if top["app"] != "string" || top["status"] != "object" {
		t.Errorf("depth=1 应只展开一层，实际 = %v", top)
	}
	deep, err := explore(exploreInput{ID: id, Op: opSchema, Depth: 3})
	if err != nil {
		t.Fatalf("schema depth=3: %v", err)
	}
	dt, _ := deep["element_type"].(map[string]any)
	st, _ := dt["status"].(map[string]any)
	if _, ok := st["detail"].(map[string]any); !ok {
		t.Errorf("depth=3 应展开到 detail，实际 = %v", dt)
	}
}

// TestExploreJQPerLine：jsonl 的 jq 是逐行执行的（每行一个输入）。
func TestExploreJQPerLine(t *testing.T) {
	hostStore(t)
	id := jsonlID(t, `{"app":"a","cpu":1}`, `{"app":"b","cpu":2}`)
	out, err := explore(exploreInput{ID: id, Op: opJQ, JqExpr: ".app"})
	if err != nil {
		t.Fatalf("jq: %v", err)
	}
	values, _ := out["values"].([]string)
	if len(values) != 2 || values[0] != `"a"` || values[1] != `"b"` {
		t.Errorf("values = %v，want 两行各一个值", values)
	}
}

// TestExploreJQOnJSON：json 是一个整体输入，按路径取值。
func TestExploreJQOnJSON(t *testing.T) {
	hostStore(t)
	id, err := Put("one.json", FormatJSON, []byte(`{"items":[{"id":7}]}`))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	out, err := explore(exploreInput{ID: id, Op: opJQ, JqExpr: ".items[0].id"})
	if err != nil {
		t.Fatalf("jq: %v", err)
	}
	if values, _ := out["values"].([]string); len(values) != 1 || values[0] != "7" {
		t.Errorf("values = %v，want [7]", out["values"])
	}
}

// TestExploreRejectsStructuredOpsOnText：text 没有结构，schema/jq 要明确拒绝并
// 指向可用的操作，而不是回一个空结果让模型反复试。
func TestExploreRejectsStructuredOpsOnText(t *testing.T) {
	hostStore(t)
	id, err := Put("run.log", FormatText, []byte("plain\n"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, op := range []string{opSchema, opJQ} {
		_, err := explore(exploreInput{ID: id, Op: op, JqExpr: "."})
		if err == nil {
			t.Errorf("text 上的 op=%s 必须报错", op)
			continue
		}
		if !strings.Contains(err.Error(), "op=read") {
			t.Errorf("op=%s 的错误应指向可用操作，实际 = %v", op, err)
		}
	}
}

// TestExploreRejectsBadID：id 来自调用方，形状不合法与路径穿越都必须在打开文件
// 之前被拒——它是拼进文件路径的唯一外部输入。
func TestExploreRejectsBadID(t *testing.T) {
	hostStore(t)
	for _, id := range []string{"", "../../etc/passwd", "zz/../x", strings.Repeat("a", 500)} {
		if _, err := explore(exploreInput{ID: id, Op: opStat}); err == nil {
			t.Errorf("id %q 必须被拒绝", id)
		}
	}
}

// TestExploreUnknownOp：不认识的 op 要把可选值列出来。
func TestExploreUnknownOp(t *testing.T) {
	hostStore(t)
	id := jsonlID(t, `{"n":1}`)
	_, err := explore(exploreInput{ID: id, Op: "tail"})
	if err == nil || !strings.Contains(err.Error(), "stat") {
		t.Fatalf("err = %v，应列出可用的 op", err)
	}
}
