package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestOneLineKeepsValidUTF8：-short 的一行摘要按字节数截断，中文描述会被切在 rune 中间，
// 整行输出随之变成非法 UTF-8（README 现在明确推荐 -short，实测 greeter.shout 的中文描述
// 就会踩到）。截断后必须仍是合法 UTF-8。
func TestOneLineKeepsValidUTF8(t *testing.T) {
	// 每个汉字 3 字节，取 max=10 保证一定切在 rune 中间。
	const desc = "查询某个对象的当前状态与用量"
	got := oneLine(desc, 10)
	if !utf8.ValidString(got) {
		t.Errorf("截断结果必须是合法 UTF-8，实际 %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("截断后应带省略号，实际 %q", got)
	}
	// 只能少一个不完整的字，不该把已完整的字也丢掉。
	if want := "查询某"; !strings.HasPrefix(got, want) {
		t.Errorf("got %q，want 以 %q 开头", got, want)
	}
}

// TestOneLineLeavesShortTextAlone：没超限的描述原样返回，避免实现退化成「一律截断」也能过。
func TestOneLineLeavesShortTextAlone(t *testing.T) {
	if got := oneLine("  hello\nworld  ", 100); got != "hello" {
		t.Errorf("got %q，want %q（去空白 + 只取第一行、不加省略号）", got, "hello")
	}
}
