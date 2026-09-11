package spill

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// bigResult 造一个含大文本 + structuredContent 的结果。
func bigResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: json.RawMessage(`{"n":1}`),
	}
}

// TestStorePutRoundTrip：落盘文件必须是完整、可解析的 JSON，且原文一字不差——
// 下载端点是模型拿完整结果的唯一途径，写残了等于结果丢了。
func TestStorePutRoundTrip(t *testing.T) {
	st := newTestStore(t, time.Hour, time.Hour)
	text := strings.Repeat("y", 5000)
	id, size, err := st.put("a.read", bigResult(text), testOwner)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if size <= int64(len(text)) {
		t.Errorf("落盘字节数 %d 应大于原文长度 %d", size, len(text))
	}
	path, ok := st.pathFor(id)
	if !ok {
		t.Fatalf("id %q 不是合法形状", id)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	var got struct {
		Tool              string            `json:"tool"`
		Content           []json.RawMessage `json:"content"`
		StructuredContent json.RawMessage   `json:"structuredContent"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v", err)
	}
	if got.Tool != "a.read" || len(got.Content) != 1 {
		t.Fatalf("落盘内容不完整: tool=%q content=%d", got.Tool, len(got.Content))
	}
	if !strings.Contains(string(got.Content[0]), text) {
		t.Error("落盘内容与原文不一致")
	}
	if string(got.StructuredContent) != `{"n":1}` {
		t.Errorf("structuredContent 丢了: %s", got.StructuredContent)
	}
}

// TestDownloadHandlerServesFile：公开的 /spill/<id> 必须能拿到落盘原文。
func TestDownloadHandlerServesFile(t *testing.T) {
	st := newTestStore(t, time.Hour, time.Hour)
	text := strings.Repeat("z", 3000)
	id, _, err := st.put("a.read", bigResult(text), testOwner)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	rec := httptest.NewRecorder()
	st.downloadHandler().ServeHTTP(rec,
		ownedRequest(id, testOwner))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s%s => %d", downloadPath, id, rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), text) {
		t.Error("下载内容与落盘原文不一致")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// TestDownloadHandlerRejectsBadID：id 只能是本插件自己生成的 base32/hex 形状。
// 路径拼接前必须校验，否则 /spill/../../etc/passwd 就是一个任意文件读取。
func TestDownloadHandlerRejectsBadID(t *testing.T) {
	st := newTestStore(t, time.Hour, time.Hour)
	secret := filepath.Join(filepath.Dir(st.dir), "secret.json")
	if err := os.WriteFile(secret, []byte("TOP-SECRET"), 0600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	bad := []string{
		"", "..", "../secret.json", "../../etc/passwd", "..%2f..%2fsecret.json",
		"sub/dir", "ZZZZ", "abc.def", "not-hex-at-all", strings.Repeat("a", 4096),
		string([]byte{0}) + "abc",
	}
	for _, id := range bad {
		t.Run(id, func(t *testing.T) {
			if _, ok := st.pathFor(id); ok {
				t.Fatalf("pathFor(%q) 认为它是合法 id", id)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.URL.Path = downloadPath + id
			st.downloadHandler().ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %q => %d，want 404", req.URL.Path, rec.Code)
			}
			if body, _ := io.ReadAll(rec.Body); strings.Contains(string(body), "TOP-SECRET") {
				t.Fatal("目录穿越读到了 spill 目录之外的文件")
			}
		})
	}
}

// TestDownloadHandlerRejectsNonGET：下载端点只读，其它方法一律 405。
func TestDownloadHandlerRejectsNonGET(t *testing.T) {
	st := newTestStore(t, time.Hour, time.Hour)
	id, _, err := st.put("a.read", bigResult("hello"), testOwner)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	rec := httptest.NewRecorder()
	st.downloadHandler().ServeHTTP(rec,
		httptest.NewRequest(http.MethodDelete, downloadPath+id, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE => %d，want 405", rec.Code)
	}
}

// TestGCRemovesExpiredOnly：TTL 到了的 spill 文件必须被清掉（磁盘不是无底洞），
// 未到期的与目录里的外来文件都不能动。
func TestGCRemovesExpiredOnly(t *testing.T) {
	st := newTestStore(t, 30*time.Minute, time.Hour)
	oldID, _, err := st.put("a.read", bigResult("old"), testOwner)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	freshID, _, err := st.put("a.read", bigResult("fresh"), testOwner)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	oldPath, _ := st.pathFor(oldID)
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldPath, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	foreign := filepath.Join(st.dir, "keep-me.txt")
	if err := os.WriteFile(foreign, []byte("not mine"), 0600); err != nil {
		t.Fatalf("write foreign: %v", err)
	}
	if err := os.Chtimes(foreign, past, past); err != nil {
		t.Fatalf("chtimes foreign: %v", err)
	}

	st.gcOnce()

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("过期文件没被清理: %v", err)
	}
	freshPath, _ := st.pathFor(freshID)
	if _, err := os.Stat(freshPath); err != nil {
		t.Errorf("未到期文件被误删: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("目录里的外来文件被误删: %v", err)
	}
}

// TestDownloadHandlerTreatsExpiredAsGone：GC 有周期，过期文件在被清掉之前
// 也不能再被下载——TTL 是对使用方的承诺，不能取决于 GC 什么时候醒。
func TestDownloadHandlerTreatsExpiredAsGone(t *testing.T) {
	st := newTestStore(t, time.Minute, time.Hour)
	id, _, err := st.put("a.read", bigResult("expired"), testOwner)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	path, _ := st.pathFor(id)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	rec := httptest.NewRecorder()
	st.downloadHandler().ServeHTTP(rec, ownedRequest(id, testOwner))
	if rec.Code != http.StatusNotFound {
		t.Errorf("过期 id => %d，want 404", rec.Code)
	}
	if body, _ := io.ReadAll(rec.Body); strings.Contains(string(body), "expired") {
		t.Error("过期文件的内容仍被吐出")
	}
}

// TestDownloadForwardsToOwnerReplica 覆盖 I2：spill 文件只在产出它的副本本地，
// 而下载请求经 LB 会随机落到任意副本。id 指向已知兄弟副本时必须把这次 GET 反代过去，
// 不能回「文件不存在」。
//
// 转发由基座的路径 owner 路由完成（见 downloadEntry），本用例验的是「插件把提取器
// 登记对了、于是这条链路真的通」；防环、白名单、单跳等判定归基座的用例。
// 公开下载不再要求属主副本再做 token 认证，Authorization 透传与否不影响正确性。
func TestDownloadForwardsToOwnerReplica(t *testing.T) {
	var gotPath string
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, "PAYLOAD-FROM-OWNER")
	}))
	defer owner.Close()
	ownerHost := strings.TrimPrefix(owner.URL, "http://")

	withBaseURL(t, "http://127.0.0.1:18011")
	runtime.SetPeers([]string{ownerHost})
	t.Cleanup(func() { runtime.SetPeers(nil) })

	st := newTestStore(t, time.Hour, time.Hour)
	// 造一个「归属兄弟副本」的 id：换个 base URL 生成，再换回来。
	runtime.SetSelfAddr(ownerHost)
	peerID := runtime.NewOwnedID()
	runtime.SetSelfAddr("127.0.0.1:18011")

	rec := httptest.NewRecorder()
	req := ownedRequest(peerID, testOwner)
	downloadEntry(t, st).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("归属兄弟副本的 id => %d，want 200（应被反代到属主）", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "PAYLOAD-FROM-OWNER") {
		t.Errorf("响应不是属主副本的内容: %q", body)
	}
	if gotPath != downloadPath+peerID {
		t.Errorf("转发路径 = %q，want %q", gotPath, downloadPath+peerID)
	}
}

// TestDownloadServesLocalCopyWithoutForwarding：id 归属兄弟副本、但这份文件本地就有
// （同机多副本共享落盘目录时的常态）时，提取器要返回空串让请求留在本地——
// 绕一趟属主既慢又可能白拿一个 404。
func TestDownloadServesLocalCopyWithoutForwarding(t *testing.T) {
	var hits int32
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		io.WriteString(w, "SHOULD-NOT-BE-CALLED")
	}))
	defer owner.Close()
	ownerHost := strings.TrimPrefix(owner.URL, "http://")

	withBaseURL(t, "http://"+ownerHost) // 先认属主，让落盘 id 内嵌它的地址
	st := newTestStore(t, time.Hour, time.Hour)
	id, _, err := st.put("demo.read", bigResult("hello-local"), testOwner)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	// 再把本副本换成别的地址：id 归属「兄弟副本」，但文件就在本地。
	runtime.SetSelfAddr("127.0.0.1:18011")
	runtime.SetPeers([]string{ownerHost})
	t.Cleanup(func() { runtime.SetPeers(nil) })

	rec := httptest.NewRecorder()
	downloadEntry(t, st).ServeHTTP(rec, ownedRequest(id, testOwner))
	if rec.Code != http.StatusOK {
		t.Fatalf("本地有这份文件 => %d，want 200", rec.Code)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("本地有文件却还是转发了 %d 次", n)
	}
}

// TestDownload404LeaksNoTopology 覆盖 M1：404 文案不得回显属主副本地址——
// 任何持合法 token 的人都能触发它，回显等于把内网拓扑告诉调用方。
func TestDownload404LeaksNoTopology(t *testing.T) {
	withBaseURL(t, "http://127.0.0.1:18011")
	st := newTestStore(t, time.Hour, time.Hour)
	// 归属一个**未登记为 peer** 的地址：不转发，走本地 404。
	runtime.SetSelfAddr("10.1.2.3:9999")
	peerID := runtime.NewOwnedID()
	runtime.SetSelfAddr("127.0.0.1:18011")

	rec := httptest.NewRecorder()
	downloadEntry(t, st).ServeHTTP(rec, ownedRequest(peerID, testOwner))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知归属的 id => %d，want 404", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if strings.Contains(string(body), "10.1.2.3") {
		t.Errorf("404 响应泄漏了内网地址: %q", body)
	}

	// 第二种路径：属主**在 peer 白名单里但连不上**，走基座反向代理的 ErrorHandler。
	// 只测上面那条（不转发）验不到 ErrorHandler，那正是 M1 里回显属主地址的地方。
	dead := "127.0.0.1:1" // 保留端口，必然拒连
	runtime.SetPeers([]string{dead})
	t.Cleanup(func() { runtime.SetPeers(nil) })
	runtime.SetSelfAddr(dead)
	deadID := runtime.NewOwnedID()
	runtime.SetSelfAddr("127.0.0.1:18011")

	rec = httptest.NewRecorder()
	downloadEntry(t, st).ServeHTTP(rec, ownedRequest(deadID, testOwner))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("属主副本连不上 => %d，want 404", rec.Code)
	}
	body, _ = io.ReadAll(rec.Body)
	if strings.Contains(string(body), dead) {
		t.Errorf("转发失败的 404 泄漏了属主副本地址: %q", body)
	}
}

// TestCloseIsIdempotent：OnStop 钩子可能被调多次（Start 的两条退出路径都会跑），
// 第二次不能 panic。
func TestCloseIsIdempotent(t *testing.T) {
	st, err := newStore(t.TempDir(), time.Hour, 10*time.Millisecond, quota{})
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	st.startGC()
	st.close()
	st.close()
}

// TestRoutePrefixAppliesToURLAndForward：宿主用 Registry.Mount 把本端点挂到别的前缀
// 之下时，**给 agent 的下载 URL** 与**副本间转发的目标路径**必须一起跟着走。
//
// 这两处是同一个前缀的两个下游，最容易只改一头：只改 URL 的话单副本能下、多副本下
// (N-1)/N 的请求转发到属主的旧路径拿 404；只改转发的话模型手里的链接直接 404。
// 两条断言缺一个，都能让这类 bug 溜过去。
func TestRoutePrefixAppliesToURLAndForward(t *testing.T) {
	const prefix = "/mcp/plugin"
	var gotPath string
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, "PAYLOAD-FROM-OWNER")
	}))
	defer owner.Close()
	ownerHost := strings.TrimPrefix(owner.URL, "http://")

	withBaseURL(t, "http://127.0.0.1:18011")
	if err := runtime.SetRoutePrefixForTest(prefix); err != nil {
		t.Fatalf("SetRoutePrefixForTest: %v", err)
	}
	t.Cleanup(func() { runtime.SetRoutePrefixForTest("") })
	runtime.SetPeers([]string{ownerHost})
	t.Cleanup(func() { runtime.SetPeers(nil) })

	st := newTestStore(t, time.Hour, time.Hour)
	if got, want := st.url("abc"), "http://127.0.0.1:18011"+prefix+downloadPath+"abc"; got != want {
		t.Errorf("下载 URL = %q, want %q", got, want)
	}

	runtime.SetSelfAddr(ownerHost)
	peerID := runtime.NewOwnedID()
	runtime.SetSelfAddr("127.0.0.1:18011")
	rec := httptest.NewRecorder()
	downloadEntry(t, st).ServeHTTP(rec,
		ownedRequestAt(prefix+downloadPath+peerID, testOwner))
	if rec.Code != http.StatusOK {
		t.Fatalf("归属兄弟副本的 id => %d，want 200（应被反代到属主）", rec.Code)
	}
	if want := prefix + downloadPath + peerID; gotPath != want {
		t.Errorf("转发路径 = %q，want %q", gotPath, want)
	}
}
