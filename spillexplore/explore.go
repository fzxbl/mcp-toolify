package spillexplore

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/itchyny/gojq"
)

// ReadOut 是 read 操作的返回结构。
type ReadOut struct {
	Content        string `json:"content"`
	NextLineOffset int    `json:"next_line_offset"`
	EOF            bool   `json:"eof"`
	Truncated      bool   `json:"truncated"`
}

// readLines 从 lineOffset（0 基）起读 limit 行，累计不超过 maxBytes 字节。
// limit<=0 表示不限行数（受 maxBytes 约束）。读到文件末尾置 EOF=true。
func readLines(path string, lineOffset, limit, maxBytes int) (ReadOut, error) {
	f, err := os.Open(path)
	if err != nil {
		return ReadOut{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var out ReadOut
	idx, bytes := 0, 0
	for sc.Scan() {
		if idx < lineOffset {
			idx++
			continue
		}
		if limit > 0 && (idx-lineOffset) >= limit {
			return out, nil // 还有后续行，EOF=false
		}
		line := sc.Text() + "\n"
		if maxBytes > 0 && bytes+len(line) > maxBytes {
			out.Truncated = true
			out.NextLineOffset = idx
			return out, nil
		}
		out.Content += line
		bytes += len(line)
		idx++
		out.NextLineOffset = idx
	}
	out.EOF = true
	return out, nil
}

// grepLines 对文件逐行做正则匹配，返回 "行号:内容"，受 maxLines/maxBytes 约束。
func grepLines(path, pattern string, maxLines, maxBytes int) ([]string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("bad pattern: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var out []string
	n, bytes := 0, 0
	for sc.Scan() {
		n++
		if re.MatchString(sc.Text()) {
			line := fmt.Sprintf("%d:%s", n, sc.Text())
			bytes += len(line)
			if (maxBytes > 0 && bytes > maxBytes) || (maxLines > 0 && len(out) >= maxLines) {
				break
			}
			out = append(out, line)
		}
	}
	return out, nil
}

// runJQ 对 json / jsonl 执行 gojq 表达式。perLine=true 时逐行（jsonl）执行。
func runJQ(path, expr string, perLine bool) ([]string, error) {
	q, err := gojq.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("bad jq: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var inputs []any
	if perLine {
		for _, ln := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if ln == "" {
				continue
			}
			var v any
			if err := json.Unmarshal([]byte(ln), &v); err != nil {
				return nil, err
			}
			inputs = append(inputs, v)
		}
	} else {
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		inputs = append(inputs, v)
	}
	var out []string
	for _, in := range inputs {
		it := q.Run(in)
		for {
			v, ok := it.Next()
			if !ok {
				break
			}
			if e, ok := v.(error); ok {
				return nil, e
			}
			b, _ := json.Marshal(v)
			out = append(out, string(b))
		}
	}
	return out, nil
}

// schemaOf：json 解析顶层；jsonl 取首行推断元素结构。输出有界的类型描述。
// depth 控制递归展开的层数（<=0 时按 1 处理，保持只展开一层的旧行为）。
func schemaOf(path string, jsonl bool, depth int) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sample = data
	if jsonl {
		if i := strings.IndexByte(string(data), '\n'); i >= 0 {
			sample = data[:i]
		}
	}
	var v any
	if err := json.Unmarshal(sample, &v); err != nil {
		return nil, err
	}
	if depth <= 0 {
		depth = 1
	}
	return map[string]any{"element_type": describe(v, depth)}, nil
}

// describe 按 depth 有限层数展开结构：
//   - depth>0 时，对象/数组展开一层，子节点以 depth-1 继续递归；
//   - depth<=0（到达深度上限）时，容器折叠为 "object"/"array" 字符串，标量给具体类型名。
//
// 这样对深层嵌套（如 Status.InstanceStatistic.Detail）可一次看清多层结构，
// 又不会无限展开导致输出爆炸。
func describe(v any, depth int) any {
	switch t := v.(type) {
	case map[string]any:
		if depth <= 0 {
			return "object"
		}
		keys := map[string]any{}
		for k, val := range t {
			keys[k] = describe(val, depth-1)
		}
		return keys
	case []any:
		if depth <= 0 {
			return "array"
		}
		if len(t) > 0 {
			return []any{describe(t[0], depth-1)}
		}
		return []any{}
	default:
		return typeName(v)
	}
}

func typeName(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
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
