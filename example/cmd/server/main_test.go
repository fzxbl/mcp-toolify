package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// exampleConfig 是本示例真正加载的那份配置。
const exampleConfig = "../../conf/mcp.toml"

// configCopy 把示例配置复制一份，把 spill.dir 换成用例私有目录，并按需追加内容。
//
// 换 dir 的理由：示例配置里写的是共享 /tmp 下的固定路径，spill 会校验目录归属，
// 同机上别的用户先建过同名目录时启动会被拒——那是测试环境噪声，不是被测行为。
// **只换值不换键**，因此配置认领制的验证（键与已装插件一一对应）依然成立。
func configCopy(t *testing.T, extra string) string {
	t.Helper()
	raw, err := os.ReadFile(exampleConfig)
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "spill")
	body := regexp.MustCompile(`(?m)^dir = ".*"$`).ReplaceAllString(string(raw),
		"dir = "+`"`+dir+`"`)
	if !strings.Contains(body, dir) {
		t.Fatalf("示例配置里应有一行 spill 的 dir = \"...\"，实际没匹配到")
	}
	path := filepath.Join(t.TempDir(), "mcp.toml")
	if err := os.WriteFile(path, []byte(body+extra), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// jsonRPC 发一条 JSON-RPC 请求，返回响应状态与正文。
func jsonRPC(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// TestExampleAssemblyServesTools：示例的完整装配（基座 + spill + 那份配置）必须真的
// 能起来并调通一个工具。
//
// 它同时是**配置键认领制**的真跑验证：配置文件里任何没被基座或某个已装插件解码走的键
// 都会让 Handlers() 失败，所以这条用例过了就说明 example/conf/mcp.toml 与已装插件实际
// 认领的键**完全一致**（多一个键、少装一个插件都过不去，见下面两条用例）。
func TestExampleAssemblyServesTools(t *testing.T) {
	runtime.ResetToolsForTest()
	t.Cleanup(runtime.ResetToolsForTest)

	r, err := build(runtime.Config{Addr: "127.0.0.1:0", ConfigPath: configCopy(t, "")})
	if err != nil {
		t.Fatalf("装配示例失败: %v", err)
	}
	t.Cleanup(func() { r.RunStop(context.Background()) })
	h, routes, err := r.Handlers()
	if err != nil {
		t.Fatalf("示例配置必须能启动（认领制：配置键与已装插件要完全对应）: %v", err)
	}
	if _, ok := routes["/spill/"]; !ok {
		t.Errorf("spill 的下载路由必须挂上，实际路由=%v", keys(routes))
	}

	srv := httptest.NewServer(h)
	defer srv.Close()

	status, body := jsonRPC(t, srv.URL, "replace-me-readonly",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+
			`{"name":"greeter.greet","arguments":{"name":"world","excited":true}}}`)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，正文 = %s", status, body)
	}
	if !strings.Contains(body, "Hello, world!") {
		t.Fatalf("只读 token 调只读工具必须拿到结果，实际正文 = %s", body)
	}

	// 只读 token 看不到高危写工具：同一套 selector 同时决定可见性与可执行性。
	_, list := jsonRPC(t, srv.URL, "replace-me-readonly",
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if !strings.Contains(list, "greeter.greet") {
		t.Errorf("只读工具必须可见: %s", list)
	}
	if strings.Contains(list, "greeter.shout") {
		t.Errorf("只读 token 不该看到 capability=write,risk=high 的工具: %s", list)
	}
}

// greetCallArgs 从文档里定位调用 greeter.greet 的那段，取出它的 arguments 并解析。
//
// 定位方式是「先找到 name":"greeter.greet"、再取紧随其后的 arguments」，因此断言真的锚在
// 那条调用上；截取用花括号配平而不是找下一个 }，否则嵌套对象会被截断。
func greetCallArgs(t *testing.T, path string) (raw []byte, args map[string]any) {
	t.Helper()
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	i := strings.Index(string(text), `"greeter.greet"`)
	if i < 0 {
		t.Fatalf("%s 里没有调用 greeter.greet 的示例", path)
	}
	rest := string(text)[i:]
	j := strings.Index(rest, `"arguments"`)
	if j < 0 {
		t.Fatalf("%s 的 greeter.greet 示例没有 arguments", path)
	}
	rest = rest[j:]
	start := strings.Index(rest, "{")
	if start < 0 {
		t.Fatalf("%s 的 arguments 后面没有对象: %.80s", path, rest)
	}
	depth, end := 0, -1
	for p, ch := range rest[start:] {
		switch ch {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				end = start + p + 1
			}
		}
		if end > 0 {
			break
		}
	}
	if end < 0 {
		t.Fatalf("%s 的 arguments 花括号不配平: %.80s", path, rest[start:])
	}
	raw = []byte(rest[start:end])
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatalf("%s 的 arguments 不是合法 JSON（%v）: %s", path, err, raw)
	}
	return raw, args
}

// TestExampleReadmeCurlArgumentsAreComplete：README 与 main.go 注释里给的第一条 curl，
// 其 arguments 必须真的能通过 schema 校验。
//
// 上一版少了 excited（生成的 schema 两个参数都是 required），新用户照抄的第一条命令
// 直接拿到一个 isError，而文案很容易被误读成装配或鉴权出了问题。
func TestExampleReadmeCurlArgumentsAreComplete(t *testing.T) {
	runtime.ResetToolsForTest()
	t.Cleanup(runtime.ResetToolsForTest)

	// 三处文档里的那份 arguments，逐个解析出来按**值**断言，再用其中一份真发一次请求。
	// 刻意不做字面量匹配：那样既偏严（文档把 `"arguments": {` 多打一个空格就误红），
	// 又偏浅（只要文件任何位置出现过那串字面量就算过，不校验它真在 greet 那条 curl 里）。
	var wantArgs []byte
	for _, doc := range []string{"../../../README.md", "../../../README.zh-CN.md", "main.go"} {
		raw, args := greetCallArgs(t, doc)
		if _, ok := args["name"]; !ok {
			t.Errorf("%s 的 greeter.greet 示例缺 name（schema 里是 required）: %s", doc, raw)
		}
		if _, ok := args["excited"]; !ok {
			t.Errorf("%s 的 greeter.greet 示例缺 excited（schema 里是 required）: %s", doc, raw)
		}
		wantArgs = raw
	}

	r, err := build(runtime.Config{Addr: "127.0.0.1:0", ConfigPath: configCopy(t, "")})
	if err != nil {
		t.Fatalf("装配示例失败: %v", err)
	}
	t.Cleanup(func() { r.RunStop(context.Background()) })
	h, _, err := r.Handlers()
	if err != nil {
		t.Fatalf("Handlers: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	_, body := jsonRPC(t, srv.URL, "replace-me-readonly",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+
			`{"name":"greeter.greet","arguments":`+string(wantArgs)+`}}`)
	if strings.Contains(body, "isError") || !strings.Contains(body, "Hello, world!") {
		t.Fatalf("文档里的那条调用必须直接成功，实际正文 = %s", body)
	}
}

// TestExampleConfigRejectsUnclaimedKey：示例配置里多出一个没人认领的键（段名或字段名
// 拼错的等价形状）必须让启动失败。这是「配置静默失效」唯一的信号。
func TestExampleConfigRejectsUnclaimedKey(t *testing.T) {
	runtime.ResetToolsForTest()
	t.Cleanup(runtime.ResetToolsForTest)

	// bakcend 是 backend 的经典笔误：认领制之前它会被完全吞掉，插件按空后端上线。
	path := configCopy(t, "\n[qouta]\nbakcend = \"redis\"\n")
	r, err := build(runtime.Config{Addr: "127.0.0.1:0", ConfigPath: path})
	if err != nil {
		t.Fatalf("装配示例失败: %v", err)
	}
	t.Cleanup(func() { r.RunStop(context.Background()) })
	_, _, err = r.Handlers()
	if err == nil {
		t.Fatal("多出一个无人认领的配置键必须让启动失败")
	}
	if !strings.Contains(err.Error(), "qouta.bakcend") {
		t.Errorf("错误必须点出那个键: %v", err)
	}
}

// TestExampleConfigRequiresEveryPlugin：留着某个插件的配置段落却不装它，必须启动失败
// ——「写了 [spill] 却没装 spill」与「段落名拼错」在基座看来是同一件事：部署方以为
// 生效的东西没生效。这条用例守住「配置与装配一一对应」这个不变量。
func TestExampleConfigRequiresEveryPlugin(t *testing.T) {
	runtime.ResetToolsForTest()
	t.Cleanup(runtime.ResetToolsForTest)

	// 一个插件都不装，但配置里有 [spill] 段。
	r := runtime.New(runtime.Config{Addr: "127.0.0.1:0", ConfigPath: configCopy(t, "")}, nil)
	t.Cleanup(func() { r.RunStop(context.Background()) })
	_, _, err := r.Handlers()
	if err == nil {
		t.Fatal("少装插件却留着它的配置段落必须让启动失败")
	}
	if !strings.Contains(err.Error(), "spill") {
		t.Errorf("错误应点出没人认领的 spill 段落: %v", err)
	}
}

// TestBuildFailureReclaimsPluginGoroutines：启动失败时，插件在 Install 里起的协程
// 必须被回收。
//
// 这是真实形状的回归（基座级用例在 runtime 包里）：把校验从 Install 挪到 build 期钩子
// 之后，同一个配置错误报出来时三个 Install 已全部跑完，泄的是 spill 的回收协程与
// redis 客户端自带的清理协程。
//
// 判据取自 goroutine profile 里的函数名，并**轮询等待**它消失（等一个必然到达的状态，
// 而不是断言某个时刻的协程数——后者受同包其它用例与 GC 影响，是典型的 flaky 写法）。
func TestBuildFailureReclaimsPluginGoroutines(t *testing.T) {
	runtime.ResetToolsForTest()
	t.Cleanup(runtime.ResetToolsForTest)

	r, err := build(runtime.Config{Addr: "127.0.0.1:0",
		ConfigPath: configCopy(t, "\n[qouta]\nbakcend = \"redis\"\n")})
	if err != nil {
		t.Fatalf("装配示例失败: %v", err)
	}
	// 刻意不 defer RunStop：本用例要证明的就是「基座自己在失败路径上清干净了」。
	if _, _, err := r.Handlers(); err == nil {
		r.RunStop(context.Background())
		t.Fatal("该配置必须启动失败")
	}
	const frame = "mcp-toolify/plugins/spill.(*store).gcLoop"
	if left := waitGoroutineGone(t, frame); left != "" {
		t.Errorf("启动失败后仍有协程停在 %s：\n%s", frame, left)
	}
}

// waitGoroutineGone 轮询 goroutine profile，等到没有栈帧包含 frame 为止；
// 超时则返回当前 profile 里相关的片段供报错。
func waitGoroutineGone(t *testing.T, frame string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var buf bytes.Buffer
		if err := pprof.Lookup("goroutine").WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutine profile: %v", err)
		}
		if !strings.Contains(buf.String(), frame) {
			return ""
		}
		if time.Now().After(deadline) {
			var hit []string
			for _, line := range strings.Split(buf.String(), "\n") {
				if strings.Contains(line, frame) {
					hit = append(hit, line)
				}
			}
			return strings.Join(hit, "\n")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// keys 返回 map 的键（仅用于报错文案）。
func keys(m map[string]http.Handler) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
