package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

// TestSpillPeerConfigStore 验证 [spill] 段的 peer 字段被解析并落到进程内 peer 状态：
// 共享密钥、超时、静态兄弟副本白名单。
func TestSpillPeerConfigStore(t *testing.T) {
	// peer 状态是进程级全局，测试后恢复默认（关闭代理）。
	t.Cleanup(func() {
		SetSpillPeer("", 0)
		SetSpillPeerProvider(nil)
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.toml")
	content := `
[spill]
max_result_tokens = 4000
peer_token = "s3cret"
peer_timeout_ms = 1200
peer_hosts = ["host-a.example.com:8011", "host-b.example.com:8011"]
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadSpillConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.PeerToken != "s3cret" || cfg.PeerTimeoutMS != 1200 {
		t.Errorf("peer cfg = %q/%d, want s3cret/1200", cfg.PeerToken, cfg.PeerTimeoutMS)
	}
	if len(cfg.PeerHosts) != 2 {
		t.Fatalf("peer_hosts = %v, want 2 entries", cfg.PeerHosts)
	}

	InitSpillConfig(path)
	if got := SpillPeerToken(); got != "s3cret" {
		t.Errorf("SpillPeerToken() = %q, want s3cret", got)
	}
	if got := SpillPeerTimeout(); got != 1200*time.Millisecond {
		t.Errorf("SpillPeerTimeout() = %v, want 1.2s", got)
	}
	if !SpillPeerAllowed("host-a.example.com:8011") {
		t.Error("host-a should be allowed via static peer_hosts")
	}
	if SpillPeerAllowed("evil.example.com:8011") {
		t.Error("unlisted host must not be allowed")
	}

	// 无 [spill] peer 字段时代理关闭：token 空、白名单拒绝一切。
	empty := filepath.Join(dir, "empty.toml")
	if err := os.WriteFile(empty, []byte("[spill]\nmax_result_tokens = 4000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	InitSpillConfig(empty)
	if SpillPeerToken() != "" {
		t.Error("peer token should be empty when unconfigured")
	}
	if SpillPeerAllowed("host-a.example.com:8011") {
		t.Error("no peer_hosts should deny all after reinit")
	}
}
