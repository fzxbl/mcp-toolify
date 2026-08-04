package runtime

import "testing"

func TestRegisterAndLookupMeta(t *testing.T) {
	RegisterMeta(ToolMeta{Name: "pkg.foo", Capability: ReadWrite, Risk: RiskHigh})
	m, ok := LookupMeta("pkg.foo")
	if !ok {
		t.Fatal("meta not found")
	}
	if m.Capability != ReadWrite || m.Risk != RiskHigh {
		t.Errorf("meta = %+v", m)
	}
	if _, ok := LookupMeta("pkg.missing"); ok {
		t.Errorf("missing tool should not be found")
	}
}
