package runtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSpillExploreEndpointAuthAndDispatch(t *testing.T) {
	oldFn := LocalSpillExplore
	t.Cleanup(func() { LocalSpillExplore = oldFn })
	LocalSpillExplore = func(id, op string, lineOffset, limit int, pattern, jqExpr string, depth, maxBytes int) (map[string]any, error) {
		return map[string]any{"id": id, "op": op}, nil
	}
	SetSpillPeer("tok-abc", 0)
	t.Cleanup(func() { SetSpillPeer("", 0) })

	srv := httptest.NewServer(SpillExploreEndpoint())
	t.Cleanup(srv.Close)

	body := `{"id":"4aaa.bbbb","op":"stat"}`

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status=%d want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Spill-Peer-Token", "tok-abc")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil || resp2.StatusCode != http.StatusOK {
		t.Fatalf("with token: err=%v status=%d", err, resp2.StatusCode)
	}
}

func TestSpillExploreEndpointDisabledWhenNoToken(t *testing.T) {
	SetSpillPeer("", 0)
	srv := httptest.NewServer(SpillExploreEndpoint())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{"id":"x","op":"stat"}`))
	req.Header.Set("X-Spill-Peer-Token", "anything")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled endpoint should 404, got %d", resp.StatusCode)
	}
}

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
