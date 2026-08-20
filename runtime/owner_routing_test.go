package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

func TestWithOwnerRoutingProxiesRemote(t *testing.T) {
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ownerForwardedHeader) == "" {
			t.Error("proxied request must carry loop-guard header")
		}
		io.WriteString(w, `{"proxied":true}`)
	}))
	defer owner.Close()
	host := hostOf(t, owner.URL)

	SetSpillBaseURL("http://10.9.9.9:1") // self != owner
	defer SetSpillBaseURL("")
	SetSpillPeers([]string{host})
	defer SetSpillPeers(nil)
	RegisterOwnerRouted("demo_tool", "id")

	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"local":true}`) })
	srv := httptest.NewServer(WithOwnerRouting(local))
	defer srv.Close()

	id := NewSpillIDForOwnerTest(host)
	body := `{"method":"tools/call","params":{"name":"demo_tool","arguments":{"id":"` + id + `"}}}`
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"proxied":true`) {
		t.Fatalf("expected proxied response, got %s", b)
	}
}

func TestWithOwnerRoutingLocalCases(t *testing.T) {
	SetSpillBaseURL("http://10.9.9.9:1")
	defer SetSpillBaseURL("")
	RegisterOwnerRouted("demo_tool", "id")
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `local`) })
	srv := httptest.NewServer(WithOwnerRouting(local))
	defer srv.Close()

	cases := []struct{ body, hdr string }{
		{`{"method":"tools/call","params":{"name":"other","arguments":{"id":"x"}}}`, ""},
		{`{"method":"tools/call","params":{"name":"demo_tool","arguments":{"id":"plainhex"}}}`, ""},
		{`{"method":"tools/call","params":{"name":"demo_tool","arguments":{"id":"` + NewSpillIDForOwnerTest("1.2.3.4:5") + `"}}}`, "1"},
	}
	for i, c := range cases {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(c.body))
		if c.hdr != "" {
			req.Header.Set(ownerForwardedHeader, c.hdr)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "local" {
			t.Fatalf("case %d expected local, got %s", i, b)
		}
	}
}
