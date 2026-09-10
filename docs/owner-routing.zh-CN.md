[English](owner-routing.md) | 简体中文

[返回根 README](../README.zh-CN.md)

# Owner routing

## 1. 适用场景

当资源只存在于创建它的副本时，负载均衡后的后续请求可能落到错误副本，例如：
大结果、交互会话、长任务句柄和等待带外确认的请求。Owner routing 将资源 ID
编码为属主副本，并把后续工具调用或 HTTP 请求单跳转发到属主；它不要求共享存储。

```go
id := runtime.NewOwnedID()
owner, ok := runtime.OwnerOf(id)
```

`NewOwnedID` 读取进程级 `SelfAddr`。如果进程尚未设置它，函数返回无归属的随机
ID，`OwnerOf` 对该 ID 返回 `ok == false`，因此不发生 owner 路由；这不等同于
`Config` 未显式配置该字段。独立 `Start` 在监听成功后会用实际监听地址回退设置
`SelfAddr`，所以这种场景仍可生成带属主的 ID，但仅适合同机或本地使用。
`Handlers` 的嵌入模式不会自动获知宿主地址，必须由宿主显式配置或调用设置接口；
否则保持无归属 ID，不启用 owner 路由。已设置时，`NewOwnedID` 生成包含本副本
`host:port` 的 ID，`OwnerOf` 返回该属主。

## 2. 注册路由声明

### tools/call

工具参数携带 owned ID 时，在启动前注册：

```go
runtime.RegisterOwnerRouted("jobs.get_status", "job_id")
runtime.RegisterOwnerRouted("jobs.cancel",     "job_id")
```

请求方法为 `POST` 且 JSON-RPC 方法为 `tools/call` 时，runtime 从指定参数提取 ID。

### HTTP route

插件自己的 pattern 应使用：

```go
runtime.RegisterOwnerRoutedRoute("/jobs/", func(r *http.Request) string {
	return strings.TrimPrefix(r.URL.Path, runtime.RoutePath("/jobs/"))
})
```

`RegisterOwnerRoutedRoute(pattern, extractor)` 接收插件自己的 pattern，由 runtime
按宿主在 `Registry.Mount` 里给出的挂载前缀补成实际请求路径，适合可嵌入、可改变前缀的插件；
它是路径形态 owner 路由的**唯一入口**。提取器返回空字符串表示本次不参与 owner 路由——
用它表达「本地已经能回答」这类情形（spill 的下载端点就靠它做本地优先，避免绕一趟属主）。

路径形态只有这一个登记入口：pattern 在**匹配时**才补前缀，宿主挂在根上时补空串——
「有前缀」与「挂在根上」是同一条代码路径，因此不需要另一个「按裸绝对路径登记」的函数
（那个函数在有前缀的部署里永远匹配不上，漏配只在多副本暴露，已删除）。换算发生在匹配时
而不是登记时，所以登记与 `Mount` 声明前缀的先后顺序无关——插件在 `init` 里登记
也照样按加了前缀的实际路径生效。

登记了 pattern 却没有对应插件路由（比如把 `/spill/` 写成 `/spil/`）时，`Mount` 直接
返回 error 让启动失败：这类漏配在单副本下毫无症状，只在跨副本转发时静默 404。

提取器可以从请求的任何位置取 id：路径（推荐）、query、header，甚至加密的 body
（confirm 的回执 id 就在卡片载荷里）。读 body 的话有两件事必须自己兜住：**限长**
（body 大小由外部决定，裸 `io.ReadAll` 是一个免费的内存放大器）与**复位**
（`r.Body = io.NopCloser(bytes.NewReader(b))`）。基座在路径形态下刻意不缓冲 body，
漏了复位，属主副本收到的就是空请求体，而本地单副本一切正常。

## 3. 路径、URL 与 handler

```go
runtime.RoutePath("/jobs/")       // 挂载前缀 + "/plugin" + "/jobs/"
runtime.PublicURL("/jobs/" + id)  // PublicBaseURL（缺省=http://<SelfAddr>）+ RoutePath(...)
```

`RoutePath` 用于实际路由路径和转发目标；`PublicURL` 用于提供给调用方的绝对地址，
没有任何对外地址时返回空字符串，调用方应判空。

两个地址分工不同，别混用：`SelfAddr` 是**副本身份**（判 id 归属、副本间拨号），
是一个裸 `host:port`，必须本副本可直连、不能填负载均衡；`PublicBaseURL` 是**交给外部的入口**
（agent、人的浏览器、回调平台），只用于 `PublicURL`，可以填域名/VIP——属主信息在 id 里，
请求落到任意副本都会被反代到属主。不配 `PublicBaseURL` 时由 `http://<SelfAddr>` 推导。

基座自己把 MCP handler 与插件路由都包好了，宿主不需要做任何事：`Registry.Mount` 交出来的
MCP handler 走包内的「MCP 形态」包裹（工具参数 + 路径两种提取形态），插件路由走
`WithPathOwnerRouting`（只有路径形态）。

两者不合成一个的原因是**要不要缓冲 POST body**：MCP 形态得把整个 body 读进内存才能解析
JSON-RPC 找到工具参数里的 id；插件路由的 POST body 形状与大小不由基座决定（上传类路由
完全合法），无条件缓冲它是一次白送的内存放大器。插件路由要跨副本就把 id 放进路径。
只有 `WithPathOwnerRouting` 是导出的——插件的用例用它复现「生产形态的入口」；
MCP 那条没有基座之外的调用场合。

路径路由的请求保留原始 path 和 method，因此 `GET`、`POST`、`DELETE` 等回调都可转发。

转发使用单跳防环标记；已转发请求不会再次转发。远端副本收到请求后会重新执行
token 认证，owner routing 不会授予额外权限。流式响应保持流式转发。属主不可达时转发回
`404`、只把属主地址写进服务端日志：这条路任何持合法 token 的调用方都能触发，回显属主
等于把内网拓扑告诉调用方。每次转发都会记一行 `[mcp] owner routing:`（方法、路径、属主、
logid）——跨副本转发在本副本原本不留痕迹，排查「这个回调到底去哪了」需要它。

一个工具都没登记按参数路由时，MCP 形态不读 body 直接放行：读 body 是每个 POST 都要付的
一次全量拷贝，而「本部署只用路径形态」很常见。

基座里只有这一份转发实现。插件只声明提取器、不自己反代——spill 的下载端点曾经自带一套
反向代理与防环头，现在已经收回基座。

## 4. 副本白名单与时机

转发目标只能来自 peer 白名单：

```go
Config{Peers: []string{"replica-a:8011", "replica-b:8011"}} // 静态配置
toolify.SetPeerProvider(func() []string {                   // 动态服务发现
	return currentReplicaHostPorts()
})
```

静态 `Config.Peers` 与 `SetPeerProvider` 互为覆盖，后设置者生效。没有 provider 且白名单
为空时拒绝所有远端转发；请求留在本地处理，避免把 ID 当作任意代理目标。

`SelfAddr`、`PublicBaseURL`、Peers/provider 和 owner 声明属于进程级状态，必须在开始接收请求前
配置。`ResetOwnerRoutedPathsForTest` 只能用于测试清理，禁止在运行期调用；运行期清空
会让全部回调路径静默失效。

## 5. 部署约束

- 所有副本必须使用相同的挂载前缀（同一份二进制、同一份配置即可满足）。
- 挂载前缀只有一个来源：宿主调用 `Registry.Mount(prefix, mount)` 时给出的 `prefix`。
  基座据此保证三处同源：`Mount` 交给宿主的 pattern、`PublicURL` 生成的 URL、
  `RoutePath` 生成的转发目标。前缀必须以 `/` 开头、不能以 `/` 结尾，不能含 `*`、`?`、
  ASCII 空格或制表符；写错时 `Mount` 返回 error 且一条路由都不挂。
- 嵌入已有 HTTP server 时要显式设置 `SelfAddr`；它必须是本副本可直连的 `host:port`
  （交给外部的链接另配 `PublicBaseURL`），
  不能填写负载均衡器地址。独立启动且未显式设置时，runtime 才会回退到实际监听地址，
  仅适合同机或本地场景。
- 宿主不应在 mount 时再改写 `Mount` 交过来的 pattern（ghttp 这类需要补通配符后缀的
  router 除外）：pattern 已经是对外绝对路径，改写它就会让转发目标与实际路由不一致。

## 6. 安全边界

当前 runtime 没有 TLS transport、peer credential 或自定义 proxy transport 的直接
配置入口。副本间转发使用明文 `http://`，并原样透传调用方的 Bearer token；因此
应仅在相同且受信的网络中部署 peer。跨越不受信网络时，需要在 runtime 外提供
TLS 或网络代理，或自行扩展内部认证；peer 白名单和 owned ID 本身不提供传输安全。
因为 token 会随请求传递，能监听该网络的主体可以获得调用权限。

插件处于进程信任边界内而不是沙箱中。插件可通过公开 API 注册 owner 声明、影响路径
转发，且可改写 `Call.Tool` 和 `Call.Args`；公开 API 不能替代代码审查。只安装经过
审查并与该信任边界相符的插件。
