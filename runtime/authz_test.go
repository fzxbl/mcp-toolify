package runtime

import (
	"strings"
	"testing"
)

func newTestAuthz() *Authz {
	return NewAuthz(AuthzConfig{
		Tokens: []TokenEntry{
			{Token: "ro-1", Name: "readonly-agent", Applicant: "gaojian15", Read: "high"},                 // 只读，读到 high，不能写
			{Token: "rw-1", Name: "readwrite-agent", Applicant: "gaojian15", Read: "high", Write: "high"}, // 读写都到 high
		},
		RiskAllowlist: map[string][]string{
			"medium": {"zhangsan", "lisi"},
			"high":   {"zhangsan"},
		},
	})
}

// TestAuthzConfig_Validate 覆盖必填审计元信息校验：缺 name / 缺 applicant / 齐全，
// 并确认错误信息里不会回显 token 值（错误会进日志/终端）。
func TestAuthzConfig_Validate(t *testing.T) {
	const secret = "s3cr3t-token-value"

	t.Run("missing name", func(t *testing.T) {
		cfg := AuthzConfig{Tokens: []TokenEntry{
			{Token: "ok-1", Name: "readonly-agent", Applicant: "gaojian15", Read: "low"},
			{Token: secret, Name: "  ", Applicant: "gaojian15", Read: "low"},
		}}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("expected error for missing name")
		}
		if !strings.Contains(err.Error(), "tokens[1]") {
			t.Errorf("error should locate the entry: %v", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error must not leak token value: %v", err)
		}
	})

	t.Run("missing applicant", func(t *testing.T) {
		cfg := AuthzConfig{Tokens: []TokenEntry{
			{Token: secret, Name: "readonly-agent", Applicant: "", Read: "low"},
		}}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("expected error for missing applicant")
		}
		if !strings.Contains(err.Error(), "tokens[0]") {
			t.Errorf("error should locate the entry: %v", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error must not leak token value: %v", err)
		}
	})

	t.Run("complete", func(t *testing.T) {
		cfg := AuthzConfig{Tokens: []TokenEntry{
			{Token: secret, Name: "readonly-agent", Applicant: "gaojian15", Read: "low"},
		}}
		if err := cfg.Validate(); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("no tokens", func(t *testing.T) {
		if err := (AuthzConfig{}).Validate(); err != nil {
			t.Errorf("empty config should pass: %v", err)
		}
	})

	t.Run("empty token", func(t *testing.T) {
		cfg := AuthzConfig{Tokens: []TokenEntry{
			{Token: "  ", Name: "readonly-agent", Applicant: "gaojian15", Read: "low"},
		}}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("expected error for empty token")
		}
		if !strings.Contains(err.Error(), "tokens[0]") {
			t.Errorf("error should locate the entry: %v", err)
		}
	})

	t.Run("duplicate token", func(t *testing.T) {
		cfg := AuthzConfig{Tokens: []TokenEntry{
			{Token: secret, Name: "readonly-agent", Applicant: "gaojian15", Read: "low"},
			{Token: "other", Name: "other-agent", Applicant: "gaojian15", Read: "low"},
			{Token: secret, Name: "readwrite-agent", Applicant: "gaojian15", Read: "low", Write: "low"},
		}}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("expected error for duplicate token")
		}
		if !strings.Contains(err.Error(), "tokens[2]") || !strings.Contains(err.Error(), "tokens[0]") {
			t.Errorf("error should reference both entry indexes: %v", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error must not leak token value: %v", err)
		}
	})
}

func TestAuthz_CapsOf(t *testing.T) {
	a := newTestAuthz()
	rw, ok := a.CapsOf("rw-1")
	if !ok || !rw.ReadOK || rw.Read != RiskHigh || !rw.WriteOK || rw.Write != RiskHigh {
		t.Errorf("rw-1 -> %+v,%v", rw, ok)
	}
	if rw.Name != "readwrite-agent" {
		t.Errorf("rw-1 caps.Name = %q", rw.Name)
	}
	ro, ok := a.CapsOf("ro-1")
	if !ok || !ro.ReadOK || ro.Read != RiskHigh || ro.WriteOK {
		t.Errorf("ro-1 -> %+v,%v", ro, ok)
	}
	if ro.Name != "readonly-agent" {
		t.Errorf("ro-1 caps.Name = %q", ro.Name)
	}
	if _, ok := a.CapsOf("unknown"); ok {
		t.Errorf("unknown token must be rejected")
	}
}

func TestAuthz_CanRun(t *testing.T) {
	a := newTestAuthz()
	if !a.CanRun("", RiskNone) || !a.CanRun("", RiskLow) {
		t.Errorf("none/low should allow anyone")
	}
	if !a.CanRun("lisi", RiskMedium) {
		t.Errorf("lisi should pass medium")
	}
	if a.CanRun("wangwu", RiskMedium) {
		t.Errorf("wangwu should fail medium")
	}
	if !a.CanRun("zhangsan", RiskHigh) {
		t.Errorf("zhangsan should pass high")
	}
	if a.CanRun("lisi", RiskHigh) {
		t.Errorf("lisi should fail high")
	}
	if a.CanRun("", RiskHigh) {
		t.Errorf("empty identity should fail high")
	}
}

// 权限向下覆盖：只配在 high 名单里的人，也能执行 medium。
func TestAuthz_CanRun_Cumulative(t *testing.T) {
	a := NewAuthz(AuthzConfig{RiskAllowlist: map[string][]string{"high": {"boss"}}})
	if !a.CanRun("boss", RiskHigh) {
		t.Errorf("boss should pass high")
	}
	if !a.CanRun("boss", RiskMedium) {
		t.Errorf("high clearance should cover medium")
	}
	if a.CanRun("nobody", RiskMedium) {
		t.Errorf("unlisted should fail medium")
	}
}
