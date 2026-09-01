package spill

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// twoTokens 是两个只读 token：用途名不同，权限完全相同。
// 「同样只有 capability=read」正是越权用例的关键：C1 不是权限高低的问题，
// 而是下载端点完全不看请求者是谁。
const twoTokens = `
[[tokens]]
token = "t-a"
name = "agent-a"
applicant = "tester"
allow = ["capability=read"]

[[tokens]]
token = "t-b"
name = "agent-b"
applicant = "tester"
allow = ["capability=read"]
`

// fixture 是一套「真实 HTTP + MCP + spill 插件」的装配，用于安全类断言。
type fixture struct {
	srv *httptest.Server
	dir string
}

// newFixture 装一个带 demo.big 工具的服务：调用它必然产出一次落盘。
func newFixture(t *testing.T, tokens string, extra string, payload string) *fixture {
	t.Helper()
	runtime.ResetToolsForTest()
	t.Cleanup(runtime.ResetToolsForTest)

	dir := filepath.Join(t.TempDir(), "spill")
	body := tokens + "\n[spill]\nthreshold_bytes = 4096\ndir = \"" +
		strings.ReplaceAll(dir, `\`, `\\`) + "\"\n" + extra
	r := runtime.New(runtime.Config{ConfigPath: writeConfig(t, body)},
		func(s *mcp.Server, opts runtime.RegisterOptions) {
			mcp.AddTool(s, &mcp.Tool{Name: "demo.big", Description: "返回一大坨"},
				func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
					return nil, map[string]any{"result": payload}, nil
				})
			runtime.RegisterTool(runtime.ToolInfo{Name: "demo.big", Pkg: "demo",
				Labels: map[string]string{"capability": "read", "risk": "none"}})
		})
	if err := Install(r); err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() { r.RunStop(context.Background()) })

	h, routes, err := r.Handlers()
	if err != nil {
		t.Fatalf("Handlers: %v", err)
	}
	mux := http.NewServeMux()
	for pattern, rh := range routes {
		mux.Handle(pattern, rh)
	}
	mux.Handle("/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	withBaseURL(t, srv.URL)
	return &fixture{srv: srv, dir: dir}
}

// callBig 以指定 token（可带身份头）调一次 demo.big，返回响应体里的 spill id。
func (f *fixture) callBig(t *testing.T, token string, headers map[string]string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.srv.URL,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call",`+
			`"params":{"name":"demo.big","arguments":{}}}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return spillIDOf(t, string(raw))
}

// get 以指定 token（可带身份头）下载一个 spill id，返回状态码与响应体。
func (f *fixture) get(t *testing.T, token, id string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+downloadPath+id, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(raw)
}

// TestDownloadDeniesOtherToken 覆盖 C1：落盘结果只属于产出它的调用主体。
// 另一个 token 即使权限相同也不能下载——token 的 allow/deny selector 是本项目
// 唯一的按工具分级手段，下载端点若不看主体就把这层分级整个绕过了。
func TestDownloadDeniesOtherToken(t *testing.T) {
	payload := strings.Repeat("A-PRIVATE-PAYLOAD ", 2000)
	f := newFixture(t, twoTokens, "", payload)
	id := f.callBig(t, "t-a", nil)

	if code, body := f.get(t, "t-a", id, nil); code != http.StatusOK ||
		!strings.Contains(body, payload) {
		t.Fatalf("属主下载 => %d，want 200 + 内容（body 前缀 %.80s）", code, body)
	}
	code, body := f.get(t, "t-b", id, nil)
	if code != http.StatusNotFound {
		t.Errorf("非属主下载 => %d，want 404（不能是 403：不泄漏存在性）", code)
	}
	if strings.Contains(body, "A-PRIVATE-PAYLOAD") {
		t.Fatal("非属主 token 拿到了别人 spill 的结果原文")
	}
}

// TestDownloadDeniesOtherIdentity 覆盖 C1 的身份维度：开了
// trust_identity_header 时，同一个 token 下的不同人也不能互相下载。
func TestDownloadDeniesOtherIdentity(t *testing.T) {
	payload := strings.Repeat("ZHANGSAN-ONLY ", 2000)
	f := newFixture(t, "trust_identity_header = true\n"+twoTokens, "", payload)
	zhangsan := map[string]string{"X-MCP-User": "zhangsan"}
	lisi := map[string]string{"X-MCP-User": "lisi"}
	id := f.callBig(t, "t-a", zhangsan)

	if code, _ := f.get(t, "t-a", id, zhangsan); code != http.StatusOK {
		t.Fatalf("属主本人下载 => %d，want 200", code)
	}
	code, body := f.get(t, "t-a", id, lisi)
	if code != http.StatusNotFound {
		t.Errorf("同 token 的另一个人下载 => %d，want 404", code)
	}
	if strings.Contains(body, "ZHANGSAN-ONLY") {
		t.Fatal("同 token 的另一个人拿到了结果原文")
	}
	// 无身份头（匿名）也不能顶替一个有身份的属主。
	if code, _ := f.get(t, "t-a", id, nil); code != http.StatusNotFound {
		t.Errorf("匿名下载有身份属主的文件 => %d，want 404", code)
	}
}

// TestOwnerMatchRules 把匹配规则的四个象限固定住，尤其是「Subject.ID 默认为空」
// 这条基座契约：空身份时退化为「同 token 用途名可下载」，不能把空串当成一个人。
func TestOwnerMatchRules(t *testing.T) {
	cases := []struct {
		name  string
		file  fileOwner
		req   fileOwner
		allow bool
	}{
		{"同 token 无身份", fileOwner{Token: "a"}, fileOwner{Token: "a"}, true},
		{"异 token 无身份", fileOwner{Token: "a"}, fileOwner{Token: "b"}, false},
		{"同 token 同人", fileOwner{Token: "a", Subject: "zs", HasIdentity: true},
			fileOwner{Token: "a", Subject: "zs", HasIdentity: true}, true},
		{"同 token 异人", fileOwner{Token: "a", Subject: "zs", HasIdentity: true},
			fileOwner{Token: "a", Subject: "ls", HasIdentity: true}, false},
		{"有身份属主 vs 匿名请求", fileOwner{Token: "a", Subject: "zs", HasIdentity: true},
			fileOwner{Token: "a"}, false},
		{"匿名属主 vs 有身份请求", fileOwner{Token: "a"},
			fileOwner{Token: "a", Subject: "zs", HasIdentity: true}, true},
		{"空 token 不可下载", fileOwner{}, fileOwner{}, false},
	}
	for _, c := range cases {
		if got := c.file.allows(c.req); got != c.allow {
			t.Errorf("%s: allows = %v, want %v", c.name, got, c.allow)
		}
	}
}

// TestDownloadRefusesSymlink 覆盖 C2：落盘目录里出现一个「合法 id 名的符号链接」
// 时不得跟随。默认目录 <tmp>/mcp-toolify/spill 是可预测路径，这就是经典的 /tmp
// 预创建攻击：攻击者先放好软链，服务端一读就把目录外的文件吐出去。
func TestDownloadRefusesSymlink(t *testing.T) {
	f := newFixture(t, twoTokens, "", "TOP-SECRET-VIA-SYMLINK"+strings.Repeat("P", 20000))
	// 软链的目标必须是一份**属主校验能通过**的真 spill 文件（这里就用本 token 刚落盘
	// 的那份，挪到落盘目录之外）：若目标是随便一个文本文件，readOwner 会先失败并 404，
	// 于是即便去掉 O_NOFOLLOW 用例也照样绿——那种写法验不到这条防线。
	id := f.callBig(t, "t-a", nil)
	real := filepath.Join(f.dir, id+fileExt)
	secret := filepath.Join(filepath.Dir(f.dir), "secret"+fileExt)
	if err := os.Rename(real, secret); err != nil {
		t.Fatalf("move spill file: %v", err)
	}
	// 用一个形状合法、但内容是软链的 id。
	fakeID := strings.Repeat("ab12", 8) // 32 个十六进制字符
	if err := os.Symlink(secret, filepath.Join(f.dir, fakeID+fileExt)); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	code, body := f.get(t, "t-a", fakeID, nil)
	if code != http.StatusNotFound {
		t.Errorf("软链 id => %d，want 404", code)
	}
	if strings.Contains(body, "TOP-SECRET-VIA-SYMLINK") {
		t.Fatal("下载端点跟随符号链接，泄漏了落盘目录之外的文件")
	}

	// 第二种：**留在落盘目录内**的相对软链。逃出目录的软链由 os.Root 结构性拦下，
	// 而目录内的相对软链 Root 是会跟随的 —— 这一层只有 O_NOFOLLOW 能挡。
	insideID := f.callBig(t, "t-a", nil)
	linkID := strings.Repeat("ef56", 8)
	if err := os.Symlink(insideID+fileExt, filepath.Join(f.dir, linkID+fileExt)); err != nil {
		t.Fatalf("symlink inside: %v", err)
	}
	code, _ = f.get(t, "t-a", linkID, nil)
	if code != http.StatusNotFound {
		t.Errorf("目录内相对软链 id => %d，want 404（O_NOFOLLOW 应拦下）", code)
	}
}

// TestCheckDirModeRejectsSharedPerms 覆盖 I7 与 C2 的前置条件：
// group/other 可访问的落盘目录必须被判为不安全（它是软链攻击与结果泄漏的前提）。
func TestCheckDirModeRejectsSharedPerms(t *testing.T) {
	for _, perm := range []os.FileMode{0o777, 0o770, 0o702, 0o750, 0o705} {
		if err := checkDirMode(perm); err == nil {
			t.Errorf("权限 %#o 必须被判为不安全", perm)
		}
	}
	for _, perm := range []os.FileMode{0o700, 0o600, 0o500} {
		if err := checkDirMode(perm); err != nil {
			t.Errorf("权限 %#o 应被接受: %v", perm, err)
		}
	}
}

// TestNewStoreTightensExistingDir 覆盖 I7：os.MkdirAll 对**已存在**的目录不改权限，
// 于是「目录 0700」这条承诺静默失效（预先存在的 0777 目录实测仍是 drwxrwxrwx）。
// 目录属于本进程用户时应当顺手收紧，改不动才启动失败。
func TestNewStoreTightensExistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spill")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	st, err := newStore(dir, time.Hour, time.Hour, quota{})
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(st.close)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("已存在的 0777 目录在 newStore 之后是 %v，want 0700", info.Mode().Perm())
	}
}

// TestNewStoreCreatesPrivateDir：新建目录必须是 0700，且 store 拒绝把落盘目录
// 指到一个符号链接上（软链可以在服务启动前被换掉）。
func TestNewStoreCreatesPrivateDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "fresh")
	st, err := newStore(dir, time.Hour, time.Hour, quota{})
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	st.close()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("新建落盘目录权限 = %v，want 0700", info.Mode().Perm())
	}

	link := filepath.Join(base, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if st, err := newStore(link, time.Hour, time.Hour, quota{}); err == nil {
		st.close()
		t.Error("落盘目录是符号链接时必须启动失败")
	}
}

// TestPutStaysInPinnedDirAfterSwap 覆盖复审 I-1：`ensurePrivateDir` 只在启动时查一次，
// 拥有**父目录**的攻击者可以在启动之后把落盘目录 mv 走、换成一个指向自己目录的软链。
// 这个序列在旧实现（每次拼绝对路径 + O_NOFOLLOW 只保护最后一段）下会让后续 put 全部
// 写进攻击者目录，并让攻击者放进去的自造 owner 文件被对应 token 下载到。
//
// 现在 store 持有 os.OpenRoot 拿到的目录句柄：路径解析从那个 fd 开始，
// 目录被换掉之后写入照旧落在原 inode 上，攻击者目录里的伪造文件也读不到。
func TestPutStaysInPinnedDirAfterSwap(t *testing.T) {
	withBaseURL(t, "http://127.0.0.1:18011")
	parent := t.TempDir() // 扮演「攻击者拥有的父目录」，如可预测的 /tmp/mcp-toolify
	dir := filepath.Join(parent, "spill")
	st, err := newStore(dir, time.Hour, time.Hour, quota{})
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(st.close)
	if _, _, err := st.put("a.read", textResult(100), testOwner); err != nil {
		t.Fatalf("首次 put: %v", err)
	}

	// 攻击者动手：把真目录挪走，把原路径换成指向自己目录的软链。
	orig := filepath.Join(parent, "orig")
	attacker := filepath.Join(parent, "attacker")
	if err := os.Rename(dir, orig); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.MkdirAll(attacker, 0o777); err != nil {
		t.Fatalf("mkdir attacker: %v", err)
	}
	if err := os.Symlink(attacker, dir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// 攻击者在自己目录里放一份自造 owner 的「结果」，等着被 token 持有者下载。
	forgedID := strings.Repeat("cd34", 8)
	forged := `{"owner":{"Token":"` + testOwner.Token + `"},"tool":"a.read",` +
		`"at":"2026-01-01T00:00:00Z","content":[{"type":"text","text":"FORGED"}]}`
	if err := os.WriteFile(filepath.Join(attacker, forgedID+fileExt),
		[]byte(forged), 0o644); err != nil {
		t.Fatalf("write forged: %v", err)
	}

	id, _, err := st.put("a.read", textResult(100), testOwner)
	if err != nil {
		t.Fatalf("目录被换掉之后 put 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(attacker, id+fileExt)); err == nil {
		t.Error("落盘写进了攻击者目录：目录句柄没有钉住原目录")
	}
	if _, err := os.Stat(filepath.Join(orig, id+fileExt)); err != nil {
		t.Errorf("落盘没有留在原目录: %v", err)
	}

	// 攻击者伪造的文件不可下载：句柄看不到它。
	rec := httptest.NewRecorder()
	st.downloadHandler().ServeHTTP(rec, ownedRequest(forgedID, testOwner))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("攻击者目录里的伪造文件 => %d，want 404", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if strings.Contains(string(body), "FORGED") {
		t.Error("下载端点读到了攻击者目录里的伪造内容")
	}
	// GC 也不该去动攻击者目录里的文件（它既不在句柄里，也不属于本实例）。
	st.gcOnce()
	if _, err := os.Stat(filepath.Join(attacker, forgedID+fileExt)); err != nil {
		t.Errorf("GC 动了落盘目录之外的文件: %v", err)
	}
}

// TestGCWarnsOnceOnDirSwap 覆盖收尾项 1：目录被换手时 GC 要打一条**非致命**告警，
// 且做去重（状态不变就不重复打），文案必须劝退「为此重启服务」——重启恰好会让新句柄
// 落到攻击者布置的软链上。恢复原样后再打一条恢复日志。
func TestGCWarnsOnceOnDirSwap(t *testing.T) {
	withBaseURL(t, "http://127.0.0.1:18011")
	parent := t.TempDir()
	dir := filepath.Join(parent, "spill")
	st, err := newStore(dir, time.Hour, time.Hour, quota{})
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(st.close)

	var quiet strings.Builder
	restore := captureLog(&quiet)
	st.gcOnce()
	restore()
	if strings.Contains(quiet.String(), "已不是启动时那个目录") {
		t.Errorf("目录没被动过就打了换手告警: %q", quiet.String())
	}

	// 攻击者：把真目录挪走，原路径换成指向自己目录的软链。
	orig := filepath.Join(parent, "orig")
	attacker := filepath.Join(parent, "attacker")
	if err := os.Rename(dir, orig); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.MkdirAll(attacker, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(attacker, dir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	var first, second strings.Builder
	restore = captureLog(&first)
	st.gcOnce()
	restore()
	got := first.String()
	if !strings.Contains(got, "已不是启动时那个目录") {
		t.Fatalf("目录被换手后必须告警: %q", got)
	}
	for _, want := range []string{"未受影响", "不要为此重启服务"} {
		if !strings.Contains(got, want) {
			t.Errorf("告警文案缺少 %q: %q", want, got)
		}
	}
	// 去重：状态没变化的第二轮不该再打。
	restore = captureLog(&second)
	st.gcOnce()
	restore()
	if strings.Contains(second.String(), "已不是启动时那个目录") {
		t.Errorf("同一状态重复告警（会刷屏）: %q", second.String())
	}

	// 恢复原样后应给一条恢复日志，且换手告警的去重状态被重置。
	if err := os.Remove(dir); err != nil {
		t.Fatalf("remove link: %v", err)
	}
	if err := os.Rename(orig, dir); err != nil {
		t.Fatalf("restore: %v", err)
	}
	var back strings.Builder
	restore = captureLog(&back)
	st.gcOnce()
	restore()
	if !strings.Contains(back.String(), "已恢复为启动时那个目录") {
		t.Errorf("目录恢复后应打一条恢复日志: %q", back.String())
	}
}

// TestDownloadRefusesHardLink 覆盖收尾项 2（nit-1）：落盘目录内的硬链接在结构上与真
// spill 文件无法区分（Lstat 看到的是普通文件），只有链接数能区分。前提是攻击者已经是
// 服务用户（那时他本可直接读结果文件），因此不构成额外风险，纯属多收一层。
func TestDownloadRefusesHardLink(t *testing.T) {
	f := newFixture(t, twoTokens, "", strings.Repeat("H", 20000))
	// 先落一份真结果，再把它挪到目录外，用硬链接以合法 id 的名字接回来。
	id := f.callBig(t, "t-a", nil)
	outside := filepath.Join(filepath.Dir(f.dir), "outside"+fileExt)
	if err := os.Rename(filepath.Join(f.dir, id+fileExt), outside); err != nil {
		t.Fatalf("move: %v", err)
	}
	linkID := strings.Repeat("9a8b", 8)
	if err := os.Link(outside, filepath.Join(f.dir, linkID+fileExt)); err != nil {
		t.Skipf("当前文件系统不支持硬链接: %v", err)
	}
	code, body := f.get(t, "t-a", linkID, nil)
	if code != http.StatusNotFound {
		t.Errorf("硬链接 id => %d，want 404", code)
	}
	if strings.Contains(body, "HHHH") {
		t.Error("下载端点吐出了硬链接指向的目录外文件")
	}
}
