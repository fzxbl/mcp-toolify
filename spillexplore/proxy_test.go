package spillexplore

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fzxbl/mcp-toolify/runtime"
)

func TestProxyForwardsToOwnerReplica(t *testing.T) {
	t.Cleanup(func() { runtime.SetSpillPeer("", 0); runtime.SetSpillPeerProvider(nil) })

	srv := httptest.NewServer(runtime.SpillExploreEndpoint())
	t.Cleanup(srv.Close)
	ownerHostPort := strings.TrimPrefix(srv.URL, "http://")

	runtime.SetSpillPeer("tok-xyz", 0)
	runtime.SetSpillPeers([]string{ownerHostPort})

	id := runtime.NewSpillIDForOwnerTest(ownerHostPort)
	p := filepath.Join(runtime.SpillDirForTest(), "toolx-"+id+".txt")
	if err := os.WriteFile(p, []byte("l1\nl2\nl3\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(p) })

	runtime.SetSpillBaseURL("http://255.255.255.255:1")
	t.Cleanup(func() { runtime.SetSpillBaseURL("") })

	res, err := SpillExplore(id, "read", 0, 2, "", "", 0, 0)
	if err != nil {
		t.Fatalf("explore err: %v", err)
	}
	if res["read"] == nil {
		t.Fatalf("expected read result, got %+v", res)
	}
}

func TestSpillExploreGoesThroughProxyOnLocalMiss(t *testing.T) {
	t.Cleanup(func() { runtime.SetSpillPeer("", 0); runtime.SetSpillPeerProvider(nil) })

	oldFn := runtime.LocalSpillExplore
	t.Cleanup(func() { runtime.LocalSpillExplore = oldFn })
	runtime.LocalSpillExplore = func(id, op string, lineOffset, limit int, pattern, jqExpr string, depth, maxBytes int) (map[string]any, error) {
		return map[string]any{"id": id, "op": op, "via": "proxy-stub"}, nil
	}

	srv := httptest.NewServer(runtime.SpillExploreEndpoint())
	t.Cleanup(srv.Close)
	ownerHostPort := strings.TrimPrefix(srv.URL, "http://")

	runtime.SetSpillPeer("tok-proxy", 0)
	runtime.SetSpillPeers([]string{ownerHostPort})

	// 归属=owner；self=不同地址；不往 store 写文件 => 本地 resolve 必然未命中
	id := runtime.NewSpillIDForOwnerTest(ownerHostPort)
	runtime.SetSpillBaseURL("http://255.255.255.255:1")
	t.Cleanup(func() { runtime.SetSpillBaseURL("") })

	res, err := SpillExplore(id, "stat", 0, 0, "", "", 0, 0)
	if err != nil {
		t.Fatalf("proxied explore err: %v", err)
	}
	if res["via"] != "proxy-stub" {
		t.Fatalf("expected result via proxy stub, got %+v", res)
	}
}

func TestRemoteMissWithoutTokenReturnsNotFound(t *testing.T) {
	runtime.SetSpillPeer("", 0) // 代理关闭
	t.Cleanup(func() { runtime.SetSpillPeer("", 0); runtime.SetSpillPeerProvider(nil) })
	runtime.SetSpillBaseURL("http://255.255.255.255:1")
	t.Cleanup(func() { runtime.SetSpillBaseURL("") })

	id := runtime.NewSpillIDForOwnerTest("10.1.2.3:9")
	_, err := SpillExplore(id, "stat", 0, 0, "", "", 0, 0)
	if err == nil {
		t.Fatal("expected not-found error when proxy disabled and file absent")
	}
}
