package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// writeMCPConf 在 <dir>/mcp.toml 写入配置内容，返回该文件路径，供 Config.ConfigPath 使用。
func writeMCPConf(t *testing.T, dir string, content string) string {
	t.Helper()
	path := filepath.Join(dir, "mcp.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMCPHandler_AuthzConfigValidation 是接线测试：确认 MCPHandler 真的把
// AuthzConfig.Validate() 串在启动路径上——配置缺 name 时必须返回 error（启动失败），
// 齐全时正常返回 handler。删掉 mount.go 里的 Validate() 调用，本用例会失败。
func TestMCPHandler_AuthzConfigValidation(t *testing.T) {
	t.Run("missing name fails startup", func(t *testing.T) {
		dir := t.TempDir()
		path := writeMCPConf(t, dir, `
[[tokens]]
token = "ro-1"
applicant = "alice"
read = "medium"
`)

		s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
		h, err := MCPHandler(Config{AuthzEnabled: true, ConfigPath: path}, s)
		if err == nil {
			t.Fatalf("expected error for authz config missing name, got handler %v", h)
		}
		if !strings.Contains(err.Error(), "invalid mcp authz config") {
			t.Errorf("error = %v, want it to contain 'invalid mcp authz config'", err)
		}
	})

	t.Run("complete config starts", func(t *testing.T) {
		dir := t.TempDir()
		path := writeMCPConf(t, dir, `
[[tokens]]
token = "ro-1"
name = "readonly-agent"
applicant = "alice"
read = "medium"
`)

		s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
		h, err := MCPHandler(Config{AuthzEnabled: true, ConfigPath: path}, s)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h == nil {
			t.Fatalf("handler is nil")
		}
	})
}
