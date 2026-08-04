package spillexplore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// SpillExplore 探索任意 spill 资源（read/grep/schema/jq），对 json/jsonl/text 通用。
//
// 核心原则：写 jq/grep 前先摸清真实结构（字段名、大小写、规模），不要凭 skill 文档里的
// fields 名想当然——按下面各格式的"先看结构"方式确认后再提取。必要时先 op=stat 看大小/行数。
//
// 按格式选择操作：
//   - jsonl（一行一条记录，如批量结果）：先 op=read limit=1 读 1 行看清单条记录的真实字段与
//     大小写，再写 op=jq（逐行执行）提取；也可 op=schema 取首行推断结构（配 depth 深看嵌套）；
//     翻页用 op=read/op=grep 按行。
//   - json（单个 JSON 对象/数组）：整份是一个值，read 一行往往就是全部内容、意义不大；应先
//     op=schema 看整体结构（配 depth 展开嵌套），再用 op=jq 按路径提取。
//   - text（扫日志等非结构化文本）：只用 op=read 行偏移分页 + op=grep 正则过滤；不支持 schema/jq。
//
// param: id — spill 资源 id（spill:// 后面那段）
// param: op — 操作：stat / read / grep / schema / jq
// param: lineOffset — read 起始行（0 基）
// param: limit — read/grep 返回行数上限（jsonl 首次探索建议 limit=1）
// param: pattern — grep 正则
// param: jqExpr — jq 表达式（仅 json/jsonl）
// param: depth — schema 递归展开层数（<=0 时默认 2）
// param: maxBytes — 返回字节上限（<=0 时默认 1MB）
//
// 返回：探索结果；error 表示资源不存在或参数错误。
//
// mcp:tool
// mcp:name=spill_explore
// mcp:tags=read,spill
// mcp:risk=low
func SpillExplore(id, op string, lineOffset, limit int, pattern, jqExpr string, depth, maxBytes int) (map[string]any, error) {
	path, ok := runtime.ResolveSpillPath(id)
	if !ok {
		return nil, fmt.Errorf("spill resource not found: %s", id)
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	isJSONL := strings.HasSuffix(path, ".jsonl")
	isJSON := strings.HasSuffix(path, ".json")
	res := map[string]any{"id": id, "download_url": runtime.SpillDownloadURLFor(id)}
	switch op {
	case "stat":
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		res["size"] = info.Size()
		res["format"] = strings.TrimPrefix(filepath.Ext(path), ".")
		return res, nil
	case "read":
		r, err := readLines(path, lineOffset, limit, maxBytes)
		if err != nil {
			return nil, err
		}
		res["read"] = r
		return res, nil
	case "grep":
		lines, err := grepLines(path, pattern, limit, maxBytes)
		if err != nil {
			return nil, err
		}
		res["lines"] = lines
		return res, nil
	case "schema":
		if !isJSON && !isJSONL {
			return nil, fmt.Errorf("schema 仅支持 json/jsonl，当前为 %s", filepath.Ext(path))
		}
		if depth <= 0 {
			depth = 2
		}
		s, err := schemaOf(path, isJSONL, depth)
		if err != nil {
			return nil, err
		}
		res["schema"] = s
		return res, nil
	case "jq":
		if !isJSON && !isJSONL {
			return nil, fmt.Errorf("jq 仅支持 json/jsonl，当前为 %s", filepath.Ext(path))
		}
		vals, err := runJQ(path, jqExpr, isJSONL)
		if err != nil {
			return nil, err
		}
		res["results"] = vals
		return res, nil
	default:
		return nil, fmt.Errorf("unknown op: %s", op)
	}
}
