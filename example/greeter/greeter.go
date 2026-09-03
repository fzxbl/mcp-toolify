// Package greeter 是用 mcp-toolify 暴露工具的最小示例。
// 在函数 godoc 上加 `// mcp:tool` 再跑 `go generate`，它就成了一个 MCP 工具。
//
// labels 的 key 语义由部署方的配置解释（基座不解释）：本示例沿用
// capability / risk 两个 key，与 example/conf/mcp.toml 里的 token 准入、
// 配额规则、二次确认规则一致。
package greeter

import "fmt"

// Greet 生成一句问候语。
//
// param: name — 要问候的名字
// param: excited — 是否在结尾加感叹号
//
// mcp:tool
// mcp:labels=capability=read,risk=none
func Greet(name string, excited bool) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	msg := "Hello, " + name
	if excited {
		msg += "!"
	}
	return msg, nil
}

// AddNumbers 返回两个整数之和（演示多返回值按变量名打包）。
//
// param: a — 第一个加数
// param: b — 第二个加数
//
// mcp:tool
// mcp:labels=capability=read,risk=none
func AddNumbers(a int, b int) (sum int) {
	return a + b
}

// Shout 把一句话改成全大写（示例里的「高危写操作」）。
//
// 它本身当然是无害的，标成 capability=write,risk=high 只是为了让示例真的走一遍准入判定：
// example/conf/mcp.toml 里只读通道看不见它、运维通道才能执行。按 label 管辖的插件
// （配额、审批一类）也用同一组 label，把它换成你自己那个真会改线上状态的函数即可。
//
// param: text — 要改写的文本
//
// mcp:tool
// mcp:labels=capability=write,risk=high
func Shout(text string) (string, error) {
	if text == "" {
		return "", fmt.Errorf("text is required")
	}
	out := make([]rune, 0, len(text))
	for _, r := range text {
		if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		out = append(out, r)
	}
	return string(out), nil
}
