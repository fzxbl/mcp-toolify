# toolify 统一 owner 路由代理 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把「按 owner 编码 id 路由的分布式代理」抽象为 toolify 的一个 MCP 调用级中间件，让 spill_explore 与 sysprobe 的跨副本能力收敛为唯一机制，业务工具回归纯本地实现、仅声明路由参数。

**Architecture:** 泛化 terminal-mcp 的 `WithSessionRouting`：一个 `tools/call` 若其某参数是 owner 编码 id（`<owner>.<rand>`），中间件据 owner 把整条 MCP 调用反代到属主实例的 `/mcp`，属主当普通本地调用执行。删除 `/spill-explore`、`/peer-action` 两套内部端点及其代理/RPC 代码。

**Tech Stack:** Go 1.25、mcp-toolify runtime、`net/http/httputil.ReverseProxy`、github.com/fzxbl/mcp-toolify（stability-lib 经本地 replace 联调）。

---

## File Structure

mcp-toolify（框架）：
- Create `runtime/owner_routing.go`：`OwnerOf`、`RegisterOwnerRouted`、`WithOwnerRouting`、防环 header、param 提取、反代。
- Create `runtime/owner_routing_test.go`：中间件与 `OwnerOf` 单测。
- Modify `runtime/spill_peer.go`：删除 `SpillExploreEndpoint`/`LocalSpillExplore`/`spillExploreReq`/`SpillExploreEndpoint`；保留 token/timeout/whitelist/`SpillOwner`(改名 `OwnerOf` 的底层)/`SpillPeerAllowed`/`SpillSelfHostPort`。
- Modify `runtime/server.go`：去掉 `/spill-explore`、`/peer-action` 挂载；`runHTTP` 用 `WithOwnerRouting` 包裹协议 handler。
- Delete `runtime/peer_action.go`、`runtime/peer_action_test.go`。
- Modify `spillexplore/tool.go`：`SpillExplore` 去掉远端代理分支，只留本地 `exploreLocal`；新增 `init()` 声明路由。
- Delete `spillexplore/proxy.go`、`spillexplore/proxy_test.go`。
- Modify `toolify.go`：导出 `RegisterOwnerRouted`/`WithOwnerRouting`/`OwnerOf`；删 `SpillExploreEndpoint`、PeerAction 导出。

stability-lib（业务接入）：
- Modify `sysprobe/tools.go`：`get_probe_status`/`cancel_probe` 纯本地；恢复内联 `cancelLocal`。
- Delete `sysprobe/distributed.go`、`sysprobe/distributed_test.go`。
- Create `sysprobe/routing.go`：`init()` 声明两个工具的路由参数。
- Modify `sysprobe/config.go`：去掉 `registerPeerActions()` 调用（保留 `runtime.SpillDir()` 默认）。
- Modify `servers/httpserver/router.go`：`mcpHandler` 外包 `toolify.WithOwnerRouting`；删除 `/spill-explore`、`/sysprobe-peer`/`/peer-action` 挂载。

---

## Task 1: OwnerOf + owner-routed registry (framework)

**Files:**
- Create: `runtime/owner_routing.go`
- Test: `runtime/owner_routing_test.go`

- [ ] **Step 1: Write failing test for OwnerOf + registry**

In `runtime/owner_routing_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd mcp-toolify && go test ./runtime/ -run 'TestOwnerOf|TestRegisterOwnerRouted'`
Expected: FAIL (undefined: OwnerOf / RegisterOwnerRouted / ownerRoutedParam)

- [ ] **Step 3: Create `runtime/owner_routing.go` (registry + OwnerOf only for now)**

```go
package runtime

import "sync"

// OwnerOf 解出 owned spill id 内嵌的属主 host:port（见 spill_id.go）。
// ok=false 表示旧式/无归属 id。中性命名，供通用 owner 路由复用。
func OwnerOf(id string) (hostPort string, ok bool) { return splitOwner(id) }

var (
	ownerRoutedMu sync.RWMutex
	ownerRouted   = map[string]string{} // toolName -> routing param name
)

// RegisterOwnerRouted 声明「工具 toolName 按参数 paramName（owned id）路由」。
// 应在 init/启动期调用，早于开始处理请求。
func RegisterOwnerRouted(toolName, paramName string) {
	ownerRoutedMu.Lock()
	ownerRouted[toolName] = paramName
	ownerRoutedMu.Unlock()
}

func ownerRoutedParam(toolName string) (string, bool) {
	ownerRoutedMu.RLock()
	defer ownerRoutedMu.RUnlock()
	p, ok := ownerRouted[toolName]
	return p, ok
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd mcp-toolify && go test ./runtime/ -run 'TestOwnerOf|TestRegisterOwnerRouted'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git -C mcp-toolify add runtime/owner_routing.go runtime/owner_routing_test.go
git -C mcp-toolify commit -m "feat(runtime): OwnerOf + owner-routed tool registry"
```

## Task 2: WithOwnerRouting middleware (framework)

**Files:**
- Modify: `runtime/owner_routing.go`
- Test: `runtime/owner_routing_test.go`

- [ ] **Step 1: Write failing tests for the middleware**

Append to `runtime/owner_routing_test.go`:

```go
import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
)

func TestWithOwnerRoutingProxiesRemote(t *testing.T) {
	// 目标副本：返回哨兵，证明请求被反代过来。
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

	// 未注册工具 / 无 owner id / 已带转发标记 → 本地。
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

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return u.Host
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd mcp-toolify && go test ./runtime/ -run TestWithOwnerRouting`
Expected: FAIL (undefined: WithOwnerRouting / ownerForwardedHeader)

- [ ] **Step 3: Add middleware to `runtime/owner_routing.go`**

Add imports `bytes encoding/json io net/http net/http/httputil net/url` and:

```go
const ownerForwardedHeader = "X-Mcp-Owner-Forwarded"

// WithOwnerRouting 包裹上游 MCP handler：tools/call 若其声明的路由参数是归属兄弟副本的
// owned id，则把整条调用反代到该副本 /mcp（属主本地执行）；其余交 next 本地处理。
func WithOwnerRouting(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get(ownerForwardedHeader) != "" {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))

		owner, ok := ownerFromCall(body)
		if !ok || owner == SpillSelfHostPort() || !SpillPeerAllowed(owner) {
			next.ServeHTTP(w, r)
			return
		}
		proxyToOwner(w, r, owner, body)
	})
}

func ownerFromCall(body []byte) (string, bool) {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil || msg.Method != "tools/call" {
		return "", false
	}
	param, ok := ownerRoutedParam(msg.Params.Name)
	if !ok {
		return "", false
	}
	var args map[string]any
	if err := json.Unmarshal(msg.Params.Arguments, &args); err != nil {
		return "", false
	}
	id, _ := args[param].(string)
	if id == "" {
		return "", false
	}
	return OwnerOf(id)
}

func proxyToOwner(w http.ResponseWriter, r *http.Request, owner string, body []byte) {
	target := &url.URL{Scheme: "http", Host: owner}
	rp := &httputil.ReverseProxy{
		FlushInterval: -1, // 立即冲刷，支持 text/event-stream
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.URL.Path = "/mcp"
			req.Header.Set(ownerForwardedHeader, "1")
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		},
	}
	rp.ServeHTTP(w, r)
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd mcp-toolify && go test ./runtime/ -run 'TestWithOwnerRouting|TestOwnerOf|TestRegisterOwnerRouted'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git -C mcp-toolify add runtime/owner_routing.go runtime/owner_routing_test.go
git -C mcp-toolify commit -m "feat(runtime): WithOwnerRouting middleware (generalize call routing by owned id)"
```

## Task 3: Wire middleware into Run mode; delete peer_action & spill-explore endpoint; fix exports

**Files:**
- Modify: `runtime/server.go`
- Delete: `runtime/peer_action.go`, `runtime/peer_action_test.go`
- Modify: `runtime/spill_peer.go`
- Modify: `toolify.go`

- [ ] **Step 1: Delete the RPC layer**

```bash
git -C mcp-toolify rm runtime/peer_action.go runtime/peer_action_test.go
```

- [ ] **Step 2: In `runtime/spill_peer.go` remove spill-explore endpoint machinery**

Delete entirely: `LocalSpillExplore` var, `spillExploreReq` type, `SpillExploreEndpoint` func, and `func SpillOwner(...)` (replaced by `OwnerOf`). KEEP token/timeout/whitelist/`SpillPeerAllowed`/`SpillSelfHostPort`/`NewSpillIDForOwnerTest`/`SpillDirForTest`. Then:

Run: `cd mcp-toolify && grep -rn "SpillOwner\|LocalSpillExplore\|SpillExploreEndpoint\|spillExploreReq\|RemoteOwner\|CallPeerAction\|RegisterPeerAction\|PeerActionEndpoint" --include=*.go .`
Expected: no matches (all removed/renamed).

- [ ] **Step 3: In `runtime/server.go` runHTTP — drop internal endpoints, wrap with WithOwnerRouting**

```go
	mux := http.NewServeMux()
	mux.Handle(spillDownloadPath, SpillDownloadHandler()) // /spill/<id> 大结果下载

	var handler http.Handler
	if cfg.AuthzEnabled {
		if cfg.ConfigPath == "" {
			return fmt.Errorf("AuthzEnabled=true but ConfigPath is empty")
		}
		authzCfg, err := LoadAuthzConfig(cfg.ConfigPath)
		if err != nil {
			return fmt.Errorf("load mcp authz config: %w", err)
		}
		if err := authzCfg.Validate(); err != nil {
			return fmt.Errorf("invalid mcp authz config: %w", err)
		}
		handler = NewAuthzHandler(s, NewAuthz(authzCfg))
	} else {
		handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	}
	mux.Handle("/", HTTPAuditHeaders(WithOwnerRouting(handler)))
```

- [ ] **Step 4: In `toolify.go` swap exports**

Delete: `SpillExploreEndpoint`, `PeerActionEndpoint`, `RegisterPeerAction`, `CallPeerAction`, `RemoteOwner`. Add `RegisterOwnerRouted`, `WithOwnerRouting`, `OwnerOf` (thin wrappers over `runtime.*`).

- [ ] **Step 5: Build runtime (spillexplore fixed in Task 4)**

Run: `cd mcp-toolify && go build ./runtime/ && go vet ./runtime/ && go test ./runtime/`
Expected: PASS. `go build ./...` may still fail in `spillexplore` — fixed next task.

## Task 4: Simplify spill_explore to local-only + self-declare routing

**Files:**
- Modify: `spillexplore/tool.go`
- Delete: `spillexplore/proxy.go`, `spillexplore/proxy_test.go`

- [ ] **Step 1: Delete proxy files**

```bash
git -C mcp-toolify rm spillexplore/proxy.go spillexplore/proxy_test.go
```

- [ ] **Step 2: In `spillexplore/tool.go` — remove proxy branch + injection, add routing declaration**

Replace the `init()` that set `runtime.LocalSpillExplore` and the two-step `SpillExplore` with:

```go
func init() {
	// spill_explore 是框架自带的按 id 路由工具：跨副本由 WithOwnerRouting 反代到属主。
	runtime.RegisterOwnerRouted("spill_explore", "id")
}

func SpillExplore(id, op string, lineOffset, limit int, pattern, jqExpr string, depth, maxBytes int) (map[string]any, error) {
	path, ok := runtime.ResolveSpillPath(id)
	if !ok {
		return nil, fmt.Errorf("spill resource not found: %s", id)
	}
	return exploreAt(path, id, op, lineOffset, limit, pattern, jqExpr, depth, maxBytes)
}
```

Delete the now-unused `exploreLocal` if present. Keep `exploreAt` + helpers.

- [ ] **Step 3: Build + vet + test whole framework**

Run: `cd mcp-toolify && go build ./... && go vet ./... && go test ./... && gofmt -l runtime/ spillexplore/ toolify.go`
Expected: all PASS; gofmt lists nothing.

- [ ] **Step 4: Commit framework收敛**

```bash
git -C mcp-toolify add -A
git -C mcp-toolify commit -m "refactor: unify cross-replica routing via WithOwnerRouting; drop spill-explore/peer-action endpoints"
```

## Task 5: sysprobe — pure-local get/cancel + self-declare routing (stability-lib)

**Files:**
- Modify: `sysprobe/tools.go`
- Delete: `sysprobe/distributed.go`, `sysprobe/distributed_test.go`
- Create: `sysprobe/routing.go`
- Modify: `sysprobe/config.go`

- [ ] **Step 1: Delete the sysprobe RPC layer**

```bash
git -C stability-lib rm sysprobe/distributed.go sysprobe/distributed_test.go
```

- [ ] **Step 2: In `sysprobe/tools.go` make get/cancel pure-local (restore inline cancel)**

```go
func GetProbeStatus(jobID string) (ScanMeta, error) {
	Init()
	return readMeta(metaPath(cfg.SpillDir, jobID))
}

func CancelProbe(jobID string) (map[string]string, error) {
	Init()
	ok := theStore.cancel(jobID)
	m, err := readMeta(metaPath(cfg.SpillDir, jobID))
	if err == nil && m.Status == "running" {
		m.Status = "canceled"
		_ = writeMeta(metaPath(cfg.SpillDir, jobID), m)
	}
	if !ok {
		return map[string]string{"job_id": jobID, "status": "not_running"}, nil
	}
	return map[string]string{"job_id": jobID, "status": "canceled"}, nil
}
```

Keep `SubmitProbe` using `runtime.NewOwnedSpillID()` for jobID/resultID/logID (unchanged). Update get/cancel doc comments to note cross-replica is handled by the framework (WithOwnerRouting), not by these functions.

- [ ] **Step 3: Create `sysprobe/routing.go`**

```go
package sysprobe

import "github.com/fzxbl/mcp-toolify/runtime"

// 声明 sysprobe 的有状态工具按 job_id 路由：多副本下框架 WithOwnerRouting 会把归属兄弟副本的
// 调用整条反代到属主实例执行。参数名与生成 schema 的 json tag 一致（jobID → job_id）。
func init() {
	runtime.RegisterOwnerRouted("sysprobe.get_probe_status", "job_id")
	runtime.RegisterOwnerRouted("sysprobe.cancel_probe", "job_id")
}
```

- [ ] **Step 4: In `sysprobe/config.go` drop `registerPeerActions()`**

Remove the `registerPeerActions()` call at the end of `Init()`'s `Once`. Keep the `runtime.SpillDir()` default and the `runtime` import.

- [ ] **Step 5: Build + vet + test sysprobe**

Run: `cd stability-lib && go build ./sysprobe/... && go vet ./sysprobe/ && go test ./sysprobe/`
Expected: PASS (existing TestSubmitRejectsOverLimit / TestConcurrencyLimit / TestUlimitPrefix green).

- [ ] **Step 6: Commit**

```bash
git -C stability-lib add sysprobe/
git -C stability-lib commit -m "refactor(sysprobe): pure-local status/cancel; declare owner-routing via framework"
```

## Task 6: Router — wrap WithOwnerRouting, drop internal endpoint mounts (stability-lib)

**Files:**
- Modify: `servers/httpserver/router.go`

- [ ] **Step 1: Replace the MCP mount block**

```go
	// owner 路由（分布式）：把归属兄弟副本的 tools/call 反代到属主 /mcp；与会话路由组合。
	router.HandleStd("ANY", "/mcp*", ptymcp.WithSessionRouting(toolify.WithOwnerRouting(mcpHandler)))
	router.HandleStd("ANY", "/spill/*", spillHandler)
```

Delete the `router.HandleStd("ANY", "/spill-explore", ...)` and any `/sysprobe-peer` / `/peer-action` mounts. Remove now-unused imports (`sysprobe` if only used for a removed endpoint — check with build).

- [ ] **Step 2: Build + vet**

Run: `cd stability-lib && go build ./servers/httpserver/... && go vet ./servers/httpserver/`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git -C stability-lib add servers/httpserver/router.go
git -C stability-lib commit -m "feat(mcp): wrap MCP handler with owner-routing middleware; drop internal endpoints"
```

## Task 7: Full verification (both repos)

**Files:** none (verification only)

- [ ] **Step 1: Framework full suite**

Run: `cd mcp-toolify && go build ./... && go vet ./... && go test ./... && gofmt -l .`
Expected: all PASS; gofmt lists nothing.

- [ ] **Step 2: stability-lib build + targeted tests + gofmt**

Run: `cd stability-lib && go build ./... && go vet ./sysprobe/ ./servers/httpserver/ && go test ./sysprobe/ && gofmt -l sysprobe/ servers/httpserver/`
Expected: all PASS; gofmt lists nothing. (Do NOT run `go test ./...` — livegate-guarded packages touch production.)

- [ ] **Step 3: Confirm no dangling references**

Run: `cd stability-lib && grep -rn "spill-explore\|peer-action\|sysprobe-peer\|CallPeerAction\|RemoteOwner\|ProbePeerEndpoint" --include=*.go .`
Expected: no matches.

- [ ] **Step 4: Follow-up note**

Confirm go.mod still has local `replace github.com/fzxbl/mcp-toolify => ../mcp-toolify` (联调). Before merge: remove the replace, tag a mcp-toolify release, bump the dependency.



