package runtime

import "testing"

func TestParseRisk(t *testing.T) {
	cases := map[string]RiskLevel{
		"":       RiskNone,
		"none":   RiskNone,
		"low":    RiskLow,
		"medium": RiskMedium,
		"high":   RiskHigh,
	}
	for in, want := range cases {
		got, ok := ParseRisk(in)
		if !ok || got != want {
			t.Errorf("ParseRisk(%q) = %v,%v want %v", in, got, ok, want)
		}
	}
	if _, ok := ParseRisk("critical"); ok {
		t.Errorf("critical should be invalid")
	}
}

func TestRiskString(t *testing.T) {
	if RiskHigh.String() != "high" {
		t.Errorf("RiskHigh.String() = %q", RiskHigh.String())
	}
}
