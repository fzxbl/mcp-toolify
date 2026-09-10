package selector

import (
	"reflect"
	"testing"
)

// TestParse 断言解析出的 Requirement 结构，防止表达式被错位解析成
// 一条恒真/恒假的规则（这类错位在纯 Match 断言下是看不出来的）。
func TestParse(t *testing.T) {
	cases := []struct {
		expr string
		want []Requirement
	}{
		{"risk=high", []Requirement{{Key: "risk", Op: OpEqual, Values: []string{"high"}}}},
		{"risk!=high", []Requirement{{Key: "risk", Op: OpNotEqual, Values: []string{"high"}}}},
		{"risk in (low,high)", []Requirement{{Key: "risk", Op: OpIn, Values: []string{"low", "high"}}}},
		{"risk notin (low,high)", []Requirement{{Key: "risk", Op: OpNotIn, Values: []string{"low", "high"}}}},
		{"risk", []Requirement{{Key: "risk", Op: OpExists}}},
		{"!risk", []Requirement{{Key: "risk", Op: OpNotExist}}},
		{"risk=high,capability=write", []Requirement{
			{Key: "risk", Op: OpEqual, Values: []string{"high"}},
			{Key: "capability", Op: OpEqual, Values: []string{"write"}},
		}},
		{"risk = high", []Requirement{{Key: "risk", Op: OpEqual, Values: []string{"high"}}}},
		{"risk=very high", []Requirement{{Key: "risk", Op: OpEqual, Values: []string{"very high"}}}},
		{"risk in (a , b)", []Requirement{{Key: "risk", Op: OpIn, Values: []string{"a", "b"}}}},
		// 重复 key 被接受，结果是一条恒不匹配的规则（刻意不报错）。
		{"risk=high,risk=low", []Requirement{
			{Key: "risk", Op: OpEqual, Values: []string{"high"}},
			{Key: "risk", Op: OpEqual, Values: []string{"low"}},
		}},
	}
	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			s, err := Parse(c.expr)
			if err != nil {
				t.Errorf("Parse(%q): %v", c.expr, err)
				return
			}
			if !reflect.DeepEqual(s.Requirements, c.want) {
				t.Errorf("Parse(%q) = %+v, want %+v", c.expr, s.Requirements, c.want)
			}
			if s.Raw != c.expr {
				t.Errorf("Parse(%q).Raw = %q", c.expr, s.Raw)
			}
		})
	}
}

func TestParseError(t *testing.T) {
	exprs := []string{
		"risk==high", "risk in low,high", "risk lt high",
		"!=high", "!!risk", "a),b=c", "a b in (c)", "risk in (a,b", "risk in ()",
		"risk!==high", "risk!=high=x", "risk!=!high", "risk in (a,!b)",
	}
	for _, expr := range exprs {
		t.Run(expr, func(t *testing.T) {
			s, err := Parse(expr)
			if err == nil {
				t.Errorf("Parse(%q) expected error, got %+v", expr, s.Requirements)
				return
			}
			// 出错时必须是 fail-closed 的零值，不能是 match-all。
			if s.Match(map[string]string{"risk": "high"}) {
				t.Errorf("Parse(%q) error path returned a match-all selector", expr)
			}
		})
	}
}

// TestParseEmptyMatchesEverything：空 selector 按 k8s 语义匹配一切，且不是错误。
// 同时钉住「零值不是 match-all」：解析失败返回的就是零值，那条路必须 fail-closed。
func TestParseEmptyMatchesEverything(t *testing.T) {
	for _, expr := range []string{"", "   "} {
		s, err := Parse(expr)
		if err != nil {
			t.Fatalf("Parse(%q) = %v, want nil", expr, err)
		}
		if !s.MatchesEverything() {
			t.Errorf("Parse(%q).MatchesEverything() = false", expr)
		}
		for _, labels := range []map[string]string{
			nil, {}, {"risk": "high"}, {"capability": "read", "pkg": "demo"},
		} {
			if !s.Match(labels) {
				t.Errorf("Parse(%q).Match(%v) = false, want true", expr, labels)
			}
		}
	}
	var zero Selector
	if zero.Match(map[string]string{"risk": "high"}) || zero.MatchesEverything() {
		t.Error("零值 Selector 必须不匹配任何 labels（解析失败走的就是这条路）")
	}
}

// TestMatch 用手工构造的 Requirement 断言匹配语义，不经过 Parse。
func TestMatch(t *testing.T) {
	labels := map[string]string{"risk": "high", "capability": "write"}
	cases := []struct {
		name string
		reqs []Requirement
		want bool
	}{
		{"risk=high", []Requirement{{Key: "risk", Op: OpEqual, Values: []string{"high"}}}, true},
		{"risk=low", []Requirement{{Key: "risk", Op: OpEqual, Values: []string{"low"}}}, false},
		{"risk!=low", []Requirement{{Key: "risk", Op: OpNotEqual, Values: []string{"low"}}}, true},
		{"risk in (low,high)", []Requirement{{Key: "risk", Op: OpIn, Values: []string{"low", "high"}}}, true},
		{"risk notin (low,medium)", []Requirement{{Key: "risk", Op: OpNotIn, Values: []string{"low", "medium"}}}, true},
		{"risk exists", []Requirement{{Key: "risk", Op: OpExists}}, true},
		{"!risk", []Requirement{{Key: "risk", Op: OpNotExist}}, false},
		// 缺失 key 的四种语义。
		{"dangerous!=true", []Requirement{{Key: "dangerous", Op: OpNotEqual, Values: []string{"true"}}}, true},
		{"dangerous notin (true)", []Requirement{{Key: "dangerous", Op: OpNotIn, Values: []string{"true"}}}, true},
		{"dangerous=true", []Requirement{{Key: "dangerous", Op: OpEqual, Values: []string{"true"}}}, false},
		{"dangerous in (true)", []Requirement{{Key: "dangerous", Op: OpIn, Values: []string{"true"}}}, false},
		{"!dangerous", []Requirement{{Key: "dangerous", Op: OpNotExist}}, true},
		{"AND both true", []Requirement{
			{Key: "risk", Op: OpEqual, Values: []string{"high"}},
			{Key: "capability", Op: OpEqual, Values: []string{"write"}},
		}, true},
		{"AND one false", []Requirement{
			{Key: "risk", Op: OpEqual, Values: []string{"high"}},
			{Key: "capability", Op: OpEqual, Values: []string{"read"}},
		}, false},
		// Values 为空的手工构造不得 panic，按 false 处理。
		{"equal without values", []Requirement{{Key: "risk", Op: OpEqual}}, false},
		{"notequal without values", []Requirement{{Key: "risk", Op: OpNotEqual}}, false},
		{"in without values", []Requirement{{Key: "risk", Op: OpIn}}, false},
		{"notin without values", []Requirement{{Key: "risk", Op: OpNotIn}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Selector{Requirements: c.reqs}
			if got := s.Match(labels); got != c.want {
				t.Errorf("Match(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

// TestZeroSelectorFailsClosed 固化零值 Selector 不匹配任何 labels。
func TestZeroSelectorFailsClosed(t *testing.T) {
	if (Selector{}).Match(map[string]string{"risk": "high"}) {
		t.Error("zero Selector matched, want fail-closed")
	}
	if (Selector{}).Match(nil) {
		t.Error("zero Selector matched nil labels, want fail-closed")
	}
}

func TestMatchAny(t *testing.T) {
	labels := map[string]string{"risk": "high"}
	low := Selector{Requirements: []Requirement{{Key: "risk", Op: OpEqual, Values: []string{"low"}}}}
	high := Selector{Requirements: []Requirement{{Key: "risk", Op: OpEqual, Values: []string{"high"}}}}
	none := Selector{Requirements: []Requirement{{Key: "risk", Op: OpNotExist}}}

	if MatchAny(nil, labels) {
		t.Error("MatchAny(nil) = true, want false")
	}
	if !MatchAny([]Selector{low, high}, labels) {
		t.Error("MatchAny(one hit) = false, want true")
	}
	if MatchAny([]Selector{low, none}, labels) {
		t.Error("MatchAny(no hit) = true, want false")
	}
}
