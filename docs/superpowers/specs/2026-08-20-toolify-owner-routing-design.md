# toolify 统一 owner 路由代理设计

日期：2026-08-20
状态：已评审待实现
涉及仓库：mcp-toolify（框架，主改动）、stability-lib（sysprobe + router 接入）

## 背景与问题

多副本部署下，有状态工具的调用被负载均衡打散到不同副本会本地未命中：

- sysprobe 异步探测：任务 meta 与运行 goroutine 只在处理 `submit` 的实例上，`get_probe_status`/`cancel_probe` 落到别的副本报 `no such file`。
- spill 大结果：spill 文件只在产出实例上，`spill_explore` 落到别的副本读不到。

这两者本质相同：**一个 owner 编码的 id + 「本地处理 or 单跳到属主」**。此前分别用 `spill_explore` 的 `/spill-explore` 代理和新加的 `/peer-action` RPC 各做一套，业务侧还需自己判 owner、调转发，侵入且重复。

terminal-mcp 的 `WithSessionRouting` 已用「反代整条 MCP 调用到属主」解决了会话级路由。本设计将其泛化为按 owner 编码 id 路由的通用中间件，收敛为唯一机制。

## 目标 / 非目标

- 目标：toolify 只提供分布式路由能力；业务工具保持纯本地实现，仅「声明」某工具按某参数路由。
- 目标：`spill_explore` 与 sysprobe 全部迁到统一中间件，删除各自的代理/RPC。
- 非目标（本轮不做）：`resources/read`（`spill://<id>`）的跨副本；spill 文件下载的跨副本（现网靠下载 URL 指向属主规避）。

## 设计

### 抽象
owner 编码 id（`<owner>.<rand>`，即现有 spill id 方案）= 可路由 id。任何 `tools/call`，只要其某参数是可路由 id，框架据 owner 把整条调用反代到属主的 `/mcp`，属主当普通本地调用执行。**无需任何内部端点**。

### toolify 新增（runtime）
- `OwnerOf(id) (hostPort string, ok bool)`：`SpillOwner` 提升为中性命名（spill id 编码不变）。
- `RegisterOwnerRouted(toolName, paramName string)`：声明工具→路由参数（全局 map）。
- `WithOwnerRouting(next http.Handler) http.Handler`：中间件，骨架照搬 `WithSessionRouting`：
  - 非 POST / 带防环 header → `next`。
  - 读 body 并复位；解 `tools/call` 的 `method` 与 `params.name`、`params.arguments.<paramName>`。
  - 该工具未注册路由参数、或取不到 id、或 `OwnerOf` 无 owner、或 `owner==SpillSelfHostPort()`、或 `!SpillPeerAllowed(owner)` → `next`（本地）。
  - 否则反代 `http://<owner>/mcp`（`FlushInterval:-1` 支持 SSE，打防环 header `X-Mcp-Owner-Forwarded`）。

### toolify 导出（toolify.go）
`RegisterOwnerRouted`、`WithOwnerRouting`、`OwnerOf`；保留 `NewOwnedSpillID`/`SpillDir`（id 带 owner 的前提）。

### 删除（收敛为唯一机制）
- `runtime/peer_action.go` 及 toolify 的 PeerAction 导出、`runtime/server.go` 里 `/peer-action` 挂载。
- `spillexplore/proxy.go`、`runtime.LocalSpillExplore` 注入、`SpillExploreEndpoint`、`server.go` 里 `/spill-explore` 挂载、toolify 的 `SpillExploreEndpoint` 导出。
- `SpillExplore` 工具去掉远端代理分支，只留本地 `exploreLocal`。
- sysprobe `distributed.go` 的 RPC 转发；`get_probe_status`/`cancel_probe` 回归纯本地读/取消。

### 业务接入（声明即可）
```go
RegisterOwnerRouted("spill_explore", "id")
RegisterOwnerRouted("sysprobe.get_probe_status", "job_id")
RegisterOwnerRouted("sysprobe.cancel_probe", "job_id")
```
参数名已核实（生成 schema 的 json tag）：sysprobe 两工具为 `job_id`（mcpgen 把 `jobID` 转 snake_case），`spill_explore` 为 `id`。

### 挂载（stability-lib/servers/httpserver/router.go）
```go
router.HandleStd("ANY", "/mcp*", ptymcp.WithSessionRouting(toolify.WithOwnerRouting(mcpHandler)))
```
不再挂 `/spill-explore`、`/peer-action`。两中间件互不冲突：一条调用要么带 `session_id`（terminal），要么带可路由 id（spill/sysprobe）。

## 路由语义（owner-first）
调用前只看 id 的 owner：属主执行，读写一致、对 cancel 安全。无 owner / 旧式 id / owner==self / 不在白名单 → 本地（与现网回退语义一致）。白名单复用 `SpillPeerAllowed`（BNS 动态发现），self 复用 `SpillSelfHostPort`。

## 安全
- 反代目标受兄弟副本白名单约束（防 SSRF），与 spill peer 一致。
- 防环 header 避免转发环路。
- 属主侧按转发来的原始 header 重新跑 authz（正确，不放大权限）。

## 测试
- 框架：`WithOwnerRouting` 单测（httptest）——本地命中不转发；远端 owner 反代到属主并回传；防环 header 生效；未注册工具/无 owner/非白名单 → 本地。`OwnerOf` 正反用例。
- sysprobe：`get_probe_status`/`cancel_probe` 纯本地行为回归；owned id 生成。
- spill_explore：本地 explore 行为回归。
- 全程 httptest 本地回环，不触达生产；两仓库 `go build ./... && go vet`、`gofmt` 全过。

## 迁移 / 发布
- 联调：stability-lib 已用 `replace github.com/fzxbl/mcp-toolify => ../mcp-toolify`，本轮沿用。
- 合入前：去掉 replace，mcp-toolify 发版并 bump 依赖。
- 兼容：owned id 方案与旧式纯随机 id 并存；未开启白名单/单副本时全部走本地，行为不变。

## 边界 / 可选后续
- `resources/read`（`spill://<id>`）与 spill 文件下载的跨副本本轮不做；可后续在中间件扩展识别 `resources/read` 的 `params.uri`。
- 若将来非 tools/call 的方法也需路由，再抽象取 id 的策略。

