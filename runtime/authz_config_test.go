package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAuthzConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.toml")
	content := `
[[tokens]]
token = "ro-1"
name = "readonly-agent"
applicant = "alice"
read = "high"

[[tokens]]
token = "rw-1"
name = "readwrite-agent"
applicant = "alice"
read = "high"
write = "low"

[risk_allowlist]
medium = ["zhangsan", "lisi"]
high = ["zhangsan"]
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAuthzConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Tokens) != 2 {
		t.Fatalf("tokens = %d", len(cfg.Tokens))
	}
	if cfg.RiskAllowlist["high"][0] != "zhangsan" {
		t.Errorf("high allowlist = %v", cfg.RiskAllowlist["high"])
	}
	if cfg.Tokens[0].Name != "readonly-agent" || cfg.Tokens[0].Applicant != "alice" {
		t.Errorf("tokens[0] name/applicant = %q/%q", cfg.Tokens[0].Name, cfg.Tokens[0].Applicant)
	}
	if cfg.Tokens[1].Name != "readwrite-agent" || cfg.Tokens[1].Applicant != "alice" {
		t.Errorf("tokens[1] name/applicant = %q/%q", cfg.Tokens[1].Name, cfg.Tokens[1].Applicant)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate on complete config: %v", err)
	}
	a := NewAuthz(cfg)
	caps, ok := a.CapsOf("rw-1")
	if !ok || !caps.ReadOK || caps.Read != RiskHigh || !caps.WriteOK || caps.Write != RiskLow {
		t.Errorf("rw-1 caps = %+v,%v", caps, ok)
	}
	if caps.Name != "readwrite-agent" {
		t.Errorf("rw-1 caps.Name = %q", caps.Name)
	}
}
