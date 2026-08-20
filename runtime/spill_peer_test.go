package runtime

import (
	"testing"
)

func TestSpillPeerAllowed(t *testing.T) {
	t.Cleanup(func() { SetSpillPeerProvider(nil) })

	// 静态列表（配置兜底路径）
	SetSpillPeers([]string{"10.0.0.1:8011"})
	if !SpillPeerAllowed("10.0.0.1:8011") {
		t.Fatal("whitelisted host should be allowed")
	}
	if SpillPeerAllowed("10.0.0.2:8011") {
		t.Fatal("non-whitelisted host must be denied")
	}

	// 动态 provider 实时反映，且覆盖静态列表
	dyn := []string{"10.0.0.9:9"}
	SetSpillPeerProvider(func() []string { return dyn })
	if !SpillPeerAllowed("10.0.0.9:9") {
		t.Fatal("provider-listed host should be allowed")
	}
	if SpillPeerAllowed("10.0.0.1:8011") {
		t.Fatal("provider overrides static list")
	}

	// provider 清除后回退到拒绝一切（安全默认）
	SetSpillPeerProvider(nil)
	if SpillPeerAllowed("10.0.0.9:9") {
		t.Fatal("nil provider must deny all (safe default)")
	}
}
