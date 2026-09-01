package spill

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fzxbl/mcp-toolify/runtime"
)

// baseTokens 是基座必需的 [[tokens]] 段。
const baseTokens = `
[[tokens]]
token = "t-ops"
name = "ops-agent"
applicant = "tester"
allow = ["capability=read"]
`

// writeConfig 写一份临时 TOML，返回路径。
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.toml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// installed 装好 spill 插件并返回 Registry。
func installed(t *testing.T, configBody string) *runtime.Registry {
	t.Helper()
	r := runtime.New(runtime.Config{ConfigPath: writeConfig(t, configBody)}, nil)
	if err := Install(r); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// 插件在 OnStop 里停 GC 协程；用例结束时跑一遍，避免协程泄漏到后续用例。
	t.Cleanup(func() { r.RunStop(context.Background()) })
	return r
}

// TestInstallClaimsFullSection：文档承诺的每个字段都必须被结构体认领，
// 否则部署方照文档把配置写全反而启动失败（基座报「无人认领的配置项」）。
func TestInstallClaimsFullSection(t *testing.T) {
	r := installed(t, `required_plugins = ["spill"]
`+baseTokens+`
[spill]
dir = "`+strings.ReplaceAll(t.TempDir(), `\`, `\\`)+`"
threshold_bytes = 65536
ttl = "30m"
gc_interval = "5m"
preview_bytes = 1024
max_file_mib = 64
max_total_mib = 512
on_error = "warn"
`)
	if _, _, err := r.Handlers(); err != nil {
		t.Fatalf("配置全字段写齐时必须能启动: %v", err)
	}
}

// TestInstallLeavesTypoUnclaimed：段内字段名拼错必须让启动失败——
// 「阈值静默变默认值」没有任何运行期信号。
func TestInstallLeavesTypoUnclaimed(t *testing.T) {
	r := installed(t, baseTokens+`
[spill]
threshhold = 1024
`)
	_, _, err := r.Handlers()
	if err == nil {
		t.Fatal("拼错的配置项必须让启动失败")
	}
	if !strings.Contains(err.Error(), "spill.threshhold") {
		t.Errorf("错误里应点出拼错的键: %v", err)
	}
}

// TestInstallRejectsBadSection：坏配置在 Install 阶段就报错，宿主据此中止启动。
func TestInstallRejectsBadSection(t *testing.T) {
	cases := map[string]string{
		"bad on_error":  `on_error = "ignore"`,
		"bad ttl":       `ttl = "一小时"`,
		"neg threshold": `threshold_bytes = -1`,
		"neg preview":   `preview_bytes = -1`,
		"zero ttl str":  `ttl = "0s"`,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			r := runtime.New(runtime.Config{
				ConfigPath: writeConfig(t, baseTokens+"\n[spill]\n"+line+"\n")}, nil)
			if err := Install(r); err == nil {
				t.Fatalf("%s 必须让 Install 失败", name)
			}
		})
	}
}

// TestInstallLogsPolicy：落盘失败策略、阈值、TTL、目录必须在启动日志里声明——
// 部署方唯一能确认「我配的东西生效了」的地方。
func TestInstallLogsPolicy(t *testing.T) {
	var logged strings.Builder
	defer captureLog(&logged)()
	installed(t, baseTokens)
	for _, want := range []string{"spill", "on_error=", "threshold_bytes=", "ttl=",
		"max_file_mib=", "max_total_mib=", "dir="} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("启动日志缺少 %q: %q", want, logged.String())
		}
	}
}

// TestSpillEndpointRequiresToken：/spill/<id> 提供的是大结果原文，必须走
// Registry.Route（自动套 token 鉴权）注册。用 RoutePublic 的版本被实测过可以裸取内容。
func TestSpillEndpointRequiresToken(t *testing.T) {
	dir := t.TempDir()
	r := installed(t, baseTokens+`
[spill]
dir = "`+strings.ReplaceAll(dir, `\`, `\\`)+`"
`)
	_, routes, err := r.Handlers()
	if err != nil {
		t.Fatalf("Handlers: %v", err)
	}
	h, ok := routes[downloadPath]
	if !ok {
		t.Fatalf("插件没有注册 %s 路由: %v", downloadPath, routes)
	}
	// 先落一份真文件，确保 401 不是因为「文件不存在」。
	st, err := newStore(dir, time.Hour, time.Hour, quota{})
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(st.close)
	id, _, err := st.put("a.read", bigResult("SPILLED-PAYLOAD"), testOwner)
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + downloadPath + id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无 token 访问 => %d，want 401", resp.StatusCode)
	}
	if strings.Contains(string(body), "SPILLED-PAYLOAD") {
		t.Fatal("无鉴权就取到了大结果原文")
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+downloadPath+id, nil)
	req.Header.Set("Authorization", "Bearer t-ops")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "SPILLED-PAYLOAD") {
		t.Errorf("带 token 访问 => %d body=%s，want 200 + 内容", resp.StatusCode, body)
	}
}

// TestOuterPluginSeesSummaryNotPayload 站在**外层插件**的视角看一次落盘：链上排在
// spill 之外的插件（审计、计量之类）在返回路径上只应看到 spill 摘要（含可追溯的
// id），看不到大结果原文。
//
// 这正是 spill 的职责边界——外层插件按文本预裁剪拦不住非文本内容（这里是一张
// 图片），而它自己往往会把整份结果 marshal 一次落进存储。
func TestOuterPluginSeesSummaryNotPayload(t *testing.T) {
	withBaseURL(t, "http://127.0.0.1:8011")
	r := installed(t, baseTokens+`
[spill]
threshold_bytes = 64
dir = "`+strings.ReplaceAll(t.TempDir(), `\`, `\\`)+`"
`)
	secret := strings.Repeat("S", 4096)
	// 外层插件的替身：只把它在返回路径上看到的结果记下来。
	var seen string
	outer := func(next runtime.Handler) runtime.Handler {
		return func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
			res, err := next(ctx, c)
			if res != nil && res.Tool != nil {
				b, _ := json.Marshal(res.Tool)
				seen = string(b)
			}
			return res, err
		}
	}
	mws := r.Middlewares()
	h := runtime.Chain([]runtime.Middleware{outer, mws[0]},
		handlerReturning(&runtime.Result{Tool: &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png",
				Data: []byte(secret)}}}}))

	res, err := h(context.Background(), textCall("a.read"))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if seen == "" {
		t.Fatal("外层插件没有看到任何结果")
	}
	if strings.Contains(seen, secret) {
		t.Error("外层插件看到了大结果原文")
	}
	id := spillIDOf(t, resultText(t, res))
	if !strings.Contains(seen, id) {
		t.Errorf("外层插件看到的结果里没有可追溯的 spill id %q: %q", id, seen)
	}
}

// TestEndToEndSpillOverHTTP 走真实的 HTTP + MCP 协议一遍：一个返回大结果的工具，
// 响应里应当只有摘要 + 下载链接，而带同一个 token 访问那个链接能拿回完整内容。
// 中间件层面的用例证明不了「路由真的挂上了、URL 真的可用」。
func TestEndToEndSpillOverHTTP(t *testing.T) {
	runtime.ResetToolsForTest()
	t.Cleanup(runtime.ResetToolsForTest)

	payload := strings.Repeat("P", 200_000)
	r := runtime.New(runtime.Config{ConfigPath: writeConfig(t, baseTokens+`
[spill]
threshold_bytes = 4096
dir = "`+strings.ReplaceAll(t.TempDir(), `\`, `\\`)+`"
`)}, func(s *mcp.Server, opts runtime.RegisterOptions) {
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
	// 对外地址由基座在 Start 里设置；这里自己挂 server，所以自己设。
	withBaseURL(t, srv.URL)

	body := post(t, srv.URL, "t-ops",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call",`+
			`"params":{"name":"demo.big","arguments":{}}}`)
	if strings.Contains(body, payload) {
		t.Fatal("MCP 响应里出现了完整的大结果原文")
	}
	// 摘要按 preview_bytes 保留一段前缀是设计的一部分，所以第二条判据是
	// 「响应体量级回到了预览量级」，而不是「一个 P 都不许出现」。
	if len(body) > 8192 {
		t.Errorf("响应体 %d 字节，远超预览量级：大结果没被换成链接", len(body))
	}
	url := srv.URL + downloadPath + spillIDOf(t, body)

	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer t-ops")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("下载 %s => %d body=%.200s", url, resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), payload) {
		t.Errorf("下载内容里没有完整 payload（下载到 %d 字节）", len(raw))
	}
}

// post 发一条 MCP JSON-RPC 请求，返回响应体原文。
func post(t *testing.T, url, token, body string) string {
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
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

// spillIDOf 从文本（摘要或 MCP 响应原文）里抽出紧跟 /spill/ 的那个 id。
// 按 id 形状匹配而不是按分隔符切：响应里的摘要是 JSON 转义过的，换行是 \n 两个字符。
var idInText = regexp.MustCompile(`(?:[0-9a-f]{32}|[a-z2-7]{2,256}\.[0-9a-f]{16})`)

func spillIDOf(t *testing.T, text string) string {
	t.Helper()
	i := strings.Index(text, downloadPath)
	if i < 0 {
		t.Fatalf("文本里没有 %s: %.400q", downloadPath, text)
	}
	id := idInText.FindString(text[i+len(downloadPath):])
	if id == "" {
		t.Fatalf("%s 之后没有合法形状的 id: %.400q", downloadPath, text[i:])
	}
	return id
}
