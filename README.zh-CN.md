# mcp-toolify

[English](README.md) | **简体中文**

[![Go Reference](https://pkg.go.dev/badge/github.com/fzxbl/mcp-toolify.svg)](https://pkg.go.dev/github.com/fzxbl/mcp-toolify)
[![Go 1.25+](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![MCP](https://img.shields.io/badge/MCP-Model%20Context%20Protocol-6E56CF)](https://modelcontextprotocol.io)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Stars](https://img.shields.io/github/stars/fzxbl/mcp-toolify?style=social)](https://github.com/fzxbl/mcp-toolify/stargazers)

**给一个普通 Go 函数加一行注释 `// mcp:tool`，`go generate`，它就成了一个 MCP 工具。**

`mcp-toolify` 是一套用 Go 构建 [Model Context Protocol](https://modelcontextprotocol.io) 服务的**代码生成器 + 运行时**。在普通函数的 godoc 上加 `// mcp:tool`，跑一次 `go generate`，它就变成一个类型完整的 MCP 工具——入参结构体、JSON schema、注册代码全部自动生成。无需手写 wrapper，运行期不用反射。

运行时是一个**最小基座**：Streamable HTTP 上的 MCP 协议、带不透明 labels 的工具注册表、k8s 风格 label selector 引擎、token 认证与准入、请求 logid、有状态工具的属主路由，以及**一条**中间件链。其余能力——审计、大结果落盘、配额、二次确认——都是挂到这条链上的**插件**；本仓库自带两个（`plugins/audit`、`plugins/spill`），其余各自独立成 module。本次部署没装的插件不进编译产物。

Go 编写，官方 MCP SDK。可直接接入 Cursor、Claude、Comate 或任意 MCP 客户端，也可嵌入你已有的 HTTP 服务。

---

## 为什么用 mcp-toolify

把 Go 逻辑暴露成 MCP 工具，通常要为每个函数手写一层 wrapper：入参结构体、JSON schema、参数说明、拆包/打包的 handler，再加注册样板代码。这些代码一旦改动就会和真实函数脱节，而且完全没表达风险、体积、鉴权等信息。mcp-toolify 抓住了六点：

- **1. 注解驱动、零样板——代码本身就是规范。** 在函数 godoc 上加 `// mcp:tool`，独立生成器（`cmd/mcpgen`）就产出类型化 wrapper：入参结构体来自形参，JSON schema 描述来自 `param:` 行，工具描述来自 doc 注释。生成代码直接调用你的函数——**运行期零反射**，工具永远不会和函数签名悄悄脱节。
- **2. 一条供插件挂载的中间件链——基座保持很小。** 每次 `tools/call` 与 `tools/list` 都走同一条 `func(next Handler) Handler` 洋葱链。横切能力都是用 `r.Use(...)` 装上去的插件，基座一个都不自带、也不认识它们是什么。必须要有某个插件的部署在 `required_plugins` 里声明，忘装就启动失败，而不是静默失效。
- **3. 跨副本的有状态工具——资源只在某个副本上，调用却可能落到任意副本。** 把属主副本编码进资源 id，用 `RegisterOwnerRouted(tool, param)` 声明哪个入参携带该 id，调用级中间件就会把整个 `tools/call` 反向代理到属主副本——在属主端重新鉴权、受兄弟副本白名单约束、单跳。你的工具依然是一个普通的本地函数。
- **4. 基于 label selector 的鉴权，deny-by-default。** 工具用 `mcp:labels=` 标注不透明 k/v；每个 token 配 k8s 风格的 `allow` / `deny` selector。同一套 selector 同时过滤 `tools/list` 与拦截 `tools/call`，「看不见」与「不能执行」不会走偏，未标注的工具一律被拒。
- **5. 独立运行 *或* 嵌入复用——共用一个 server。** 既可作为独立 HTTP 进程运行，也可拿到 MCP handler 与插件路由，**挂载到你已有的 HTTP server 上**，共用端口与生命周期。手写工具也能注册到同一个 server（只需登记其 labels）。
- **6. 零私有依赖——干净、可移植、可审计。** 基座只依赖官方 Go MCP SDK、`jsonschema-go` 与 `BurntSushi/toml`；生成器额外用 `golang.org/x/tools` 与 `yaml.v3`（仅代码生成时），spill 插件为它的探索工具带一个 `gojq`。外置插件的依赖（Redis 驱动、审批系统客户端）只在你安装那个插件时才进来。

以及一批让上述能力可靠落地的机制：

- **对棘手类型给出诚实的 schema。** `interface{}` 参数会生成显式的半受限 JSON schema（类型联合），而不是 SDK 那种无约束空节点；多返回值打包成稳定的、带字段名的 JSON 对象。模型无法用 JSON 构造的接口参数用 `mcp:bind=param:Type` 绑定到具体类型，`mcp:import=<path>` 引入该类型所在的外部包。
- **配置 fail-closed。** 没有 token、selector 语法错误、缺少必需插件、配置里出现未知/拼错的键、某个插件的启动期自检没过——一律启动失败。静默降级的服务比起不来的服务更危险。

---

## 架构：基座只保留什么，插件负责什么

```
HTTP 请求
  └ logid ──► 请求头快照 ──► token 认证（401）──► 属主路由
                                                  └ MCP handler
                                                      └ token 准入（allow/deny）
                                                          └ 插件链：(audit) ▸ (二次确认) ▸ (quota) ▸ spill
                                                              └ 统一执行终点 ──► 你的 Go 函数
```

**基座（`runtime/`，由根包再导出）只保留这些：**

- Streamable HTTP 上的 MCP 协议，**stateless**：每个请求重读 `Authorization` 与身份头，从而支持「一个 agent 用同一条连接服务多人」。
- 工具注册表：名字、包名与**不透明**的 labels。基座从不解释任何 label key。
- `selector/` 引擎（k8s label selector 语法），基座在它之上只做 token 准入这一件事。
- token 认证（HTTP 层 401）与准入：`allow` / `deny` 同时过滤 `tools/list` 与 `tools/call`。
- 请求 logid：注入 ctx、回写同名响应头。
- 有状态工具的属主路由，`tools/call` 参数与插件 HTTP 路由两种形态都支持（`RegisterOwnerRouted`、`RegisterOwnerRoutedPath`、`WithOwnerRouting`、`NewOwnedID`、`OwnerOf`）。
- 一条中间件链、插件唯一依赖的 `Registry`，以及一个统一执行终点。一次调用最多执行一次：不存在可供插件重新进入的重放路径。

**其余全部归插件。** 仓库内置两个，都是你自己 `Install` 的普通包：

| 插件 | 配置段 | 一句话职责 |
|---|---|---|
| `plugins/audit` | `[audit]` | 每次调用交给你自己的 `Sink`——异步、尽力而为，永不阻塞返回 |
| `plugins/spill` | `[spill]` | 超大结果落盘，模型只拿到摘要 + 下载链接 |

审计、配额、二次确认都是**外置插件**（各自独立 module，装法完全一样）——这些策略是部署方的业务判断，不是基座该有的立场。见下面的《外置插件》。

`go list -deps ./runtime/` 里不含任何插件：依赖箭头永远只从插件指向基座。

## 快速开始

需要 Go 1.25+。

**1. 给函数加注解。**

```go
package greeter

// Greet 生成一句问候语。
//
// param: name — 要问候的名字
// param: excited — 是否加感叹号
//
// mcp:tool
// mcp:labels=capability=read,risk=none
func Greet(name string, excited bool) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	msg := "Hello, " + name
	if excited {
		msg += "!"
	}
	return msg, nil
}
```

**2. 在你自己的 module 里建 `mcpgen.yaml`** —— 列出要扫描的包与生成 wrapper 的输出目录（路径相对该文件所在目录）。

```yaml
output:
  dir: ./tools
packages:
  - github.com/you/yourmod/greeter
```

**3. 加一条 `//go:generate` 指令**，放在与 `mcpgen.yaml` 同目录的 Go 文件里（如 `gen.go`），再运行它。

```go
//go:generate go run github.com/fzxbl/mcp-toolify/cmd/mcpgen -config ./mcpgen.yaml
```

```bash
go generate ./...
```

**4. 组装 server：基座 + 你要的插件。**

```go
package main

import (
	"context"
	"log"

	toolify "github.com/fzxbl/mcp-toolify"
	"github.com/fzxbl/mcp-toolify/plugins/spill"
	"github.com/fzxbl/mcp-toolify/runtime"
	"github.com/you/yourmod/tools" // 生成物
)

func main() {
	r := toolify.New(toolify.Config{Addr: ":8011", ConfigPath: "./conf/mcp.toml"},
		tools.RegisterAll)
	// 安装顺序就是洋葱顺序（先装的在最外层）。
	// 外置插件（二次确认、配额）排在 audit 与 spill 之间，理由见《插件顺序》。
	for _, install := range []func(*runtime.Registry) error{
		spill.Install,
	} {
		if err := install(r); err != nil {
			log.Fatal(err)
		}
	}
	if err := r.Start(context.Background()); err != nil {
		log.Fatal(err)
	}
}
```

可直接运行的端到端样例见 [`example/`](./example)：装配在 [`example/cmd/server/main.go`](./example/cmd/server/main.go)，它真正加载的配置在 [`example/conf/mcp.toml`](./example/conf/mcp.toml)。

```bash
go generate ./example/...   # 生成物不入库
go run ./example/cmd/server -addr :8011 -config ./example/conf/mcp.toml

curl -sS localhost:8011 -H 'Authorization: Bearer replace-me-readonly' \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call",
       "params":{"name":"greeter.greet","arguments":{"name":"world","excited":true}}}'
```

启动日志末尾会打出生效链与每个 token 的规则原文，例如
`[mcp] plugin chain (outer→inner): [spill]`。调 `greeter.shout`
（labels 是 `capability=write,risk=high`）需要 ops token。
示例会把落盘结果写到固定的 `/tmp/mcp-toolify-example/spill`：
共享机器上请把 `[spill] dir` 改成你自己有权的目录——落盘目录不属于本进程时 spill 会
直接拒绝启动。

用 `go run ./cmd/listtools` 可以查看已暴露的工具（名称 + 描述 + schema）；
`-short` 是紧凑输出（每行一个工具、不打印 schema），`-json` 输出含 schema 的完整 JSON。

## 注解标记

全部写在函数的 godoc 注释里：

- `mcp:tool` —— 暴露该函数（必填）。
- `mcp:name=<n>` —— 覆盖工具名（默认 `<包名>.<snake_case 函数名>`）。
- `mcp:labels=k=v,k2=v2` —— **不透明** labels（如 `capability=write,risk=high`），用于启动期过滤（`Config.Match`）、token 鉴权与插件规则。纯标记写成 `k=true`。基座不给任何 key 赋予含义，`capability` / `risk` 是什么由你的配置决定。
- `mcp:bind=<param>:<Type>` —— 把无法由 JSON 构造的接口参数绑定到具体入参类型。
- `mcp:import=<path>` —— `mcp:bind` 类型若在源包之外，用它引入所在包的 import path。

`param: <名字> — <说明>` 行会成为对应参数的 JSON schema 描述。

未打标的工具不是「中性默认」：配上推荐的 `deny = ["!risk", "!capability"]` 之后它既不可见也不可执行。这正是「有人加了工具却忘了打标」应有的失败形态。

## label selector 语法

一套引擎（`selector/`）同时服务 token 的 `allow`/`deny`、注册期的 `Config.Match`，以及外置插件自己暴露的 selector（配额规则、二次确认规则）。语法即 k8s label selector，判据是工具的 labels **外加基座投影的两个键**：`name`（完整工具名）与 `pkg`（源码包名）。

| 操作符 | 例子 | 命中条件 |
|---|---|---|
| `=` | `capability=write` | key 存在且相等 |
| `!=` | `risk!=high` | key **缺失** 或 值不同 |
| `in (…)` | `risk in (low,medium)` | key 存在且值在列表里 |
| `notin (…)` | `risk notin (high)` | key **缺失** 或 值不在列表里 |
| `key` | `risk` | key 存在（任意值） |
| `!key` | `!risk` | key **缺失** |

- **同一个 selector 内的逗号是 AND**：`capability=write,risk in (medium,high)`。
- **一组 selector 之间是 OR**：`allow = ["capability=read", "capability=write,pkg in (greeter)"]`。
- `in` / `notin` 前后**各要一个空格**（写 `risk in (high)`，不接受 `risk in(high)`）——对 k8s 语法的刻意收敛。
- **缺失 key 的语义照搬 k8s**，而这条正是唯一需要记住的坑：`!=` 与 `notin` 对**缺失该 key** 的工具也算匹配成功。所以 `deny = ["!risk", "!capability"]` 不是装饰——没有它，一个未打标的工具会直接从 `risk notin (high)` 底下溜过去。
- 语法错误一律启动失败（基座这边是注册期 `Match` 与 token 规则；插件也应对自己的 selector 这样做），绝不会在运行期退化成「什么都不匹配」。

## 鉴权

token 鉴权**始终启用**：`Config.ConfigPath` 必须指向含至少一条 `[[tokens]]` 的 TOML 文件。没有配置路径、或配置里一个 token 都没有，一律启动失败——框架不会起一个无鉴权的 MCP 端点，也刻意不提供关闭开关。

```toml
identity_headers = ["X-MCP-User"]      # 调用人身份来源请求头（有序，取第一个非空）
# trust_identity_header = true         # 默认关闭：未开启时身份头一律被忽略
# required_plugins = ["spill"]         # 必须在场的插件，缺失即启动失败

[[tokens]]
token = "..."             # 以 Authorization: Bearer <token> 传入
name = "readonly-agent"   # token 的**用途名**，插件按它记接入通道；绝不是 token 值
applicant = "you"         # 申请人，仅留档追溯、不进日志
# identity = "fixed:ops-robot"   # 给该 token 固定 Subject.ID，优先级高于任何请求头
allow = ["capability=read,risk in (none,low)"]
deny  = ["!risk", "!capability", "dangerous=true"]
```

- `deny` 优先于 `allow`；两者都不命中即**拒绝**（deny-by-default）。
- 同一套 selector 同时决定 `tools/list` 可见性与 `tools/call` 可执行性，「看不见」与「不能执行」不会走偏。命中的规则在**进入插件链之前**一次性确定并锁进 ctx，插件改写 `Subject` 也放大不了自己的可见范围。
- **调用人身份默认不被信任。** 请求头里的名字是调用方自己填的，因此除非显式打开 `trust_identity_header = true`（仅当前置可信网关完成人的认证并**覆写**这些头时才成立），`identity_headers` 一律被忽略。服务号/机器人请用 `identity = "fixed:<id>"` 把身份绑定到 token。其余情况 `Subject.ID` 恒为空，而按人判定的插件（配额、二次确认）应当**拒绝**该调用，不该把空串当成一个用户。
- 按人的授权（「这个人能不能执行高危工具」）**不在基座**，见[自己写插件](#自己写插件)。
- 缺 `token`/`name`/`applicant`、token 或 name 重复、selector 语法错误、`identity` 不是 `fixed:<id>` 形式，都会启动失败。

`config.example.toml` 是本文所有配置键的带注释 schema。它里面的插件段落**刻意保持注释状态**：这份文件同时会被「不装任何插件的基座」加载，而无人认领的段落会让启动失败（见下）。想看一份真的会连插件一起加载的配置，见 `example/conf/mcp.toml`。

## 以 HTTP 运行 / 挂载到既有 server

`toolify.New(cfg, tools.RegisterAll)` 返回 `*Registry`，`r.Start(ctx)` 即独立 HTTP server。要挂到你已有的 server 上：

```go
r := toolify.New(cfg, tools.RegisterAll)
// spill.Install(r) / yourplugin.Install(r) ... 注册顺序即中间件链顺序
mcpH, routes, err := r.Handlers()
mux.Handle("/mcp", mcpH)         // MCP 协议端点（Streamable HTTP）
for pattern, h := range routes { // 插件自注册的 HTTP 路由
	mux.Handle(pattern, h)
}
```

**每个副本要把 MCP handler 与插件路由挂在同一套前缀上。** 属主路由反向代理时
**保留原路径与原方法**，因此副本之间对「东西挂在哪」不一致，会让跨副本的插件
回调与 spill 转发**静默失效**（转发过去落到 404）。单副本部署可以随意挂。

`Handlers()` 是幂等的——重复调用返回同一个 handler（失败也返回同一个 error），插件中间件不会被装两遍。插件用 `r.Route(...)` 注册的路由取回来时**已经包好了 token 认证**；确实需要免鉴权访问的路由必须用 `r.RoutePublic(...)` 显式表态。自行挂载时记得在退出前调用 `r.RunStop(ctx)`，插件才能关文件、连接池与回收协程。`RunStop` 是幂等的，而且**启动失败时基座会自己跑一次**（配置认领检查、build 期钩子、`net.Listen` 失败都算）：那时每个插件的 `Install` 都已经起过回收协程与连接池，而「让每个调用方在每条失败路径上记得清一次」是一条必漏的约定。
清理一旦跑过，这个 `Registry` 就报废了：之后 `Handlers()` 与 `Start()` 一律拒绝。重试（比如 `net.Listen` 失败后换个端口）必须新建一个 `Registry`——复用已清理的那个会起一个降级服务：连接池已关、回收协程已停，只读工具照常返回结果，而受插件管辖的高危工具永久被拒、落盘文件再也不被回收。

## 插件配置

插件用 `r.Config(&myCfg)` 读自己的 TOML 段落，解码时认领到的键会被记录下来；文件里**没人认领**的键一律启动失败。正是这项检查把 `[qouta]`、`on_eror = "deny"` 从「静默用默认值」变成「启动报错」。但它成立的前提是插件守住两条契约——`toml.MetaData.Undecoded()` 只能看出「哪些键没被解码」，看不出「解到哪去了」：

- **不要用 map 兜底解码自己的段落。** `map[string]any` / `map[string]string` 会让段内**所有**键都被判为「已解码」，于是 `[yourplugin] bakcend=... limmit=...` 被整段吞掉，`Handlers()` 照常返回 nil error，插件按默认值上线。请用具名字段的结构体。
- **文档承诺支持的每个字段都要在结构体里声明。** 只声明一部分时，部署方照文档把配置写全反而启动失败。结构体的字段集就是该段落的对外契约，不能比文档窄。

相关且有意如此：配置里留着某插件的段落、而本次部署没有安装该插件，**同样**启动失败。在基座看来「写了 `[spill]` 却没装 spill」与「段落名拼错」是同一件事——两者都意味着部署方以为生效的东西并没有生效。因此配置与装配必须一一对应；示例的冒烟测试就断言这一点。

## 插件顺序

`r.Use` 是唯一注入点，安装顺序即洋葱顺序（先装的在最外层）。本仓库自带 audit 与 spill，常见外置插件各自该占的位置不是口味问题：

```
audit ▸ (阻塞式二次确认) ▸ quota ▸ spill
```

- **audit 最外** —— 被任何内层插件拒掉的调用也要留下审计记录。
- **阻塞式的二次确认插件在 quota 之外** —— 等人期间一个工具都没执行，那就不该已经占掉额度。额度只在「被批准后真正执行」时扣一次。
- **也在 spill 之外** —— 还在等确认的调用根本没有结果可落盘。
- **spill 最内** —— 它必须看到未经改写的原始结果才能按真实大小判定。

## 外置插件

配额、二次确认是在本仓库里写出来、然后**刻意移走**的（审计也移走过，后来确认它除了 `runtime` 什么都不依赖，才搬回来）。两个理由：

- 它们的策略是部署方的业务判断，不是基座的立场：一个人一天能做几次高危操作、谁有权批准什么。基座对这些不该有意见。
- 这样才守得住「**每个插件都必须能作为外部 module 装进来**」这条规矩。三个插件都真的从独立 module 里构建并跑过完整测试，只靠一条 `replace`——这才证明基座导出的表面足够用，也证明没有给任何内置插件开特例（搬回来的 audit 同样不占任何特例，随时能按同样方式再移出去）。

外置插件可以依赖的东西——完整清单，也正是这三个插件实际用到的那些：

- `r.Use` —— 插件链（唯一注入点，安装顺序即洋葱顺序）。
- `r.Config` —— 自己的 TOML 段落，与内置插件一样参与认领制（见《插件配置》）。
- `r.Route` / `r.RoutePublic` —— 自己的 HTTP 端点；`Route` 由基座统一套 token 认证。
- `r.OnBuild` / `r.OnStop` —— 启动期自检，以及协程/连接回收。
- `r.Named` —— `required_plugins` 校验的那个名字。
- `runtime.RegisterOwnerRouted` / `RegisterOwnerRoutedPath` / `NewOwnedID` —— 任何跨副本的有状态资源。
- `Call.Meta` —— 插件之间的通道；外层插件（通常就是审计插件）会**通用地**把整张表取走，这也是一个插件把自己的上下文送进审计流、而审计插件完全不认识它的唯一方式。

### 阻塞在进程之外的事情上

二次确认这类插件会**阻塞**调用协程，直到人给出答复。这是被支持的用法，而下面五条是它安全的前提。它们都是**链本身**的性质，所以对任何这种形状的插件都成立：

1. **在被允许之前不要调 `next`。** 安全不变量是**位置性**的：拒绝、超时、客户端断开、推送失败、进程重启——每条失败路径都必须发生在 `next` **之前**，于是「没批准」永远等于「没执行」。不存在重放路径，也没有「已确认」标记可以被伪造：能执行的只有调用方那次仍在飞的原始请求。
2. **等待必须有上限，且显著小于 client 的单次调用超时。** 这样超时由服务端判定，模型读到的是一句明确的「未执行，还要就重发」，而不是一次连接被断。
3. **挂起量要有全局上限，也要有按人上限。** 一个被阻塞的请求会占住一个协程直到等待结束；没有按人上限，一个陷入重试循环的调用方就能把其他人挡在所有受管辖工具之外。
4. **重试要幂等。** 同一个人、同一个工具、同一份参数应当并入已有的挂起单，而不是再问一次；agent 的自动重试不能把一次操作变成一堆审批。
5. **`Subject.ID` 为空时在登记任何东西之前就拒掉。** 对匿名调用方来说「同一个人」没有意义——身份来源见《鉴权》。

两条必须说清的限制，因为它们都关于基座：

- 用 `r.Route` 注册的插件路由由基座做的是 **token 认证，不是按人授权**。如果决定的 body 里带着审批人名字，那么「知道一个挂起 id + 持有一个合法 token」就足以替别人回答。请把推送服务当成可信路径的一部分。
- **挂起状态只在某一个副本的内存里。** 请配好 `PublicBaseURL` 与 peer 白名单，并用 `RegisterOwnerRoutedPath` 注册回调前缀，基座才会把回调反代回持有等待者的那个副本（见《跨副本的有状态工具》）。进程重启会让所有在等的调用失败——方向是安全的，但那是真实的用户可见失败，值得报警。

## 审计（plugins/audit）

> 完整字段、事件字段、`Sink` 契约与可观测计数见
> [`plugins/audit/README.md`](plugins/audit/README.md)。

```toml
[audit]
headers = ["X-Tenant", "User-Agent"]  # 凭据类头一律启动失败
queue_size = 4096                     # 填 0 = 取默认值；没有「无界队列」的写法
flush_timeout = "3s"                  # 退出时排空队列的预算，直接加在停止耗时上
max_args_bytes = 1024                 # 填 0 = 取默认值；截断无法关闭
max_result_bytes = 2048
```

```go
audit.OnEvent(func(e audit.Event) error { return myBackend.Write(e) })
```

- **投递是异步、尽力而为的。** 中间件在返回路径上只入队，后台单 worker 把事件交给你的 `Sink`；**永不阻塞调用、永不改变返回值**——审计后端再慢也不会把它的 P99 加到每一次 MCP 调用上。
- **刻意没有 fail-closed 档。** 审计判定在**返回路径**上、工具此刻已经执行完，拒绝返回挡不住任何副作用；真正需要 fail-closed 的是执行**前**的门（按人白名单、二次确认、配额）。代价写在插件 README 里而不是藏起来：「每次调用必有记录」降级为尽力而为，队列打满或 `kill -9` 都会丢事件。
- **丢弃绝不静默**：计数 + 限流告警（每类每分钟最多一条，带「被压制多少条 / 累计多少条」）+ `audit.ReadStats()` 供宿主接监控。
- **开始接流之前至少注册一个 `Sink`**，否则启动失败——「插件装了却一条也不落」正是 `required_plugins` 看不见的那种静默失效。
- 两处覆盖不到，都关于基座：token 准入的拒绝根本到不了插件（那一层在整条链之前），`tools/list` 事件记的是**过滤前**的全量清单。做「谁看见 / 试过哪些工具」的审计时，要把基座那两条限流日志（`准入拒绝` / `tools/list 过滤`）一并采集。

## 大结果落盘（plugins/spill）

> 完整字段、默认值、宿主 API 与 `spill_explore` 的操作清单见
> [`plugins/spill/README.md`](plugins/spill/README.md)。

```toml
[spill]
dir = "/var/tmp/mcp-toolify/spill"
threshold_bytes = 65536      # **序列化后**字节；填 0 表示「取默认值」，没有「关闭」这一档
ttl = "30m"
gc_interval = "5m"
preview_bytes = 2048
max_file_mib = 64            # 磁盘上限单位是 MiB（不是字节）
max_total_mib = 512
on_error = "deny"
```

- 按整份结果的**序列化后**字节判定——文本、图片、音频、嵌入资源、资源链接与 `structuredContent` 都算，因为 JSON 转义与 base64 会让 payload 膨胀；错误态结果（`isError`）同样落盘。超阈值即写盘并把结果改写成摘要 + 下载链接。
- `/spill/<id>` 用 `r.Route` 注册，由基座套 token 鉴权（它提供的是工具结果原文），插件还额外做**属主校验**（落盘时记下的 token 用途名，属主有身份时再比对 `Subject.ID`）。非属主一律裸 404——不是 403，也不回显属主地址。
- 落盘目录启动时收紧为 0700 并校验，随后以句柄形式持有（`os.OpenRoot`），启动之后把该路径换成软链也无法把读写重定向到别处。另有一条**非致命**的周期性对账告警：目录身份变了就告警——**不要为此重启服务**，重启才会让新句柄落到被换上去的路径。
- 摘要里的绝对 URL 取自 `PublicBaseURL`，它必须是**本副本可直连的地址**；配成负载均衡入口会让下载落到非属主副本。不配它对 spill 只是可接受的降级（id 退回随机形态，下载落到别的副本就 404），阻塞式二次确认则不同：回调到不了发起副本，那个操作就永远无法被批准。
- 进程退出只停回收协程、不删文件（正在下载的结果不该因一次重启消失），残留文件由下次启动的首轮扫描清掉。

### 在自己的代码里用这个存储

同一套存储是导出的：**明知**输出会很大的工具（导出 CSV、扫日志）可以自己写进去，而不是先返回一个巨大结果再让中间件截断：

```go
id, err := spill.Put("query.csv", spill.FormatText, csvBytes)  // 整块内容已在内存里
w, err := spill.Create("scan.jsonl", spill.FormatJSONL)        // 流式写入，w.ID() 取 id
id, path, err := spill.CreatePath(spill.FormatText)            // 只接受文件名的第三方写入方
url := spill.URLFor(id)                                        // 给模型的绝对下载地址
spill.SetDefaultDir(dir)                                       // [spill] 没配 dir 时的宿主默认值
```

- 返回 `spill.URLFor(id)` + 一小段预览即可：模型用 `spill_explore` 工具按需取片段（`stat` / 按行 `read` / `grep` / `schema`，`json`/`jsonl` 还能用 `jq` 过滤），人则从 `/spill/<id>` 下载整份。
- `Put`/`Create` 写进去的内容与落盘结果一样是**按属主隔离**的。`CreatePath` 是给「只接受路径」的写入方（日志库、外部命令）留的出口：写入不再由插件控制，因此内容标记为**共享**（任何通过 token 认证的调用方都能下载），单文件字节上限也不生效——**请自行约束写入量**。写入方在旁边生成的兄弟文件（`<id>.ext-text.wf`）会被识别为同一个 id，TTL/配额不会漏算。
- 插件没装时它们统一返回 `spill.ErrNotInstalled`：让「宿主忘装 spill」变成启动期/首次调用就暴露的错误，而不是静默丢数据。

## 自己写插件

一个插件就是带 `func Install(r *runtime.Registry) error` 的普通包。下面是「按人的授权」——**刻意不内置**，因为数据源（值班表、邮件组、审批系统）随组织而变：

```go
// 按人的授权：数据源可以是值班表、邮件组或外部审批系统。
//
// 注意：判定抽成 Allowed 而不是内联在中间件里，是为了你自己的审批/通知流程也能复用它。
// 位置对「判据会随时间变的策略」（值班、封网窗口）很关键：把本插件装在二次确认插件
// **之内**，它就在「被批准的调用真正往下走」那一刻求值——那正是这些策略关心的时刻；
// 装在二次确认之外则是「登记时在值班、批准落到下班后」照样执行。
func Allowed(id string) error {
	if id == "" {
		return fmt.Errorf("%s", "未解析到调用人身份，无法按人判定")
	}
	if !onDuty(id) {
		return fmt.Errorf("%s 当前不在值班，无权执行该操作", id)
	}
	return nil
}

func Install(r *runtime.Registry) error {
	r.Named("identity-authz")
	sel, err := selector.Parse("capability=write,risk in (low,medium,high)")
	if err != nil {
		return err
	}
	r.Use(func(next runtime.Handler) runtime.Handler {
		return func(ctx context.Context, c *runtime.Call) (*runtime.Result, error) {
			if c.Method != "tools/call" || !sel.Match(c.Labels) {
				return next(ctx, c)
			}
			var id string
			if c.Subject != nil {
				id = c.Subject.ID
			}
			if err := Allowed(id); err != nil {
				return runtime.DenyResult(c, "identity-authz", err.Error()), nil
			}
			return next(ctx, c)
		}
	})
	return nil
}
```

### 插件契约

下面每一条都由基座强制或依赖。四个内置插件是参考实现。

**Registry 提供的能力**

- `r.Use(mw)` 是**唯一**注入点；注册顺序即洋葱顺序。
- `r.Named(name)` 声明插件名，供 `required_plugins` 校验与启动日志。
- `r.Config(&cfg)` 是读配置的**唯一**正当方式，两条契约见[插件配置](#插件配置)（禁止用 map 解码、文档承诺的字段都要声明）。
- `r.OnBuild(fn)` 在基座自身校验全部通过之后、开始接流之前执行一次启动期自检，返回 error 即启动失败；**panic 同样是启动失败**（会被 recover 转成 error——否则 `sync.Once` 会把「无 handler 也无 error」缓存下来，宿主挂上去就是一个 nil handler）。「宿主有没有注册我需要的回调」这类检查要放这里而不是 `Install`：在 `Install` **之后**、接流之前注册回调是合法用法。**运行期兜底不要撤**：`OnBuild` 只覆盖「启动之前就能看出来」的那一半，接流之后把回调置回 nil 只有兜底管得住。凡是要宿主注册回调的插件（审计的 sink、二次确认的 notifier）都该做成双层。钩子里**只能做校验与日志**：在钩子里调 `Use` / `Tool` / `Route` / `RoutePublic` / `Named` / `Config` / `OnBuild` 一律**启动失败**——此刻它们的表现全是静默的：链还没装（钩子里 `r.Use` 会真生效，却不会出现在**已经打印过**的 `plugin chain` 日志里），而 `required_plugins` 校验与配置认领检查都已跑完。`OnStop` 不在此列：清理钩子在 `RunStop` 时才用，钩子里登记是真生效的。
- `r.Route(pattern, h)` 注册需鉴权的路由，`r.RoutePublic(pattern, h)` 注册确实要免鉴权的路由。分成两个方法而不是加一个 bool：漏写 public 只会多一层鉴权，漏写 bool 却会少一层。
- `r.OnStop(fn)` 关文件、连接池与回收协程。要**先登记再起协程**，这样即便后面某个插件 `Install` 失败、宿主中止启动，协程仍是能被收走的。
- `r.Tool(add)` 注册插件自带的 MCP 工具。这类工具必须同时登记 labels（`runtime.RegisterTool` / `toolify.RegisterToolMeta`）——未登记 labels 的工具按设计既不可见也不可执行，漏了这一步的表现就是「我的工具凭空消失了」。

**`Call` 上会咬人的几点**

- `Subject.ID` 默认为空（除非 `trust_identity_header = true` 或 token 绑了固定身份）。按人判定的插件必须**显式**处理空身份，不能把 `""` 当成一个用户。
- `Call.Headers` 已剔除 `Authorization` / `Proxy-Authorization` / `Cookie` / `Set-Cookie`，不要绕过它去 `*http.Request` 上取凭据。
- **`Call.Tool` 与 `Call.Args` 可写，而且改了真生效**：执行终点会把它们回写进合成请求。因此**凡是按工具名做的判定都必须在改写之后再判一次**。当前已知一处：token 准入判据（进入链之前就按 token 解析好，改写换不掉）。新增任何按工具名分流的插件时，先自问「改写后还成立吗」。
- `Call.Meta` 是插件间通信通道，基座只认识 `denied_by` / `deny_reason`（由 `DenyResult` 写入）。私有 key 请加「插件名.」前缀。外层的审计插件应当**通用地**把整张表捎进审计事件——这也是插件把自己的上下文送进审计流、而审计插件完全不必认识它的方式。

**阻塞在进程之外的东西上**

- 中间件可以阻塞（等人、等审批系统、等锁），前提是阻塞发生在调用 `next` **之前**：那才是「没跑完 = 没执行」成立的原因。不要先执行再回滚。
- 用自己的定时器给等待设上限，**同时**响应 `ctx.Done()`；上限要显著小于 MCP 客户端的单次调用超时，让服务端给出明确结论而不是一次断连。
- 并发等待者要有**全局**上限，也要有**按人**上限：每个等待者都是一个挂住的协程加一份状态。没有按人上限时，一个陷入重试循环的调用方就能让所有人用不了这个工具。
- 等待要按 `(Subject.ID, 工具名, 规范化 args)` 幂等：Agent 会重试，一次动作不能变成一队待批。
- 内存里的等待者活不过重启。让它们失败（安全的方向）并告警，不要假装还挂着。
- 《外置插件》里描述的那个二次确认插件是以上五条的参考实现。

**血的教训（纪律）**

- 测试注入点用「未导出结构体字段 + nil 表示生产」，**禁止包级可变 var**（全局状态、不能 `t.Parallel()`、漏恢复会污染其它用例）。唯一例外是**给使用方**的注册钩子（本仓的 `spill.SetDefaultDir`，外置插件里的审计 sink、二次确认 notifier），它必须能在任何插件实例存在之前调用。
- 契约里写了「不得改写传入数据」的回调，要传**防御性拷贝**。实测：脱敏钩子就地改写会顺着 `Call.Args` 与 SDK `Arguments` 共享的底层数组污染真实请求字节。
- 请求路径上的告警要「沿触发 + 周期汇总」限流；「必然打日志」不能变成「逐请求一条」（实测：0.35s 的关闭窗口打出 9598 条）。
- 生效策略要打进启动日志。那是部署方唯一能确认「我配的东西生效了」的地方。
- 凡是「按外部传入的 id 读本地文件」，`os.OpenRoot` **加上** `Lstat` + `IsRegular` + `O_NOFOLLOW` 三道一起用：`os.Root` 只保证「不逃出根」，不保证「不跟随根内的相对软链」。少了 `Lstat`，目录里的一个 FIFO 就能让请求协程永久阻塞。
- `runtime.ResetOwnerRoutedForTest` 与 `runtime.ResetOwnerRoutedPathsForTest` **仅测试使用**。运行期调用任一个都会清空进程级注册表，让所有属主路由与回执路由集体静默失效。它们被导出只是因为 `runtime` 之外的插件在测试里需要它们。

### 任何插件都必须能被外置

仓库内的插件是*参考实现*，不是特权实现。规则是：**插件的生产代码只准 import `mcp-toolify/runtime` 与 `mcp-toolify/selector`** —— 不许碰任何内部路径，也不许依赖外部作者拿不到的基座配合。

理由是外部插件作者的能力上限恰好等于基座导出的 API 面。内置插件一旦伸手去拿别的东西，基座就悄悄长出了一个特例，而「这个你自己也能写」这句话在没人察觉的情况下变成了假话。因此插件每需要一项新能力，就把这项能力以**通用**形态加进基座——这正是「属主路由收一个路径提取函数而不是认识某个具体回调前缀」、「`Call.Meta` 作为不透明表通用透传而不是加一个『批准人』字段」的原因。

这条线由机器守着：`TestPluginsOnlyDependOnPublicPackages`（`runtime/externalizable_test.go`）对 `plugins/...` 跑一次 `go list`，出现任何其它仓内 import 即失败。它刻意不看测试 import——跨插件的**测试**是合法的组合验证。

审计、配额、二次确认三个插件就是这条规则作用在自己身上的结果：先在仓内写好，再移出去、以各自的 module 装回来（audit 后来搬回仓内，正是因为这一遍证明了它除 `runtime` 之外什么都不依赖——搬回来也不占任何特例，随时能按同样方式再移出去）。真做这一遍，才让「外置插件也能做到这些」从一句声明变成事实——也正是它让基座以**通用**形态长出了 `RegisterOwnerRoutedPath` 与不透明的 `Call.Meta`，而不是去认识「回执」或「审计记录」是什么。

## 请求 ID（logid）

```toml
[log]
logid_header = "X-Log-Id"   # 读取入站 logid 的头名；省略 => "X-Log-Id"
```

每个 HTTP 请求都带一个 logid：优先取该头、缺失或**不合规**则自动生成，注入请求 ctx、回写同名响应头，并以 `Call.LogID` 暴露给插件。合规的定义是「1-64 位 `[A-Za-z0-9._:-]`」——它来自调用方，而框架与审计的日志行是无引号的 `key=value` 形状，不校验就等于允许往日志里注入伪造字段（终审实测：`X-Log-Id: fake logid=deadbeef actor=admin` 会被整串采信）。不合规的值会被丢弃并自生成，同时打一条限流告警。

框架不自带接入层 access 日志：宿主 HTTP server 本来就有一份，审计插件按同一个 logid 与它串联即可。**但 logid 不做去重**：调用方每次发同一个值时，审计事件与 access log 就没法一对一，需要严格一对一请在网关侧保证唯一。

## 跨副本的有状态工具

有些工具会产出一个只物理存在于「创建它的那个副本」上的资源：落在本地磁盘的大结果、一个交互式会话、一个长时任务或探针。负载均衡部署下，后续调用（读它、轮询它、取消它）可能落到*别的*副本而未命中。基座用通用方式解决——无需共享存储，无需注册任何逻辑：

- **把属主编码进 id**：用 `NewOwnedID()`（取自 `Config.PublicBaseURL`）。未设置对外地址时 id 保持旧式随机形态，不做任何路由。
- **声明 id 出现在哪**，在 init 时注册一次。有两种形态，因为后续请求既可能是工具调用、也可能是普通 HTTP 请求：

  ```go
  // (a) tools/call 的入参
  runtime.RegisterOwnerRouted("your.get_status", "job_id")
  runtime.RegisterOwnerRouted("your.cancel",     "job_id")

  // (b) 插件用 r.Route 注册的 HTTP 路由 —— 属主在路径里
  runtime.RegisterOwnerRoutedPath("/confirm/", func(r *http.Request) string {
      return strings.TrimPrefix(r.URL.Path, "/confirm/")
  })
  ```

- **其余交给基座。** `WithOwnerRouting` 包住 MCP 端点（形态 (a) 加 (b)）；`Registry.Routes()` 返回的每条插件路由都被 `WithPathOwnerRouting` 包住（只做形态 (b)，不缓冲 body）。若取出的 id 属于*远端*白名单兄弟副本，就把请求反向代理到该属主并**保留原路径与原方法**（所以 `POST /confirm/x` 与 `GET /spill/x` 都能用），结果流式回传；否则透传给本地 handler。防环 header 把转发限制为单跳，被转发的请求会在属主副本上重新鉴权，因此路由不带来任何额外权限。

转发目标被约束在实时的兄弟副本白名单内——`Config.Peers` 给静态快照，或对接你自己的服务发现：

```go
toolify.SetPeerProvider(func() []string { return currentReplicaHostPorts() })
toolify.SetPeers([]string{"replica-a:8011", "replica-b:8011"})
```

白名单为空（且未注册 provider）则拒绝一切远端转发，防止 SSRF：指向不在白名单内的远端属主的调用会在本地服务（并直接未命中），而不会被代理到任何地方。

**部署前提，不是细节**：副本间转发走明文 `http://`，并把调用方的 `Bearer` token **原样透传**（`runtime/owner_routing.go`，`plugins/spill` 里是同一形状）。内网能抓包的人就能拿到该 token——而装了二次确认插件之后，这个 token 正是「批准高危操作」的凭据。**因此 peer 只能部署在同一受信网络内；跨机部署时请在 peer 之间加 TLS**，或用副本间的内部凭据替代透传调用方 token。

## 目录结构

- `toolify.go` —— 对外入口：`New`、`Config`、`Registry`、`RegisterToolMeta`、`WithOwnerRouting`、`RegisterOwnerRouted`、`RegisterOwnerRoutedPath`、`NewOwnedID`、`OwnerOf`、`SetPeers`、`SetPeerProvider`。
- `runtime/` —— 基座：MCP/HTTP 装配、工具注册表、token 鉴权与准入、中间件链、执行终点、logid、属主路由、`Registry`。
- `selector/` —— k8s 风格 label selector 引擎。
- `plugins/audit` —— 内置插件：异步尽力而为地把审计事件交给宿主 `Sink`。
- `plugins/spill` —— 内置插件：大结果落盘（配额 / 二次确认各自独立成 module）。
- `cmd/mcpgen/` —— 代码生成器（`go run github.com/fzxbl/mcp-toolify/cmd/mcpgen`）。
- `cmd/listtools/` —— 打印已暴露工具与 schema 的调试小工具。
- `example/` —— 可运行的端到端样例：`cmd/server`（装配）、`greeter`（带注解的工具）、`conf/mcp.toml`（它加载的配置）。
- `config.example.toml` —— 所有配置键的带注释 schema。

生成的 `*_gen.go` **不入库**（`/example/tools/` 已 gitignore）：新克隆的仓库在构建或测试之前先跑 `go generate ./example/...`。

## 安全

- **认证与准入。** 每个请求都必须带上已配置的 `Authorization: Bearer <token>`，否则在进入 MCP 层之前就被 401；未登记 labels 的工具既不可见也不可执行（deny-by-default）。插件路由默认同样需要鉴权，除非用 `RoutePublic` 显式声明。`Call.Headers` 已剔除凭据类头，**误用**（顺手把请求头转发出去）因此拿不到 token。但这不是对抗恶意插件的保证：插件只用导出 API（`SetPeers` + 自造带归属前缀的 id）就能诱导属主路由把整条请求连 `Authorization` 明文反代到指定 host——见下面的信任边界。
- **身份。** 客户端身份头默认不被信任；仅当前置网关会覆写这些头时才打开 `trust_identity_header`，或给 token 绑定 `identity = "fixed:<id>"`。
- **信任边界：插件在边界之内，不是被沙箱起来。** `allow` / `deny` 约束的是**调用方（token）**，不是插件。改写 `Call.Tool` 是正式契约的一部分，这意味着插件对「最终执行哪个工具」拥有**完全权威**——实测探针：weak token 调它有权的 `probe.entry`，插件把 `c.Tool` 改成该 token **无权**的 `demo.write`，执行终点照常执行（准入判的是改写前的名字）。这不是缺陷，是设计。基座保证的东西窄得多：准入判据在链跑起来之前就按 token 解析好，链内换不掉。约束插件的唯一手段是代码评审与「不装不可信插件」。
- **启动 fail-closed。** 没有 token、selector 语法错误、缺少必需插件、配置里有无人认领的键、某个插件的 build 期钩子报错——一律拒绝启动。
- **副本间流量是明文 + 透传 Bearer token**，见上面的部署前提。

## 许可

MIT，见 [LICENSE](./LICENSE)。欢迎贡献与 star。
