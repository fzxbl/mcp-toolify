package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEstimateTokens 验证估算规则：ASCII 约 4 字符 1 token，非 ASCII 每字符 1 token。
func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"ascii8", "abcdefgh", 2},
		{"ascii_round_up", "abcde", 2},
		{"cjk", "中文测试", 4},
		{"mixed", "abcd中文", 3},
		{"invalid_utf8", string([]byte{0xff, 0xfe}), 2},
	}
	for _, c := range cases {
		if got := estimateTokens([]byte(c.in)); got != c.want {
			t.Errorf("%s: estimateTokens(%q) = %d, want %d", c.name, c.in, got, c.want)
		}
	}
}

// TestLoadSpillConfig 验证从 mcp.toml 的 [spill] 段读取阈值。
func TestLoadSpillConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.toml")
	content := `
[[tokens]]
token = "ro-1"
read = "high"

[spill]
max_result_tokens = 1234
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadSpillConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MaxResultTokens != 1234 {
		t.Errorf("MaxResultTokens = %d, want 1234", cfg.MaxResultTokens)
	}
}

// TestLoadSpillConfigMissingSection 验证 toml 中没有 [spill] 段时得到零值（由调用方归一化）。
func TestLoadSpillConfigMissingSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.toml")
	content := `
[[tokens]]
token = "ro-1"
read = "high"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadSpillConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MaxResultTokens != 0 {
		t.Errorf("MaxResultTokens = %d, want 0", cfg.MaxResultTokens)
	}
}

// TestInitSpillConfigDefaults 覆盖阈值来源：文件缺失 / 配置 0 / 显式 -1 / 解析失败。
func TestInitSpillConfigDefaults(t *testing.T) {
	// 阈值是进程级全局状态，用 Cleanup 统一恢复默认值，避免 t.Fatal 提前退出时把 -1 泄漏给后续测试。
	t.Cleanup(func() { SetSpillThreshold(defaultMaxResultTokens) })

	dir := t.TempDir()

	InitSpillConfig(filepath.Join(dir, "missing.toml"))
	if got := spillThreshold(); got != defaultMaxResultTokens {
		t.Errorf("missing file: threshold = %d, want %d", got, defaultMaxResultTokens)
	}

	zero := filepath.Join(dir, "zero.toml")
	if err := os.WriteFile(zero, []byte("[spill]\nmax_result_tokens = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	InitSpillConfig(zero)
	if got := spillThreshold(); got != defaultMaxResultTokens {
		t.Errorf("zero: threshold = %d, want %d", got, defaultMaxResultTokens)
	}

	off := filepath.Join(dir, "off.toml")
	if err := os.WriteFile(off, []byte("[spill]\nmax_result_tokens = -1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	InitSpillConfig(off)
	if got := spillThreshold(); got != -1 {
		t.Errorf("off: threshold = %d, want -1", got)
	}

	// 小于 -1 的可疑配置归一化为 -1（并打告警日志）。
	SetSpillThreshold(-100)
	if got := spillThreshold(); got != -1 {
		t.Errorf("negative: threshold = %d, want -1", got)
	}

	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte("[spill\nmax_result_tokens = "), 0o644); err != nil {
		t.Fatal(err)
	}
	InitSpillConfig(bad)
	if got := spillThreshold(); got != defaultMaxResultTokens {
		t.Errorf("bad: threshold = %d, want %d", got, defaultMaxResultTokens)
	}
}
