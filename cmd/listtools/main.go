// listtools 用 in-memory transport 启动 mcp server，验证暴露给 MCP 客户端
// （Cherry Studio / Claude Desktop 等）的完整元数据。
//
// 默认输出每个工具的 name + description + inputSchema + outputSchema，
// 让你检查描述、参数说明、required、类型是否符合预期。
//
// 用法：
//
//	go run ./mcp/cmd/listtools                      # 详细：name + desc + 参数
//	go run ./mcp/cmd/listtools -short               # 紧凑：每行一个工具
//	go run ./mcp/cmd/listtools -grep app_command    # name 子串过滤
//	go run ./mcp/cmd/listtools -enable pandora      # 包过滤
//	go run ./mcp/cmd/listtools -tags read           # tag 过滤
//	go run ./mcp/cmd/listtools -json                # 完整 JSON（含 schema）
//	go run ./mcp/cmd/listtools -call pandora.is_app_exist -args '{"cluster":"hba","app":"foo"}'
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fzxbl/mcp-toolify/example/tools"
	"github.com/fzxbl/mcp-toolify/runtime"
)

func main() {
	var (
		enable  = flag.String("enable", "", "包白名单，逗号分隔；空=全部")
		tags    = flag.String("tags", "", "tag 过滤，逗号分隔；空=全部")
		short   = flag.Bool("short", false, "紧凑输出：每行一个工具，不打印 schema")
		jsonOut = flag.Bool("json", false, "JSON 完整输出（包括 schema）")
		grep    = flag.String("grep", "", "只显示 name 包含该子串的工具")
		call    = flag.String("call", "", "调用工具：填写 tool name；与 -args 配合")
		args    = flag.String("args", "{}", "调用工具时的 JSON 入参，配合 -call")
	)
	flag.Parse()

	srv := mcp.NewServer(&mcp.Implementation{Name: "listtools", Version: "0"}, nil)
	tools.RegisterAll(srv, runtime.RegisterOptions{
		Enable: splitCSV(*enable),
		Tags:   splitCSV(*tags),
	})

	srvT, cliT := mcp.NewInMemoryTransports()
	go srv.Run(context.Background(), srvT)

	cli := mcp.NewClient(&mcp.Implementation{Name: "listtools", Version: "0"}, nil)
	sess, err := cli.Connect(context.Background(), cliT, nil)
	must(err)
	defer sess.Close()

	if *call != "" {
		callTool(sess, *call, *args)
		return
	}

	res, err := sess.ListTools(context.Background(), nil)
	must(err)

	list := res.Tools
	if *grep != "" {
		filtered := list[:0]
		for _, t := range list {
			if strings.Contains(t.Name, *grep) {
				filtered = append(filtered, t)
			}
		}
		list = filtered
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })

	switch {
	case *jsonOut:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		must(enc.Encode(list))
	case *short:
		for _, t := range list {
			fmt.Printf("%-48s %s\n", t.Name, oneLine(t.Description, 100))
		}
	default:
		printDetailed(list)
	}
	fmt.Fprintf(os.Stderr, "\ntotal: %d tools\n", len(list))
}

func printDetailed(list []*mcp.Tool) {
	for i, t := range list {
		if i > 0 {
			fmt.Println(strings.Repeat("─", 80))
		}
		fmt.Printf("● %s\n", t.Name)
		if d := strings.TrimSpace(t.Description); d != "" {
			fmt.Printf("  description:\n")
			for _, line := range strings.Split(d, "\n") {
				fmt.Printf("    %s\n", line)
			}
		}
		printSchema("  input", t.InputSchema)
		printSchema("  output", t.OutputSchema)
	}
}

// printSchema 直接把 server 返回的 schema 原样 JSON 缩进打印，
// 不做任何本地结构体解析，避免丢字段（如类型联合 type:[...]）。
func printSchema(label string, schemaAny any) {
	if schemaAny == nil {
		return
	}
	b, err := json.MarshalIndent(schemaAny, "    ", "  ")
	if err != nil {
		return
	}
	if string(b) == "null" || string(b) == "{}" {
		return
	}
	fmt.Printf("%s:\n    %s\n", label, b)
}

func oneLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

func callTool(sess *mcp.ClientSession, name, argsJSON string) {
	var argMap map[string]any
	must(json.Unmarshal([]byte(argsJSON), &argMap))
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: argMap,
	})
	must(err)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	must(enc.Encode(res))
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
