package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// unreadableBody 的 Read 一定失败：用它区分「基座读了 body」与「基座根本没读」——
// 读了会被 withMCPOwnerRouting 判成 400，没读则原样交给下游 handler。
type unreadableBody struct{}

func (unreadableBody) Read([]byte) (int, error) { return 0, errors.New("body 不该被读") }

// TestMCPOwnerRoutingReadsBodyOnlyWhenSomeToolRouted：一个工具都没登记按参数路由时，
// MCP 包裹不该为了找工具名去读 body。
//
// 为什么值得一条用例：读 body 是**每个** POST 都要付的一次全量拷贝（工具入参可以很大），
// 而「这个部署压根没用工具形态的 owner 路由」是很常见的情形。第二段反过来验证：
// 一旦有工具登记，同一个请求就必须走到读 body 那步——否则这条短路等于把功能关掉了。
func TestMCPOwnerRoutingReadsBodyOnlyWhenSomeToolRouted(t *testing.T) {
	ResetOwnerRoutedForTest()
	t.Cleanup(ResetOwnerRoutedForTest)
	ResetOwnerRoutedPathsForTest()
	t.Cleanup(ResetOwnerRoutedPathsForTest)
	resetRoutePrefix(t)

	var localHit int32
	h := withMCPOwnerRouting(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&localHit, 1)
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", unreadableBody{}))
	if atomic.LoadInt32(&localHit) != 1 || rec.Code != http.StatusNoContent {
		t.Fatalf("没有工具登记时 body 仍被读了：hit=%d code=%d",
			atomic.LoadInt32(&localHit), rec.Code)
	}

	RegisterOwnerRouted("demo_tool", "id")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", unreadableBody{}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("有工具登记时应当读 body（读失败 => 400），实际 %d", rec.Code)
	}
}

func TestOwnerOfOwnedAndPlain(t *testing.T) {
	SetSelfAddr("10.0.0.1:8011")
	defer SetSelfAddr("")
	owned := NewOwnedID() // owner = 10.0.0.1:8011
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

func TestMCPOwnerRoutingProxiesRemote(t *testing.T) {
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ownerForwardedHeader) == "" {
			t.Error("proxied request must carry loop-guard header")
		}
		io.WriteString(w, `{"proxied":true}`)
	}))
	defer owner.Close()
	host := hostOf(t, owner.URL)

	SetSelfAddr("10.9.9.9:1") // self != owner
	defer SetSelfAddr("")
	SetPeers([]string{host})
	defer SetPeers(nil)
	RegisterOwnerRouted("demo_tool", "id")

	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"local":true}`) })
	srv := httptest.NewServer(withMCPOwnerRouting(local))
	defer srv.Close()

	id := newIDForOwner(host)
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

func TestMCPOwnerRoutingLocalCases(t *testing.T) {
	SetSelfAddr("10.9.9.9:1")
	defer SetSelfAddr("")
	RegisterOwnerRouted("demo_tool", "id")
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `local`) })
	srv := httptest.NewServer(withMCPOwnerRouting(local))
	defer srv.Close()

	cases := []struct{ body, hdr string }{
		{`{"method":"tools/call","params":{"name":"other","arguments":{"id":"x"}}}`, ""},
		{`{"method":"tools/call","params":{"name":"demo_tool","arguments":{"id":"plainhex"}}}`, ""},
		{`{"method":"tools/call","params":{"name":"demo_tool","arguments":{"id":"` + newIDForOwner("1.2.3.4:5") + `"}}}`, "1"},
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

// TestRoutesWrapPluginRoutesWithPathOwnerRouting：基座交给宿主挂载的**插件路由**也必须带上
// 路径形态的 owner 路由。
//
// 这条断言看着像内部细节，实际是多副本部署的唯一防线：插件路由上的 id 常常是有归属的
// （spill 的落盘文件、confirm 的挂起回执），少了这一层，「回调/下载被负载均衡打到别的
// 副本」就是一次静默失败，而单副本部署完全看不出来。
func TestRoutesWrapPluginRoutesWithPathOwnerRouting(t *testing.T) {
	var ownerHit int32
	ownerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ownerHit, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ownerSrv.Close()
	ownerHost := hostOf(t, ownerSrv.URL)

	SetSelfAddr("10.9.9.9:1") // self != owner
	defer SetSelfAddr("")
	SetPeers([]string{ownerHost})
	defer SetPeers(nil)
	ResetOwnerRoutedPathsForTest()
	defer ResetOwnerRoutedPathsForTest()

	// Registry 把进程级前缀定死成本用例的 Config 值（这里没配前缀 → 空串），
	// 于是登记的 pattern 与请求路径同源。
	r := New(Config{ConfigPath: writeTokenConfig(t, okTokenConfig)}, nil)
	RegisterOwnerRoutedRoute("/thing/", func(r *http.Request) string {
		return strings.TrimPrefix(r.URL.Path, "/thing/")
	})
	var localHit int32
	// 用 RoutePublic：这条断言与鉴权无关，免鉴权路由同样要能跨副本。
	r.RoutePublic("/thing/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&localHit, 1)
	}))
	defer r.RunStop(context.Background())
	if _, _, err := r.Handlers(); err != nil {
		t.Fatalf("Handlers: %v", err)
	}
	h, ok := r.Routes()["/thing/"]
	if !ok {
		t.Fatal("插件路由没有被交出来")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/thing/"+newIDForOwner(ownerHost), nil))
	if atomic.LoadInt32(&localHit) != 0 {
		t.Error("归属兄弟副本的 id 不该由本副本的插件路由处理")
	}
	if n := atomic.LoadInt32(&ownerHit); n != 1 {
		t.Errorf("属主被打中 %d 次，期望 1 次", n)
	}
}

// TestOwnerRoutedRouteRegisteredBeforePrefixStillMatches：登记**早于**挂载前缀
// 生效（插件在 init 里登记就是这样）时，路由仍必须按加了前缀的实际路径生效。
//
// 这条守的是一次真实的设计缺陷：表若在登记当刻就把 pattern 换算成实际路径，早登记的那条
// 会永远按空前缀入表、再也匹配不上，而这种漏配只在多副本部署下暴露。现在换算发生在匹配时。
func TestOwnerRoutedRouteRegisteredBeforePrefixStillMatches(t *testing.T) {
	var ownerHit int32
	ownerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ownerHit, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ownerSrv.Close()
	ownerHost := hostOf(t, ownerSrv.URL)

	SetSelfAddr("10.9.9.9:1") // self != owner
	defer SetSelfAddr("")
	SetPeers([]string{ownerHost})
	defer SetPeers(nil)
	ResetOwnerRoutedPathsForTest()
	defer ResetOwnerRoutedPathsForTest()
	resetRoutePrefix(t)

	// 顺序刻意反着来：先登记，后设前缀（宿主 Mount 发生在插件 Install 之后，正是这个顺序）。
	RegisterOwnerRoutedRoute("/thing/", func(r *http.Request) string {
		return strings.TrimPrefix(r.URL.Path, RoutePath("/thing/"))
	})
	if err := setRoutePrefix("/mcp/plugin"); err != nil {
		t.Fatalf("setRoutePrefix: %v", err)
	}

	if !OwnerRoutedPathRegisteredForTest("/mcp/plugin/thing/") {
		t.Fatal("登记没有按加了前缀的实际路径生效")
	}
	var localHit int32
	h := WithPathOwnerRouting(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&localHit, 1)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/mcp/plugin/thing/"+newIDForOwner(ownerHost), nil))
	if atomic.LoadInt32(&localHit) != 0 {
		t.Error("归属兄弟副本的请求不该由本副本处理")
	}
	if n := atomic.LoadInt32(&ownerHit); n != 1 {
		t.Errorf("属主被打中 %d 次，期望 1 次", n)
	}
}

// TestPluginNamesReportsInstallOrder：插件名单必须按安装顺序（外→内）给出，
// 宿主与示例靠它断言链序约束（例如「confirm 必须装在 quota 之外」）。
func TestPluginNamesReportsInstallOrder(t *testing.T) {
	r := NewRegistry(Config{})
	for _, n := range []string{"audit", "confirm", "quota"} {
		r.Named(n)
	}
	got := r.PluginNames()
	want := []string{"audit", "confirm", "quota"}
	if len(got) != len(want) {
		t.Fatalf("PluginNames = %v，want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PluginNames = %v，want %v（顺序即链序）", got, want)
		}
	}
	// 返回的必须是副本：调用方改它不能影响 Registry 里的名单。
	got[0] = "tampered"
	if r.PluginNames()[0] != "audit" {
		t.Error("PluginNames 返回了内部切片，调用方能改到链序名单")
	}
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return u.Host
}

// pathRoutingFixture 装一套「本副本 + 路径提取器」的现场：返回本副本 handler
// 与「本地是否被调用过」的读取闭包。前缀固定 /confirm/，与 confirm 插件的回调一致。
// 清掉进程级路由前缀：匹配时会给 pattern 补前缀，而本现场的请求路径是裸的，
// 上一个用例留下的前缀会让它匹配不上。
func pathRoutingFixture(t *testing.T) (http.Handler, func() bool) {
	t.Helper()
	unlockRoutePrefixForTest() // 清掉上一个用例留下的前缀，本现场请求路径是裸的。
	ResetOwnerRoutedPathsForTest()
	t.Cleanup(ResetOwnerRoutedPathsForTest)
	RegisterOwnerRoutedRoute("/confirm/", func(r *http.Request) string {
		return strings.TrimPrefix(r.URL.Path, "/confirm/")
	})
	var localHit int32
	local := withMCPOwnerRouting(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&localHit, 1)
		w.WriteHeader(http.StatusTeapot)
	}))
	return local, func() bool { return atomic.LoadInt32(&localHit) > 0 }
}

// TestOwnerRoutedPathProxiesToOwner：普通 HTTP 路由（不是 tools/call）按路径里的
// owned id 反代到属主副本，且**保留原路径**——属主与本副本是同一份二进制，
// 挂载前缀必然相同，硬编码 /mcp 会把请求打到一个不存在的路径。
func TestOwnerRoutedPathProxiesToOwner(t *testing.T) {
	var ownerHit int32
	var gotPath string
	ownerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ownerHit, 1)
		gotPath = r.URL.Path
		if r.Header.Get(ownerForwardedHeader) == "" {
			t.Error("反代出去的请求必须带防环头")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ownerSrv.Close()
	ownerHost := hostOf(t, ownerSrv.URL)

	SetSelfAddr("10.9.9.9:1") // self != owner
	defer SetSelfAddr("")
	SetPeers([]string{ownerHost})
	defer SetPeers(nil)

	local, localHit := pathRoutingFixture(t)
	id := newIDForOwner(ownerHost)
	wantPath := "/confirm/" + id
	rec := httptest.NewRecorder()
	local.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, wantPath, nil))

	if localHit() {
		t.Error("归属兄弟副本的 id 不该由本副本处理")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("状态码 %d，期望 204（属主返回的）", rec.Code)
	}
	if n := atomic.LoadInt32(&ownerHit); n != 1 {
		t.Errorf("属主被打中 %d 次，期望 1 次", n)
	}
	if gotPath != wantPath {
		t.Errorf("反代没有保留原路径，实际 %s，期望 %s", gotPath, wantPath)
	}
}

// TestOwnerRoutedPathIgnoresSelfOwnedID：属主就是自己 → 本地处理，不反代自己。
func TestOwnerRoutedPathIgnoresSelfOwnedID(t *testing.T) {
	SetSelfAddr("10.9.9.9:1")
	defer SetSelfAddr("")
	SetPeers([]string{"10.9.9.9:1"})
	defer SetPeers(nil)

	local, localHit := pathRoutingFixture(t)
	rec := httptest.NewRecorder()
	local.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/confirm/"+newIDForOwner("10.9.9.9:1"), nil))
	if !localHit() {
		t.Error("属主是本副本时必须本地处理")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("状态码 %d，期望 418（本地 handler 的）", rec.Code)
	}
}

// TestOwnerRoutedPathRefusesNonPeerOwner：属主不在 peer 白名单 → 本地处理，
// 绝不反代（白名单是 SSRF 防线，id 里的地址是调用方可控输入）。
func TestOwnerRoutedPathRefusesNonPeerOwner(t *testing.T) {
	SetSelfAddr("10.9.9.9:1")
	defer SetSelfAddr("")
	SetPeers(nil)

	local, localHit := pathRoutingFixture(t)
	rec := httptest.NewRecorder()
	local.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/confirm/"+newIDForOwner("127.0.0.1:9"), nil))
	if !localHit() {
		t.Error("白名单外的属主必须退回本地处理，不能反代")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("状态码 %d，期望 418（本地 handler 的）", rec.Code)
	}
}

// TestOwnerRoutedPathDoesNotLoop：带防环头的请求直接本地处理，
// 否则两个副本会把同一个请求来回踢。
func TestOwnerRoutedPathDoesNotLoop(t *testing.T) {
	SetSelfAddr("10.9.9.9:1")
	defer SetSelfAddr("")
	SetPeers([]string{"1.2.3.4:5"})
	defer SetPeers(nil)

	local, localHit := pathRoutingFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/confirm/"+newIDForOwner("1.2.3.4:5"), nil)
	req.Header.Set(ownerForwardedHeader, "1")
	rec := httptest.NewRecorder()
	local.ServeHTTP(rec, req)
	if !localHit() {
		t.Error("带防环头的请求必须本地处理")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("状态码 %d，期望 418（本地 handler 的）", rec.Code)
	}
}

// TestOwnerRoutedPathGETIsRouted：路径路由不读 body，因此对 GET 同样成立——
// 回调端点未必是 POST，把它绑死在 POST 上会让 GET 回调静默落到非属主副本。
func TestOwnerRoutedPathGETIsRouted(t *testing.T) {
	var ownerHit int32
	ownerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ownerHit, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ownerSrv.Close()
	ownerHost := hostOf(t, ownerSrv.URL)

	SetSelfAddr("10.9.9.9:1")
	defer SetSelfAddr("")
	SetPeers([]string{ownerHost})
	defer SetPeers(nil)

	local, localHit := pathRoutingFixture(t)
	rec := httptest.NewRecorder()
	local.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/confirm/"+newIDForOwner(ownerHost), nil))
	if localHit() {
		t.Error("GET 回调也该被反代到属主")
	}
	if n := atomic.LoadInt32(&ownerHit); n != 1 {
		t.Errorf("属主被打中 %d 次，期望 1 次", n)
	}
}

// TestProxyToOwnerFailureIsNotFoundWithoutTopology：属主在白名单里但连不上时，
// 转发失败必须回 404、且响应里不带属主地址。
//
// 这条判定原先长在 spill 插件自己的转发实现里（那里显式装了 ErrorHandler）。转发统一到
// 基座之后，它得由基座保证：默认 ErrorHandler 会回 502，而任何持合法 token 的调用方都能
// 触发这条路——回显属主等于把内网拓扑告诉调用方。
func TestProxyToOwnerFailureIsNotFoundWithoutTopology(t *testing.T) {
	const dead = "127.0.0.1:1" // 保留端口，必然拒连
	SetSelfAddr("10.9.9.9:1")
	defer SetSelfAddr("")
	SetPeers([]string{dead})
	defer SetPeers(nil)

	local, localHit := pathRoutingFixture(t)
	rec := httptest.NewRecorder()
	local.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/confirm/"+newIDForOwner(dead), nil))
	if localHit() {
		t.Error("归属兄弟副本的请求不该由本副本处理")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("属主连不上 => %d，want 404", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, dead) {
		t.Errorf("转发失败的响应泄漏了属主地址: %q", body)
	}
}
