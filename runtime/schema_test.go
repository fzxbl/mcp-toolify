package runtime

import "testing"

type relaxProbeSpec struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

type relaxProbeInput struct {
	Cluster   string         `json:"cluster"`
	Operation relaxProbeSpec `json:"operation"`
	Raw       any            `json:"raw"`
	List      []any          `json:"list"`
	M         map[string]any `json:"m"`
	Plain     int            `json:"plain"`
}

func TestAnyInputSchema_RelaxesInterfaceNodes(t *testing.T) {
	s := AnyInputSchema[relaxProbeInput]()

	// 顶层 raw (interface{}) 应被放宽为 anyTypes
	raw := s.Properties["raw"]
	if raw == nil {
		t.Fatal("missing property raw")
	}
	if len(raw.Types) != len(anyTypes) {
		t.Errorf("raw.Types = %v, want %v", raw.Types, anyTypes)
	}

	// operation.value (interface{}) 应被放宽
	op := s.Properties["operation"]
	if op == nil {
		t.Fatal("missing property operation")
	}
	val := op.Properties["value"]
	if val == nil {
		t.Fatal("missing operation.value")
	}
	if len(val.Types) != len(anyTypes) {
		t.Errorf("operation.value.Types = %v, want %v", val.Types, anyTypes)
	}

	// list ([]any) 的 items 应被放宽
	list := s.Properties["list"]
	if list == nil || list.Items == nil {
		t.Fatal("missing list/items")
	}
	if len(list.Items.Types) != len(anyTypes) {
		t.Errorf("list.Items.Types = %v, want %v", list.Items.Types, anyTypes)
	}

	// m (map[string]any) 的 additionalProperties 应被放宽
	m := s.Properties["m"]
	if m == nil || m.AdditionalProperties == nil {
		t.Fatal("missing m/additionalProperties")
	}
	if len(m.AdditionalProperties.Types) != len(anyTypes) {
		t.Errorf("m.additionalProperties.Types = %v, want %v", m.AdditionalProperties.Types, anyTypes)
	}

	// plain (int) 不应被改动
	plain := s.Properties["plain"]
	if plain == nil {
		t.Fatal("missing plain")
	}
	if plain.Type != "integer" {
		t.Errorf("plain.Type = %q, want integer", plain.Type)
	}
	if len(plain.Types) != 0 {
		t.Errorf("plain.Types should be empty, got %v", plain.Types)
	}
}
