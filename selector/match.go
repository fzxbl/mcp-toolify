package selector

// Match 判定一组 labels 是否满足全部条件（AND）。
// 沿用 k8s 缺失语义：!= 与 notin 对不存在该 key 的对象也算匹配成功。
// 条件为空说明 Selector 未经 Parse 构造（Parse 已拒绝空串），按非法态 fail-closed 返回 false。
func (s Selector) Match(labels map[string]string) bool {
	if len(s.Requirements) == 0 {
		return false
	}
	for _, r := range s.Requirements {
		if !r.match(labels) {
			return false
		}
	}
	return true
}

// match 判定单个条件是否成立。带值的操作符在 Values 为空时返回 false，
// 避免外部手工构造的 Requirement 触发下标越界。
func (r Requirement) match(labels map[string]string) bool {
	if r.Op != OpExists && r.Op != OpNotExist && len(r.Values) == 0 {
		return false
	}
	v, ok := labels[r.Key]
	switch r.Op {
	case OpExists:
		return ok
	case OpNotExist:
		return !ok
	case OpEqual:
		return ok && v == r.Values[0]
	case OpNotEqual:
		return !ok || v != r.Values[0]
	case OpIn:
		return ok && contains(r.Values, v)
	case OpNotIn:
		return !ok || !contains(r.Values, v)
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// MatchAny 判定 labels 是否命中任一 selector（OR）。
func MatchAny(ss []Selector, labels map[string]string) bool {
	for _, s := range ss {
		if s.Match(labels) {
			return true
		}
	}
	return false
}
