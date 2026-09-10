# 运行时

[English](runtime.md) | 简体中文

返回[根 README](../README.zh-CN.md)。

运行时包提供 `Registry`、HTTP MCP handler、token 认证、label 准入、logid、插件
中间件和插件 HTTP 路由。插件安装完成后，基座在接收请求前完成配置与注册校验。

## 配置

`toolify.Config` 是 `runtime.Config` 的别名：

| 字段 | 语义 |
| --- | --- |
| `Addr` | HTTP 监听地址，例如 `:8080`；为空时监听系统分配的端口。 |
| `SelfAddr` | **本副本可直连**的 `host:port`，例如 `10.1.2.3:8011`。它是副本身份（判 id 归属与副本间拨号），不能填负载均衡地址。为空时独立启动回退到实际监听地址。 |
| `PublicBaseURL` | **交给外部**的入口地址，例如 `https://mcp.example.com`，只用于 `PublicURL` 拼绝对链接；可为域名/VIP。为空则由 `http://<SelfAddr>` 推导。 |
| `ConfigPath` | 含基座 TOML（至少一个 `[[tokens]]`）的路径。为空、文件不可读或无 token 时启动失败。 |
| `Enable` | 注册期包名白名单；为空表示不过滤。 |
| `Match` | 注册期 label selector；为空或全空白表示不过滤，语法错误使启动失败。 |
| `RequiredPlugins` | 必须安装的插件名；与配置文件顶层 `required_plugins` 取并集，缺失使启动失败。 |
| `Peers` | 静态兄弟副本 `host:port` 白名单；也可用 `SetPeerProvider` 动态提供，后设置者覆盖先设置者。 |

`ConfigPath` 与至少一个 token 是启动前提；运行时没有关闭 token 鉴权的配置开关。
完整字段示例见 [`config.example.toml`](../config.example.toml)。

### 进程级状态

以下状态由进程内共享，而不是按 `Registry` 实例隔离：挂载前缀（由 `Mount` 给出）、owner-routed
工具声明、owner-routed 路径声明，以及 peer 白名单/provider。一个进程只应装配一套
彼此兼容的配置；需要不同前缀、owner 声明或 peer 集合时应拆分到不同进程。
`Config.Peers`、`SetPeerProvider` 和 owner route 注册应在开始接收流量前完成。

### TOML 基座段

顶层键包括 `identity_headers`、`trust_identity_header`、
`required_plugins`、`[log]` 和 `[[tokens]]`。基座解码后会检查配置认领情况：
未知键、拼错的段落或字段、以及存在但没有安装对应插件的插件段落都会使启动失败。
插件应通过 `Registry.Config` 使用具名结构体解码自己的段落；用 map 兜底会削弱
字段拼写检查。

* `identity_headers` 是有序请求头列表，取第一个非空值；省略或清空时默认为
  `["X-MCP-User"]`。只有 `trust_identity_header = true` 时才读取。
* `trust_identity_header` 默认 `false`。开启它的前提是前置可信网关已经认证调用人
  并覆盖这些请求头；基座不据此实现按人授权。
* `required_plugins` 是插件名数组，和 `Config.RequiredPlugins` 合并。
* `[log].logid_header` 同时指定入站和出站 logid 头；省略或空值默认为
  `X-Log-Id`。
* 每个 `[[tokens]]` 的 `token`、`name`、`applicant` 均必须非空；`token` 和
  `name` 不能与其他条目重复。`allow` 和 `deny` 是 selector 字符串数组，
  每条数组内为 OR。
* `identity` 可选且只接受 `fixed:<非空 id>`。绑定后完全不读取请求身份头，即使
  `trust_identity_header` 开启；没有固定身份且不信任请求头时 `Subject.ID` 为空。

token 通过 `Authorization: Bearer <token>` 传入，也兼容直接传裸 token。未配置或
无法识别的 token 返回 HTTP 401，不在错误中回显 token。`TokenConfig.Name` 进入
`Subject.Token`，这里是用途名而不是 token 密文；`Applicant` 仅用于配置留痕。

## 工具准入和 selector

准入规则只看工具注册表中的 labels。`deny` 命中任一条就拒绝；否则必须命中
`allow` 任一条，既不命中 allow 也拒绝（default deny）。同一规则同时用于
`tools/list` 的可见性和 `tools/call` 的执行，未登记 labels 的工具两者都不可用。
准入在插件链之前执行。

selector 语法和语义：

* 单条 selector 内的逗号条件是 AND；
* 一个 `allow` 或 `deny` 列表的多条 selector 是 OR；
* 支持 `key=value`、`key!=value`、`key in (a,b)`、`key notin (a,b)`、裸
  `key`（存在）和 `!key`（不存在）；
* `in` / `notin` 前后必须各有一个空格，值必须用括号包围；
* key 不能包含操作符字符或空白，value 不能包含 `= ! ( ) ,`，value 可含空格；
* `!=` 和 `notin` 按 Kubernetes 语义对缺失 key 也匹配；为避免新工具漏标导致
  放行，相关 allow 规则应配 `deny = ["!<key>"]` 兜底；
* 空 selector（空字符串或全空白）匹配全部；但未经过 `Parse` 构造的零值
  `Selector{}` 不匹配任何内容；
* 注册期 `Match` 额外注入内置投影 `pkg`（包名）。运行时工具标签还带有内置
  `name`（工具名）和 `pkg` 投影；这些投影覆盖同名的自定义值，不应自行设置。

生成工具的注册期过滤由 `Enable` 和 `Match` 共同决定；selector 合法但没有工具
匹配时只会注册零个工具，并在启动日志中给出 registered/filtered 计数。

## Registry 生命周期

独立运行：

```go
r := toolify.New(cfg, tools.RegisterAll)
// plugin.Install(r)
err := r.Start(ctx)
```

嵌入已有 HTTP server：

```go
r := toolify.New(cfg, tools.RegisterAll)
defer r.RunStop(context.Background())
if err := r.Mount("/mcp", func(pattern string, h http.Handler) {
    mux.Handle(pattern, h) // 需要通配符的 router 在这里补 pattern+"*"
}); err != nil {
    log.Fatal(err)
}
```

`New` 创建 registry 并注册生成工具；插件随后调用 `Named`、`Use`、`Route` /
`RoutePublic`、`Config` 等完成安装。`Start` 自己监听并阻塞到 context 取消或 server
退出；`Mount` 不监听端口，只把 handler 与路由交给宿主的 router，并记下挂载前缀；
`Handlers` 是 `Mount` 的下层，供「完全自己组装」的场景使用（那时没有挂载前缀）。
三者都会执行基座配置加载、配置认领、必需插件、selector 和插件 build hook 校验。

按当前实现，`Handlers` 的组装和 build hook 具有幂等语义：中间件只安装一次，build
hook 按注册顺序最多执行一次；hook 返回 error 或 panic 会让启动失败，后续 hook 不
执行。hook 只应做校验和日志，不能在其中调用 `Use`、`Tool`、`Route`、`RoutePublic`、
`Named`、`Config` 或 `OnBuild`；这类注册表变化会被拒绝。`OnStop` 可在 hook 中登记。

`RunStop` 执行清理钩子且幂等；单个清理 hook panic 不会阻止后续 hook。启动失败时
基座也会自动清理。清理执行过的 `Registry` 不可再次 `Start` 或 `Handlers`，重试
必须新建 registry 并重新安装插件。`Route` 重复 pattern（包括与 `RoutePublic`
跨类型重复）同样使启动失败。

## HTTP 路由和前缀

`Route(pattern, h)` 注册需要 token 认证的插件路由；`RoutePublic(pattern, h)` 显式
注册免认证路由，适合健康检查。`Mount(prefix, mountFunc)` 交给宿主的 pattern 已是加好
前缀的实际路径，宿主必须原样挂载、不能再次拼接或改写。需要认证的路由在 token 鉴权尚未
完成时不会外露。

布局固定：MCP 端点在 `prefix` 本身（空前缀时是 `/`），插件路由在 `prefix + "/plugin"`
之下。同一个前缀还决定 `RoutePath` / `PublicURL` 生成的插件地址与 owner routing 的转发
目标路径——三处同源，且只有 `Mount` 的那一个参数可以改。所有副本必须使用一致前缀。
`Mount` 同时校验「登记了 owner 路由的 pattern 都有对应路由」，不一致即启动失败。
插件若需要对外 URL，应配置本副本可直连的
`SelfAddr`（它是副本身份，填负载均衡会让所有副本看起来是同一个）；交给外部的链接取
`PublicBaseURL`，那一项可以是域名/VIP。

## logid

每个 HTTP 请求经 `HTTPLogID` 处理：从配置的头（默认 `X-Log-Id`）读取，缺失或
不合规时生成 16 位小写十六进制值；合规值长度为 1 至 64 字节，字符集仅允许
`A-Z a-z 0-9 . _ - :`。该值写回同名响应头、放入 context，并通过 `Call.LogID`
提供给插件。不合规输入会被丢弃并限流告警；合规输入不会去重，因此调用方重复
使用同一 logid 时无法保证事件一一对应。

## 插件安全边界

插件位于进程信任边界内，不是沙箱。中间件可以读取和修改 `Call`；其中
`Call.Tool` 与 `Call.Args` 会在下游执行前回写到合成的 MCP 请求，因此改写工具名
或参数会实际改变最终执行内容。插件必须经过审查，不能把不可信输入直接变成工具
选择或参数改写。`Call.Headers` 是快照，且基座剔除 `Authorization`、
`Proxy-Authorization`、`Cookie`、`Set-Cookie`；插件不应把凭据重新放入日志或结果。
插件间共享的 `Call.Meta` key 应使用约定常量或插件名前缀。

有状态插件的副本归属、`RegisterOwnerRouted`、`RegisterOwnerRoutedRoute`、peer
白名单和转发边界详见 [owner routing 文档](owner-routing.zh-CN.md)，本文不重复展开。
