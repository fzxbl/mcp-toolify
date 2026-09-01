// Package selector 实现 k8s label selector 语法的解析与匹配。
// 所有 label 值按字符串处理，无序关系；操作符：= != in notin Exists(!)DoesNotExist。
//
// 与 k8s 的差异（刻意收敛）：in/notin 的操作符前后必须各有一个空格，
// 即只接受 "key in (a,b)" 这种写法，不接受 "key in(a,b)"。
//
// 新增操作符时需同步改三处：常量块、parse 的分支、match 的 switch。
package selector

import (
	"fmt"
	"strings"
)

// Op 是单个条件的操作符。
type Op string

// 支持的操作符集合。除 OpExists 外，常量值都是表达式里的真实字面 token，
// 会被直接打进解析错误信息，改动即改动对外错误文案。
const (
	OpEqual    Op = "="
	OpNotEqual Op = "!="
	OpIn       Op = "in"
	OpNotIn    Op = "notin"
	// OpExists 没有对应的字面语法 token，裸 key（如 "risk"）即表示存在性判定。
	OpExists   Op = "exists"
	OpNotExist Op = "!"
)

// Requirement 是一个条件，如 risk in (low,high)。
type Requirement struct {
	Key    string
	Op     Op
	Values []string
}

// Selector 是若干条件的 AND 组合。
// 零值非法：必须经 Parse 构造，直接用 Selector{} 会被 Match 判为不匹配。
type Selector struct {
	// Raw 是解析前的原始表达式，供基座做错误回显与启动日志打印生效规则，勿删。
	Raw          string
	Requirements []Requirement
}

// Parse 解析 selector 字符串；空串或语法错误返回 error。
// 出错时返回零值 Selector（不匹配任何 labels），确保调用方跳过坏规则时是 fail-closed。
func Parse(expr string) (Selector, error) {
	if strings.TrimSpace(expr) == "" {
		return Selector{}, fmt.Errorf("empty selector")
	}
	parts, err := splitTop(expr)
	if err != nil {
		return Selector{}, fmt.Errorf("selector %q: %w", expr, err)
	}
	s := Selector{Raw: expr}
	for _, part := range parts {
		req, err := parseRequirement(strings.TrimSpace(part))
		if err != nil {
			return Selector{}, fmt.Errorf("selector %q: %w", expr, err)
		}
		s.Requirements = append(s.Requirements, req)
	}
	return s, nil
}

// splitTop 按逗号切分，但括号内的逗号不切；括号不配平返回 error。
func splitTop(expr string) ([]string, error) {
	var out []string
	depth, start := 0, 0
	for i, r := range expr {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unbalanced ')' at %d", i)
			}
		case ',':
			if depth == 0 {
				out = append(out, expr[start:i])
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unclosed '('")
	}
	return append(out, expr[start:]), nil
}

// ValidateLabelKey 校验 label key 的字符集，供生成期（mcpgen）与解析期共用同一套规则。
// 语法规则的所有权在本包：调用方不要另行实现，否则规则会漂移出「能标注但永远匹配不上」的输入。
func ValidateLabelKey(k string) error { return validateKey(k) }

// ValidateLabelValue 校验 label value 的字符集，与 ValidateLabelKey 同理。
func ValidateLabelValue(v string) error { return validateValue(v) }

// validateKey 校验 label key 的字符集：非空，且不含操作符字符与空白。
// 缺了这道校验，"!=high" 之类的畸形表达式会被解析成一条恒真/恒假的规则并静默生效。
func validateKey(k string) error {
	if k == "" {
		return fmt.Errorf("missing label key")
	}
	if strings.ContainsAny(k, "=!(),") || strings.ContainsAny(k, " \t\r\n\v\f") {
		return fmt.Errorf("invalid label key %q", k)
	}
	return nil
}

// validateValue 校验 label value 的字符集：不含操作符字符。
// 刻意不复用 validateKey：value 允许含空格（如 risk=very high 是已固化的契约）。
// 缺了这道校验，"risk!==high" 会被解析成 值为 "=high" 的恒真规则并静默生效。
func validateValue(v string) error {
	if strings.ContainsAny(v, "=!(),") {
		return fmt.Errorf("invalid label value %q", v)
	}
	return nil
}

// parseRequirement 解析单个条件。
func parseRequirement(s string) (Requirement, error) {
	if s == "" {
		return Requirement{}, fmt.Errorf("empty requirement")
	}
	if strings.HasPrefix(s, "!=") {
		return Requirement{}, fmt.Errorf("missing key before '!='")
	}
	if strings.HasPrefix(s, "!") {
		key := strings.TrimSpace(s[1:])
		if err := validateKey(key); err != nil {
			return Requirement{}, err
		}
		return Requirement{Key: key, Op: OpNotExist}, nil
	}
	for _, op := range []Op{OpIn, OpNotIn} {
		if i := strings.Index(s, " "+string(op)+" "); i > 0 {
			key := strings.TrimSpace(s[:i])
			if err := validateKey(key); err != nil {
				return Requirement{}, err
			}
			rest := strings.TrimSpace(s[i+len(string(op))+2:])
			if !strings.HasPrefix(rest, "(") || !strings.HasSuffix(rest, ")") {
				return Requirement{}, fmt.Errorf("%s values must be wrapped in ()", op)
			}
			var vals []string
			for _, v := range strings.Split(rest[1:len(rest)-1], ",") {
				if v = strings.TrimSpace(v); v != "" {
					if err := validateValue(v); err != nil {
						return Requirement{}, err
					}
					vals = append(vals, v)
				}
			}
			if len(vals) == 0 {
				return Requirement{}, fmt.Errorf("%s needs at least one value", op)
			}
			return Requirement{Key: key, Op: op, Values: vals}, nil
		}
	}
	return parseComparison(s)
}

// parseComparison 解析 = / != / Exists 三种形式。注意 != 必须先于 = 判断。
func parseComparison(s string) (Requirement, error) {
	if i := strings.Index(s, "!="); i > 0 {
		return kv(s[:i], s[i+2:], OpNotEqual)
	}
	if i := strings.Index(s, "="); i > 0 {
		if strings.Contains(s, "==") {
			return Requirement{}, fmt.Errorf("unsupported operator '=='")
		}
		return kv(s[:i], s[i+1:], OpEqual)
	}
	if err := validateKey(s); err != nil {
		// 走到这里说明既没有 = 也没有 != ，且 key 字符集非法。含空白时更可能是
		// 敲错了操作符（risk lt high）或 in/notin 缺空格（risk notin(a)），据实指出。
		if strings.ContainsAny(s, " \t\r\n\v\f") {
			return Requirement{}, fmt.Errorf("cannot parse %q: unknown operator or missing spaces around in/notin", s)
		}
		return Requirement{}, err
	}
	return Requirement{Key: s, Op: OpExists}, nil
}

// kv 构造带单个值的条件，校验 key 与 value 的字符集。
func kv(k, v string, op Op) (Requirement, error) {
	k, v = strings.TrimSpace(k), strings.TrimSpace(v)
	if err := validateKey(k); err != nil {
		return Requirement{}, err
	}
	if v == "" {
		return Requirement{}, fmt.Errorf("missing value after %q", op)
	}
	if err := validateValue(v); err != nil {
		return Requirement{}, err
	}
	return Requirement{Key: k, Op: op, Values: []string{v}}, nil
}
