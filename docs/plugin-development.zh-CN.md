[English](plugin-development.md) | 简体中文

[返回根 README](../README.zh-CN.md)

# 插件开发

## 1. 插件形态与依赖边界

插件是一个普通 Go 包，入口为：

```go
func Install(r *runtime.Registry) error
```

仓内插件与外部插件使用同一公开接口，没有额外特权。插件生产代码只能依赖公开的
`runtime` 与 `selector` 包，不得依赖基座的内部包，也不应依赖其他插件。仓内测试
`TestPluginsOnlyDependOnPublicPackages`（`runtime/externalizable_test.go`）会检查
`plugins/...` 的生产代码依赖；测试导入不在该检查范围内。

## 2. Registry 公共插件面

`Registry` 提供以下插件接口：

| 方法 | 用途 |
| --- | --- |
| `Named` | 声明插件名；参与 `required_plugins` 检查和启动日志 |
| `Config` | 解码并认领插件自己的 TOML 配置段 |
| `Use` | 注册 `func(next runtime.Handler) runtime.Handler` 中间件 |
| `Tool` | 注册插件自带的 MCP 工具 |
| `Route` / `RoutePublic` | 分别注册需要 token 鉴权和明确免鉴权的 HTTP 路由 |
| `OnBuild` | 注册启动前校验钩子 |
| `OnStop` | 注册退出清理钩子 |

配置必须解码到具名 struct，不能用 `map[string]any` 或其他 map 兜底。文档承诺的
每个字段都必须在 struct 中声明；否则认领检查会把合法配置报为无人认领。配置文件中
没有被基座或插件认领的键，以及安装了插件段但未安装对应插件，都会导致启动失败。

插件自带的工具还必须同时登记 labels：

```go
r.Tool(addTool)
runtime.RegisterTool(runtime.ToolInfo{
	Name: "example.status", Pkg: "example",
	Labels: map[string]string{"capability": "read", "risk": "none"},
})
```

缺少 labels 登记的运行期工具在准入规则下不可见、不可执行。生成工具的 labels 由
生成代码处理；手写或插件运行期工具不能省略这一步。

## 3. 中间件顺序

`r.Use` 的调用顺序就是进入顺序：先安装者在外层，后安装者在内层。常见结构为：

```text
audit ▸ 判断类策略 ▸ 等待/副作用类策略 ▸ spill
```

- audit 放外层，使内层拒绝也能被记录。
- 判断类策略（例如只做准入判断）应先于等待或产生副作用的策略。
- 等待人工决定、扣配额、写外部计数等策略属于等待/副作用类；在允许继续前不要
  调用 `next`，避免先执行后撤销。
- spill 放内层，使它看到最终的原始结果并判断大小。

这些是链上的相对顺序约束；存在多个同类插件时，按其实际副作用和策略依赖排列。

## 4. 构建与停止生命周期

`OnBuild` 在基座完成配置加载、配置认领、必需插件和其他自身校验后，且在 handler
组装、开始接收请求前执行。钩子按注册顺序执行；返回 error 或发生 panic 都会使
启动失败，panic 会被转换为启动错误并记录堆栈。

`OnBuild` 的契约是只做校验和日志；钩子不应调用 `Use`、`Tool`、`Route`、
`RoutePublic`、`Named`、`Config` 或 `OnBuild`。当前基座会比较可观测的注册状态，
检测到变化时拒绝启动，但插件不能依赖该比较覆盖所有无效果或失败的调用。
`OnStop` 是清理例外，可以在钩子中登记；这符合源码允许的停止阶段登记语义。

插件应先登记 `OnStop`，再启动后台协程、打开连接池或创建其他资源，确保后续
`Install` 失败仍可回收。宿主退出时调用：

```go
r.RunStop(context.Background())
```

`RunStop` 幂等；启动失败时基座也会执行一次。清理后的 Registry 不得复用来重试
启动，应创建新的 Registry 并重新安装插件。

## 5. `Call` 关键契约

中间件收到 `*runtime.Call`。字段来源、可写性和关键约束如下：

| 字段 | 来源 | 可写性与关键约束 |
| --- | --- | --- |
| `Method` | JSON-RPC 请求方法 | 插件可读；通常按 `tools/call`、`tools/list` 分支 |
| `Tool` | `tools/call` 的工具名 | 可写，执行终点使用改写后的值；按工具名判断的策略须考虑后续改写 |
| `Labels` | 工具注册信息，含 `name`/`pkg` 投影 | 可读；用于 selector 或插件策略，基座只负责匹配 |
| `Args` | `tools/call` 的原始 JSON 参数 | 可写，执行终点使用改写后的值；交给其他组件前应防御性拷贝 |
| `Headers` | HTTP 请求头快照 | 只读契约；已剔除凭据，插件不得修改，也不应从原始请求另取凭据 |
| `Subject` | token 鉴权阶段解析的调用主体 | 可读；`ID` 默认空，`Token` 是 token 用途名而非 token 原文；按人策略须拒绝空身份 |
| `LogID` | HTTP logid 层生成或接收的请求标识 | 可读，用于关联请求和日志 |
| `Meta` | runtime 创建或由插件写入的插件通信 map | 可写；跨插件固定键使用基座常量，私有键加 `<插件名>.` 前缀 |
| `Tools` | 基座注入的工具查询函数 | 可调用但调用前必须判空；用于需要读取工具清单的插件逻辑 |

`DenyResult` 会向 `Meta` 写入 `denied_by` 和 `deny_reason`，并返回 MCP
`IsError` 结果，供调用方看到业务级拒绝原因。基座 token 准入在进入插件链前解析，
因此插件改写 `Tool` 不会改变已经解析的准入结果。

## 6. 阻塞式插件

等待人工确认、外部系统或锁的插件应遵守：

1. 在 `next` 之前完成等待和所有拒绝路径；未获允许、超时、客户端取消、通知失败
   或进程重启都不能到达 `next`。
2. 服务端等待上限应明显短于 MCP client 的单次调用超时，并处理 `ctx.Done()`。
3. 同时设置全局待处理上限和按人上限，避免一个调用方占满等待资源。
4. 按 `(Subject.ID, tool, 规范化 args)` 做幂等；重试应加入既有待处理记录，而不是
   重复创建审批。
5. 空 `Subject.ID` 在登记等待前拒绝。
6. 等待状态在内存中，重启后失败并告警；不要假定重启后仍然待处理。

插件 HTTP callback 使用 `r.Route` 时，得到的是 token 认证，不是按人授权。持有
有效 token 且知道待处理标识的调用方可能代表其他人提交决定，因此 callback 服务
必须位于信任路径内。多副本场景还要用 owner routing 将 callback 转回保存等待状态
的副本；见[Owner routing](owner-routing.zh-CN.md)。

## 7. 实现规范

- 测试注入点优先使用实例字段或显式依赖，避免包级可变变量；宿主注册 hook
  （例如必须在插件实例创建前设置的 hook）是例外。
- 对交给宿主或其他组件的输入做防御性拷贝，尤其是可能与 SDK buffer 共享底层数组的
  `Call.Args`。
- 请求路径告警采用限流和汇总，不要逐请求无界打印。
- 启动日志声明生效策略（例如阈值、上限、规则和依赖），便于确认配置确实生效。
- 本地文件读取的路径约束、符号链接和普通文件检查等专属安全细节，请参阅
  [spill 文档](../plugins/spill/README.zh-CN.md)，不要复制为通用插件规则。
