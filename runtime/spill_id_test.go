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

func TestNewSpillIDWithOwner(t *testing.T) {
	id := newSpillIDWithOwner("10.0.0.1:80")
	hp, ok := splitOwner(id)
	if !ok || hp != "10.0.0.1:80" {
		t.Fatalf("splitOwner failed: ok=%v hp=%q id=%q", ok, hp, id)
	}
	legacy := newSpillIDWithOwner("")
	if _, ok := splitOwner(legacy); ok {
		t.Fatalf("empty owner id must have no owner info: %q", legacy)
	}
	if len(legacy) != 32 {
		t.Fatalf("legacy id should be 32 hex chars, got %d (%q)", len(legacy), legacy)
	}
}

func TestSplitOwnerRejectsLegacyAndGarbage(t *testing.T) {
	if _, ok := splitOwner("0123456789abcdef0123456789abcdef"); ok {
		t.Fatal("legacy random id must be treated as no-owner")
	}
	if _, ok := splitOwner("zzzz.deadbeef"); ok {
		t.Fatal("garbage owner seg must be rejected")
	}
	if _, ok := splitOwner(""); ok {
		t.Fatal("empty id must be no-owner")
	}
}

func TestSetSpillBaseURLParsesSelfHostPort(t *testing.T) {
	old := getSpillBaseURL()
	t.Cleanup(func() { SetSpillBaseURL(old) })

	SetSpillBaseURL("http://10.20.30.40:8011")
	if hp := SpillSelfHostPort(); hp != "10.20.30.40:8011" {
		t.Fatalf("self host:port = %q, want 10.20.30.40:8011", hp)
	}
	SetSpillBaseURL("")
	if hp := SpillSelfHostPort(); hp != "" {
		t.Fatalf("empty base should yield empty self host:port, got %q", hp)
	}
}

func TestCreateEmbedsOwnerWhenBaseSet(t *testing.T) {
	old := getSpillBaseURL()
	t.Cleanup(func() { SetSpillBaseURL(old) })
	SetSpillBaseURL("http://127.0.0.1:9009")

	st := newDiskSpillStore(t.TempDir(), spillTTLConfig{})
	id, _ := st.create("toolx", FormatJSON)
	hp, ok := splitOwner(id)
	if !ok || hp != "127.0.0.1:9009" {
		t.Fatalf("create id has no/wrong owner: ok=%v hp=%q id=%q", ok, hp, id)
	}
}
