package runtime

import (
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/fzxbl/mcp-toolify/selector"
)

// RegisterOptions 控制注册哪些生成的工具。
//
// 生成代码按值接收它并逐个工具调用 Allow，因此新增字段必须是「拷贝安全」的：
// 需要跨拷贝共享的可变状态（如计数器）只能放指针。
type RegisterOptions struct {
	Enable []string // 包名白名单，空表示不过滤
	Match  string   // selector 字符串，空表示不过滤

	// matcher 是 Match 的预编译结果，由 Compile 填充。非 nil 时 Allow 直接用它，
	// 避免每个工具重复 Parse 同一个表达式。
	matcher *selector.Selector
	// counts 记录本轮注册的放行/过滤数，由 Compile 分配。基座据此在启动日志里
	// 暴露「selector 合法但 label 拼错导致 0 匹配」这类 Parse 抓不到的事故。
	counts *registerCounts
}

// registerCounts 是一轮注册的计数器，被 RegisterOptions 的各份拷贝共享。
type registerCounts struct {
	registered atomic.Int64
	filtered   atomic.Int64
}

// Compile 预编译 Match 并分配计数器，返回可直接交给 registrar 的副本。
// Match 语法错误在此报出——启动期失败远好过「服务起来了但 tools/list 是空的」。
// 纯空白的 Match 视同未配置（不过滤），否则配置里多打一个空格就会翻转语义。
func (o RegisterOptions) Compile() (RegisterOptions, error) {
	o.counts = &registerCounts{}
	if strings.TrimSpace(o.Match) == "" {
		o.matcher = nil
		return o, nil
	}
	s, err := selector.Parse(o.Match)
	if err != nil {
		return o, fmt.Errorf("match selector: %w", err)
	}
	o.matcher = &s
	return o, nil
}

// Counts 返回本轮注册的放行数与被过滤数；未经 Compile 时均为 0。
func (o RegisterOptions) Counts() (registered, filtered int) {
	if o.counts == nil {
		return 0, 0
	}
	return int(o.counts.registered.Load()), int(o.counts.filtered.Load())
}

// Allow 判定某包某 labels 的工具是否应被注册。
// 基座只额外注入内置 label pkg（包名），其余 label 语义一概不解释。
//
// 未经 Compile 时（直接构造 RegisterOptions 的调用方）现场 Parse，
// 语法错误 fail-closed 返回 false。
func (o RegisterOptions) Allow(pkg string, labels map[string]string) bool {
	ok := o.allow(pkg, labels)
	if o.counts != nil {
		if ok {
			o.counts.registered.Add(1)
		} else {
			o.counts.filtered.Add(1)
		}
	}
	return ok
}

func (o RegisterOptions) allow(pkg string, labels map[string]string) bool {
	if len(o.Enable) > 0 && !contains(o.Enable, pkg) {
		return false
	}
	s := o.matcher
	if s == nil {
		if strings.TrimSpace(o.Match) == "" {
			return true
		}
		parsed, err := selector.Parse(o.Match)
		if err != nil {
			return false
		}
		s = &parsed
	}
	full := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		full[k] = v
	}
	full["pkg"] = pkg
	return s.Match(full)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
