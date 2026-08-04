// Package greeter is a tiny example of tools exposed via mcp-toolify.
// Add `// mcp:tool` to a function's godoc and run `go generate` to expose it.
package greeter

import "fmt"

// Greet 生成一句问候语。
//
// param: name — 要问候的名字
// param: excited — 是否在结尾加感叹号
//
// mcp:tool
// mcp:tags=read
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
func AddNumbers(a int, b int) (sum int) {
	return a + b
}
