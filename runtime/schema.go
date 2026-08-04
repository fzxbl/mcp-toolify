package runtime

import "github.com/google/jsonschema-go/jsonschema"

// anyTypes 是 interface{} 字段在 JSON Schema 中的半受限类型联合：
// 接受任意 JSON 值（对象/数组/字符串/数字/布尔/null），但显式列出类型，
// 以便给模型类型提示，而不是落到完全无约束的空 schema。
var anyTypes = []string{"object", "array", "string", "number", "boolean", "null"}

// AnyInputSchema 反射出 T 的输入 schema，并把其中所有「无类型约束」节点
// （interface{} / []any 的 items / map[string]any 的 additionalProperties 等）
// 放宽为 anyTypes 类型联合。生成器对含 interface{} 入参的工具用它显式设置
// Tool.InputSchema，从而绕过 SDK 默认反射产生的空 schema。
func AnyInputSchema[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic("runtime.AnyInputSchema: " + err.Error())
	}
	relaxAnySchema(s)
	return s
}

// relaxAnySchema 递归遍历 schema，把无类型约束的节点改为 anyTypes 联合。
func relaxAnySchema(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if isUnrestricted(s) {
		s.Types = append([]string(nil), anyTypes...)
	}
	for _, child := range s.Properties {
		relaxAnySchema(child)
	}
	for _, child := range s.PatternProperties {
		relaxAnySchema(child)
	}
	relaxAnySchema(s.AdditionalProperties)
	relaxAnySchema(s.Items)
	for _, child := range s.ItemsArray {
		relaxAnySchema(child)
	}
	for _, child := range s.PrefixItems {
		relaxAnySchema(child)
	}
	relaxAnySchema(s.Contains)
	for _, child := range s.AllOf {
		relaxAnySchema(child)
	}
	for _, child := range s.AnyOf {
		relaxAnySchema(child)
	}
	for _, child := range s.OneOf {
		relaxAnySchema(child)
	}
}

// isUnrestricted 判断节点是否完全没有类型约束（即 interface{} 反射出的空 schema）。
// 只要带有任意一种类型约束关键字，就视为已约束，不放宽。
func isUnrestricted(s *jsonschema.Schema) bool {
	if s == nil {
		return false
	}
	if s.Type != "" || len(s.Types) > 0 {
		return false
	}
	if len(s.Enum) > 0 || s.Const != nil || s.Ref != "" {
		return false
	}
	if len(s.AnyOf) > 0 || len(s.OneOf) > 0 || len(s.AllOf) > 0 || s.Not != nil {
		return false
	}
	if len(s.Properties) > 0 || len(s.PatternProperties) > 0 || s.AdditionalProperties != nil {
		return false
	}
	if s.Items != nil || len(s.ItemsArray) > 0 || len(s.PrefixItems) > 0 || s.Contains != nil {
		return false
	}
	return true
}
