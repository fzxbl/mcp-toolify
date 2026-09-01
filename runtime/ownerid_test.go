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

func TestSetPublicBaseURLParsesSelfHostPort(t *testing.T) {
	old := PublicBaseURL()
	t.Cleanup(func() { SetPublicBaseURL(old) })

	SetPublicBaseURL("http://10.20.30.40:8011/")
	if got := PublicBaseURL(); got != "http://10.20.30.40:8011" {
		t.Errorf("PublicBaseURL() = %q, want trailing slash trimmed", got)
	}
	if hp := SelfHostPort(); hp != "10.20.30.40:8011" {
		t.Fatalf("self host:port = %q, want 10.20.30.40:8011", hp)
	}
	SetPublicBaseURL("")
	if hp := SelfHostPort(); hp != "" {
		t.Fatalf("empty base should yield empty self host:port, got %q", hp)
	}
}

func TestNewOwnedIDEmbedsSelf(t *testing.T) {
	old := PublicBaseURL()
	t.Cleanup(func() { SetPublicBaseURL(old) })
	SetPublicBaseURL("http://127.0.0.1:9009")

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
