package spill

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/fzxbl/mcp-toolify/runtime"
	"github.com/itchyny/gojq"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// exploreToolName 是探索工具的名字。它不带包名前缀（生成的业务工具才有
// <pkg>.<func> 形状）：这是插件自带的工具，宿主的 token 规则按 name 放行它。
const exploreToolName = "spill_explore"

// 探索操作。刻意只有这五种：它们对应「多大」「看几行」「过滤」「什么结构」
// 「按路径取值」这五个真实动作，再多的操作只会让模型在选择上出错。
const (
	opStat   = "stat"
	opRead   = "read"
	opGrep   = "grep"
	opSchema = "schema"
	opJQ     = "jq"
)

// 探索的输出上界。它们防的是「一次探索把整份大内容搬进模型上下文」——
// 那正是落盘要避免的事。
const (
	defaultExploreBytes = 1 << 20 // 单次返回字节上限，默认 1MiB
	maxExploreBytes     = 8 << 20 // 调用方可要求的上限的上限
	maxScanLineBytes    = 4 << 20 // 单行最长 4MiB，超长行直接报错而不是静默截断
	defaultSchemaDepth  = 2
	maxGrepLines        = 2000
)

// exploreInput 是 spill_explore 的入参。
type exploreInput struct {
	ID         string `json:"id" jsonschema:"spill 内容 id（工具返回的摘要或 spill:// 后面那段）"`
	Op         string `json:"op" jsonschema:"操作：stat / read / grep / schema / jq"`
	LineOffset int    `json:"line_offset" jsonschema:"read 的起始行（0 基）"`
	Limit      int    `json:"limit" jsonschema:"read/grep 返回行数上限（jsonl 首次探索建议取 1）"`
	Pattern    string `json:"pattern" jsonschema:"grep 的正则"`
	JqExpr     string `json:"jq_expr" jsonschema:"jq 表达式（仅 json/jsonl）"`
	Depth      int    `json:"depth" jsonschema:"schema 递归展开层数（不填按 2）"`
	MaxBytes   int    `json:"max_bytes" jsonschema:"返回字节上限（不填按 1MiB）"`
}

// exploreDesc 是工具描述。它同时是给模型的使用说明，因此把「先摸结构再提取」
// 这条纪律写在最前面：凭字段名想当然写 jq 是这类工具最常见的失败方式。
const exploreDesc = "探索一份已落盘的 spill 内容（read/grep/schema/jq/stat），" +
	"对 json / jsonl / text 通用。核心原则：写 jq/grep 之前先摸清真实结构" +
	"（字段名与大小写、规模），不要按文档猜。" +
	"按格式选择操作：jsonl 先 op=read limit=1 看一条记录、再 op=jq 逐行提取；" +
	"json 先 op=schema（配 depth 展开嵌套）、再 op=jq 按路径取值；" +
	"text 只能 op=read 分页与 op=grep 过滤。不确定多大时先 op=stat。" +
	"返回：该操作的结果；error 表示内容不存在、已过期或参数错误。"

// addExploreTool 把探索工具注册到 MCP server，并登记 labels。
//
// labels 打的是 capability=read / risk=none：它只读已经产出的内容，且内容的
// 可见性由落盘时的属主与 token 规则决定，不是一个能改变线上状态的操作。
func addExploreTool(s *mcp.Server) {
	runtime.RegisterTool(runtime.ToolInfo{
		Name: exploreToolName, Pkg: Name,
		Labels: map[string]string{"capability": "read", "risk": "none", "plugin": Name},
	})
	mcp.AddTool(s, &mcp.Tool{Name: exploreToolName, Description: exploreDesc}, handleExplore)
}

// handleExplore 是工具处理函数。
func handleExplore(ctx context.Context, req *mcp.CallToolRequest,
	in exploreInput) (*mcp.CallToolResult, any, error) {
	out, err := explore(in)
	if err != nil {
		return runtime.ToolError(err), nil, nil
	}
	return nil, map[string]any{"result": out}, nil
}

// explore 执行一次探索。
func explore(in exploreInput) (map[string]any, error) {
	rc, info, err := Open(in.ID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	budget := in.MaxBytes
	if budget <= 0 {
		budget = defaultExploreBytes
	}
	if budget > maxExploreBytes {
		budget = maxExploreBytes
	}

	switch in.Op {
	case opStat, "":
		return map[string]any{"id": info.ID, "name": info.Name,
			"format": string(info.Format), "size": info.Size,
			"modified": info.ModTime.Format("2006-01-02 15:04:05"),
			"download": URLFor(info.ID)}, nil
	case opRead:
		return readLines(rc, in.LineOffset, in.Limit, budget)
	case opGrep:
		return grepLines(rc, in.Pattern, in.Limit, budget)
	case opSchema:
		return schemaOf(rc, info.Format, in.Depth, budget)
	case opJQ:
		return runJQ(rc, info.Format, in.JqExpr, budget)
	default:
		return nil, fmt.Errorf("op %q 不认识（只接受 stat / read / grep / schema / jq）", in.Op)
	}
}

// scanner 返回一个能扫长行的行扫描器。
func scanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxScanLineBytes)
	return sc
}

// readLines 从 lineOffset（0 基）起读 limit 行，累计不超过 budget 字节。
// limit<=0 表示不限行数（仍受 budget 约束）。
func readLines(r io.Reader, lineOffset, limit, budget int) (map[string]any, error) {
	sc := scanner(r)
	var b strings.Builder
	idx, next, truncated, eof := 0, lineOffset, false, false
	for {
		if !sc.Scan() {
			eof = sc.Err() == nil
			break
		}
		if idx < lineOffset {
			idx++
			continue
		}
		if limit > 0 && idx-lineOffset >= limit {
			break // 还有后续行，eof=false
		}
		line := sc.Text() + "\n"
		if b.Len()+len(line) > budget {
			truncated = true
			break
		}
		b.WriteString(line)
		idx++
		next = idx
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取内容: %w", err)
	}
	return map[string]any{"content": b.String(), "next_line_offset": next,
		"eof": eof, "truncated": truncated}, nil
}

// grepLines 逐行做正则匹配，返回 "行号:内容"，受 limit 与 budget 约束。
func grepLines(r io.Reader, pattern string, limit, budget int) (map[string]any, error) {
	if pattern == "" {
		return nil, fmt.Errorf("op=grep 需要 pattern")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("pattern 不是合法正则: %w", err)
	}
	if limit <= 0 || limit > maxGrepLines {
		limit = maxGrepLines
	}
	sc := scanner(r)
	out, n, used, truncated := make([]string, 0, 16), 0, 0, false
	for sc.Scan() {
		n++
		if !re.MatchString(sc.Text()) {
			continue
		}
		line := fmt.Sprintf("%d:%s", n, sc.Text())
		if used+len(line) > budget || len(out) >= limit {
			truncated = true
			break
		}
		out = append(out, line)
		used += len(line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取内容: %w", err)
	}
	return map[string]any{"matches": out, "scanned_lines": n, "truncated": truncated}, nil
}

// schemaOf 推断结构：json 解析整份，jsonl 取首行作为样本。
func schemaOf(r io.Reader, f Format, depth, budget int) (map[string]any, error) {
	if f == FormatText {
		return nil, fmt.Errorf("text 内容没有结构可推断，请用 op=read / op=grep")
	}
	if depth <= 0 {
		depth = defaultSchemaDepth
	}
	sample, err := sampleValue(r, f, budget)
	if err != nil {
		return nil, err
	}
	return map[string]any{"element_type": describeValue(sample, depth)}, nil
}

// sampleValue 取一份用于推断结构的样本值：jsonl 取首行，json 取整份（受 budget 约束）。
func sampleValue(r io.Reader, f Format, budget int) (any, error) {
	var raw []byte
	if f == FormatJSONL {
		sc := scanner(r)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				raw = []byte(line)
				break
			}
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("读取内容: %w", err)
		}
	} else {
		b, err := io.ReadAll(io.LimitReader(r, int64(budget)+1))
		if err != nil {
			return nil, fmt.Errorf("读取内容: %w", err)
		}
		if len(b) > budget {
			return nil, fmt.Errorf("这份 json 超过 %d 字节，无法整份解析："+
				"请用下载链接取原文，或改用 op=grep 定位片段", budget)
		}
		raw = b
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("内容为空（还在写入时可能如此，稍后重试）")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("内容不是合法 JSON: %w", err)
	}
	return v, nil
}

// runJQ 对 json / jsonl 执行 jq 表达式；jsonl 逐行执行。
func runJQ(r io.Reader, f Format, expr string, budget int) (map[string]any, error) {
	if f == FormatText {
		return nil, fmt.Errorf("text 内容不支持 jq，请用 op=read / op=grep")
	}
	if expr == "" {
		return nil, fmt.Errorf("op=jq 需要 jq_expr")
	}
	q, err := gojq.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("jq 表达式无效: %w", err)
	}
	out, used, truncated := make([]string, 0, 16), 0, false
	emit := func(v any) bool {
		it := q.Run(v)
		for {
			got, ok := it.Next()
			if !ok {
				return true
			}
			if e, ok := got.(error); ok {
				out = append(out, fmt.Sprintf("jq error: %v", e))
				return true
			}
			b, _ := json.Marshal(got)
			if used+len(b) > budget {
				truncated = true
				return false
			}
			out = append(out, string(b))
			used += len(b)
		}
	}
	if f == FormatJSONL {
		sc := scanner(r)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var v any
			if err := json.Unmarshal([]byte(line), &v); err != nil {
				return nil, fmt.Errorf("第 %d 行不是合法 JSON: %w", len(out)+1, err)
			}
			if !emit(v) {
				break
			}
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("读取内容: %w", err)
		}
	} else {
		v, err := sampleValue(r, FormatJSON, budget)
		if err != nil {
			return nil, err
		}
		emit(v)
	}
	return map[string]any{"values": out, "truncated": truncated}, nil
}

// describeValue 按 depth 有限层数展开结构：到达深度上限时容器折叠成
// "object"/"array"，标量给类型名。既能一次看清多层嵌套，又不会展开到爆。
func describeValue(v any, depth int) any {
	switch t := v.(type) {
	case map[string]any:
		if depth <= 0 {
			return "object"
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = describeValue(val, depth-1)
		}
		return out
	case []any:
		if depth <= 0 {
			return "array"
		}
		if len(t) > 0 {
			return []any{describeValue(t[0], depth-1)}
		}
		return []any{}
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", v)
	}
}
