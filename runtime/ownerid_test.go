package runtime

import "testing"

func TestEncodeDecodeOwnerIPv4(t *testing.T) {
	seg := encodeOwner("10.123.45.67:8011")
	if seg == "" || seg[0] != '4' {
		t.Fatalf("ipv4 seg should start with '4', got %q", seg)
	}
	if len(seg) > 12 {
		t.Fatalf("ipv4 owner seg too long: %d (%q)", len(seg), seg)
	}
	hp, ok := decodeOwner(seg)
	if !ok || hp != "10.123.45.67:8011" {
		t.Fatalf("roundtrip failed: ok=%v hp=%q", ok, hp)
	}
}

func TestEncodeDecodeOwnerHostname(t *testing.T) {
	seg := encodeOwner("host.example.com:8011")
	if seg == "" || seg[0] != 'h' {
		t.Fatalf("hostname seg should start with 'h', got %q", seg)
	}
	hp, ok := decodeOwner(seg)
	if !ok || hp != "host.example.com:8011" {
		t.Fatalf("roundtrip failed: ok=%v hp=%q", ok, hp)
	}
}

func TestNewIDForOwner(t *testing.T) {
	id := newIDForOwner("10.0.0.1:80")
	hp, ok := OwnerOf(id)
	if !ok || hp != "10.0.0.1:80" {
		t.Fatalf("OwnerOf failed: ok=%v hp=%q id=%q", ok, hp, id)
	}
	legacy := newIDForOwner("")
	if _, ok := OwnerOf(legacy); ok {
		t.Fatalf("empty owner id must have no owner info: %q", legacy)
	}
	if len(legacy) != 32 {
		t.Fatalf("ownerless id should be 32 hex chars, got %d (%q)", len(legacy), legacy)
	}
}

func TestOwnerOfRejectsPlainAndGarbage(t *testing.T) {
	if _, ok := OwnerOf("0123456789abcdef0123456789abcdef"); ok {
		t.Fatal("plain random id must be treated as no-owner")
	}
	if _, ok := OwnerOf("zzzz.deadbeef"); ok {
		t.Fatal("garbage owner seg must be rejected")
	}
	if _, ok := OwnerOf(""); ok {
		t.Fatal("empty id must be no-owner")
	}
}

// TestSelfAddrAndPublicBaseURL：两个地址各管一件事——SelfAddr 是副本身份（id 归属、拨号），
// PublicBaseURL 是交给外部的入口（拼链接）。没配后者时由前者推导，配了则互不影响。
func TestSelfAddrAndPublicBaseURL(t *testing.T) {
	oldAddr, oldBase := SelfHostPort(), PublicBaseURL()
	t.Cleanup(func() { SetSelfAddr(oldAddr); SetPublicBaseURL(oldBase) })

	SetSelfAddr("10.20.30.40:8011")
	SetPublicBaseURL("")
	if hp := SelfHostPort(); hp != "10.20.30.40:8011" {
		t.Fatalf("self host:port = %q, want 10.20.30.40:8011", hp)
	}
	if got := PublicBaseURL(); got != "http://10.20.30.40:8011" {
		t.Errorf("PublicBaseURL() = %q, want 由 SelfAddr 推导", got)
	}

	SetPublicBaseURL("https://mcp.example.com/")
	if got := PublicBaseURL(); got != "https://mcp.example.com" {
		t.Errorf("PublicBaseURL() = %q, want 末尾斜杠被去掉", got)
	}
	if hp := SelfHostPort(); hp != "10.20.30.40:8011" {
		t.Errorf("设置对外入口把副本身份改成了 %q：两者必须正交", hp)
	}

	SetSelfAddr("")
	SetPublicBaseURL("")
	if hp := SelfHostPort(); hp != "" {
		t.Fatalf("empty self addr expected, got %q", hp)
	}
	if got := PublicBaseURL(); got != "" {
		t.Fatalf("两者都没设时 PublicBaseURL 应为空串，got %q", got)
	}
}

func TestNewOwnedIDEmbedsSelf(t *testing.T) {
	old := SelfHostPort()
	t.Cleanup(func() { SetSelfAddr(old) })
	SetSelfAddr("127.0.0.1:9009")

	hp, ok := OwnerOf(NewOwnedID())
	if !ok || hp != "127.0.0.1:9009" {
		t.Fatalf("NewOwnedID has no/wrong owner: ok=%v hp=%q", ok, hp)
	}
}

// TestPeerAllowedDefaultsClosed：未注册 provider 时拒绝一切远端转发（防 SSRF）。
func TestPeerAllowedDefaultsClosed(t *testing.T) {
	t.Cleanup(func() { SetPeerProvider(nil) })

	SetPeerProvider(nil)
	if PeerAllowed("10.0.0.1:8011") {
		t.Error("no provider must deny every forward target")
	}
	SetPeers([]string{"10.0.0.1:8011"})
	if !PeerAllowed("10.0.0.1:8011") {
		t.Error("listed peer should be allowed")
	}
	if PeerAllowed("10.0.0.2:8011") {
		t.Error("unlisted peer must be denied")
	}
	// provider 覆盖静态列表：后调用者生效。
	SetPeerProvider(func() []string { return []string{"10.0.0.9:9"} })
	if !PeerAllowed("10.0.0.9:9") || PeerAllowed("10.0.0.1:8011") {
		t.Error("provider must override the static peer list")
	}
	SetPeers(nil)
	if PeerAllowed("10.0.0.9:9") {
		t.Error("empty peer list must deny everything")
	}
}
