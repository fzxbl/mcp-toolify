package runtime

import "testing"

func TestOwnerOf(t *testing.T) {
	SetSpillBaseURL("http://10.0.0.1:8011")
	defer SetSpillBaseURL("")
	owned := NewOwnedSpillID() // owner = 10.0.0.1:8011
	if hp, ok := OwnerOf(owned); !ok || hp != "10.0.0.1:8011" {
		t.Fatalf("OwnerOf(owned)=%q,%v want 10.0.0.1:8011,true", hp, ok)
	}
	if _, ok := OwnerOf("deadbeefdeadbeefdeadbeefdeadbeef"); ok {
		t.Fatal("plain hex id must have no owner")
	}
}

func TestRegisterOwnerRouted(t *testing.T) {
	RegisterOwnerRouted("demo_tool", "id")
	if p, ok := ownerRoutedParam("demo_tool"); !ok || p != "id" {
		t.Fatalf("ownerRoutedParam=%q,%v want id,true", p, ok)
	}
	if _, ok := ownerRoutedParam("unknown"); ok {
		t.Fatal("unknown tool must not be routed")
	}
}
