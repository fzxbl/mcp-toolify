# mcp-toolify

[English](README.md) | **简体中文**

[![Go Reference](https://pkg.go.dev/badge/github.com/fzxbl/mcp-toolify.svg)](https://pkg.go.dev/github.com/fzxbl/mcp-toolify)
[![Go 1.25+](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![MCP](https://img.shields.io/badge/MCP-Model%20Context%20Protocol-6E56CF)](https://modelcontextprotocol.io)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Stars](https://img.shields.io/github/stars/fzxbl/mcp-toolify?style=social)](https://github.com/fzxbl/mcp-toolify/stargazers)

**给一个普通 Go 函数加一行注释 `// mcp:tool`，`go generate`，它就成了一个 MCP 工具。**

`mcp-toolify` 是一套用 Go 构建 [Model Context Protocol](https://modelcontextprotocol.io) 服务的**代码生成器 + 运行时**。在普通函数的 godoc 上加 `// mcp:tool`，跑一次 `go generate`，它就变成一个类型完整的 MCP 工具——入参结构体、JSON schema、注册代码全部自动生成。无需手写 wrapper，运行期不用反射。运行时还补齐了真实部署真正需要的能力：**连接级鉴权**、**超大返回值自动落盘**、**调用审计**。

> 写函数、打标记、生成。工具的 schema、描述、参数说明、风险元数据，全部直接来自你已经写好的代码。

Go 编写，官方 MCP SDK，支持 stdio 与 Streamable HTTP。可直接接入 Cursor、Claude、Comate 或任意 MCP 客户端，也可嵌入你已有的宿主服务。

---

## 为什么用 mcp-toolify

把 Go 逻辑暴露成 MCP 工具，通常要为每个函数手写一层 wrapper：入参结构体、JSON schema、参数说明、拆包/打包的 handler，再加注册样板代码。这些代码一旦改动就会和真实函数脱节，而且完全没表达风险、体积、鉴权等信息。mcp-toolify 抓住了五点：

- **1. 注解驱动、零样板——代码本身就是规范。** 在函数 godoc 上加 `// mcp:tool`，独立生成器（`cmd/mcpgen`）就产出类型化 wrapper：入参结构体来自形参，JSON schema 描述来自 `param:` 行，工具描述来自 doc 注释。生成代码直接调用你的函数——**运行期零反射**，工具永远不会和函数签名悄悄脱节。
- **2. 超大返回值自动落盘——上下文窗口永不被撑爆。** 每个工具返回值都会估算体积；超过可配置的 token 阈值时，不再把大 payload 塞进上下文，而是落盘为临时文件并返回一个 MCP 资源链接 + 简短摘要。内置配套工具 `spill_explore`（对 json/jsonl/text 支持 `read` / `grep` / `schema` / `jq`）让 Agent 按需探索、只取需要的那几行。跨机部署还会自动给出一个可直连下载的 URL。
- **3. 连接级鉴权，两层校验。** 每个 token 有读/写**风险上限**，决定这条连接最多能做什么（`tools/list` 也据此过滤）；`tools/call` 再按**调用者身份**匹配分级白名单。一个 Agent 可以用同一条连接服务很多人，每个人仍按其身份受控。工具用 `mcp:risk=low|medium|high` 声明风险、用 `write` 标签声明写意图。
- **4. 独立运行 *或* 嵌入复用——共用一个 server。** 既可作为独立进程以 stdio/HTTP 运行，也可拿到两个 `http.Handler`，**挂载到你已有的 HTTP server 上**，共用端口与生命周期。外部手写工具也能注册到同一个 server，与生成的工具并存（只需登记其风险元数据）。
- **5. 零私有依赖——干净、可移植、可审计。** 只依赖官方 Go MCP SDK、`jsonschema-go`、`BurntSushi/toml`，以及内置落盘工具用到的 `gojq`。生成器是独立 module，仅依赖 `golang.org/x/tools` 与 `yaml.v3`。没有别的东西需要信任。

以及一批让上述能力可靠落地的机制：

- **对棘手类型给出诚实的 schema。** `interface{}` 参数会生成一个显式的半受限 JSON schema（类型联合），而不是 SDK 默认那种无约束空节点，模型仍能拿到类型提示。多返回值会打包成一个稳定的、带字段名的 JSON 对象。
- **接口入参也能处理。** 模型无法用 JSON 构造的接口参数，用 `mcp:bind=param:Type` 绑定到具体类型；`mcp:import=<path>` 引入该类型所在的外部包。完全通用，没有任何框架专属的特判。
- **可插拔审计。** 注入一个 `Logger` 即可把每次调用（用户、工具、入参、结果、耗时）以结构化字段记录；不注入则回退标准库 log。
- **有界、自清理的落盘存储。** 落盘文件存于临时目录，按类别设 TTL，后台 GC 定期回收，不会无限堆积。

---

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
// mcp:tags=read
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

**2. 在 `mcpgen.yaml` 里列出要扫描的包。**

```yaml
output:
  dir: ./tools
packages:
  - github.com/you/yourmod/greeter
  # 可选：暴露内置的大结果探索工具
  - github.com/fzxbl/mcp-toolify/spillexplore
```

**3. 生成 wrapper。**

```go
//go:generate go run github.com/fzxbl/mcp-toolify/cmd/mcpgen -config ./mcpgen.yaml
```

```bash
go generate ./...
```

**4. 启动 server。**

```go
package main

import (
	"context"

	toolify "github.com/fzxbl/mcp-toolify"
	"github.com/you/yourmod/tools" // 生成物
)

func main() {
	_ = toolify.Start(context.Background(),
		toolify.Config{Transport: "stdio"}, tools.RegisterAll)
}
```

可直接运行的端到端样例见 [`example/`](./example)。用下面的命令查看已暴露的工具（名称 + 描述 + schema）：

```bash
go run ./cmd/listtools -short
```

## 注解标记

全部写在函数的 godoc 注释里：

- `mcp:tool` —— 暴露该函数（必填）。
- `mcp:name=<n>` —— 覆盖工具名（默认 `<包名>.<snake_case 函数名>`）。
- `mcp:tags=a,b` —— 启动时按 tag 过滤；`write` 标签标记为写操作，供鉴权使用。
- `mcp:risk=low|medium|high` —— 风险等级（缺省 none），由 HTTP 鉴权强制执行。
- `mcp:bind=<param>:<Type>` —— 把无法由 JSON 构造的接口参数绑定到具体入参类型。
- `mcp:import=<path>` —— `mcp:bind` 类型若在源包之外，用它引入所在包的 import path。

`param: <名字> — <说明>` 行会成为对应参数的 JSON schema 描述。

## 以 HTTP 运行 / 挂载到既有 server

`toolify.Start` 配 `Config{Transport: "http", Addr: ":8080"}` 即独立 HTTP server。要挂到你已有的 server 上：

```go
mcpH, spillH, err := toolify.Handlers(cfg, tools.RegisterAll)
mux.Handle("/mcp", mcpH)      // MCP 协议端点（Streamable HTTP）
mux.Handle("/spill/", spillH) // 大结果下载端点
```

## 鉴权

设 `Config.AuthzEnabled = true`（仅 HTTP 生效），并让 `Config.ConfigPath` 指向一个 TOML 文件：

```toml
[spill]
max_result_tokens = 4000   # 估算超过该值的结果落盘；-1 关闭

[[tokens]]
token = "..."          # Authorization: Bearer <token>
name = "readonly-agent"
applicant = "you"
read = "medium"        # 读操作最高风险；省略表示不允许读
# write = "low"        # 写操作最高风险；省略表示不允许写

[risk_allowlist]
high = ["alice"]       # 允许执行 high 风险工具的 X-MCP-User
medium = ["bob"]
```

- 连接与 `tools/list` 只按 **token** 的上限过滤（这条连接最多能做什么）。
- `tools/call` 再按**调用者**（`X-MCP-User`）匹配 `risk_allowlist` 校验 `medium`/`high` 工具。

## 审计请求头

把 `Config.ConfigPath` 指向同一个 TOML 文件，即可把指定请求头记入审计日志。纯审计用途，与 `AuthzEnabled` 无关：

```toml
[audit]
headers = ["X-Tenant-Id", "X-Client-Id"]
```

列出的每个 header 会成为一个独立审计字段，字段名为 header 名的小写形式（如 `X-Tenant-Id` → `x-tenant-id`）；请求未带该 header 时记为 `-`。省略该段或留空表示不额外记录任何 header。header 值会被直接信任，因此仅应在可信网关之后启用，且不要把凭据类 header（`Authorization`、`Cookie`）列入。

## 请求 ID 与接入层日志

每个 HTTP 请求都带一个 **logid** 用于跨日志串联：

```toml
[log]
logid_header = "X-Log-Id"   # 读取入站 logid 的头名；省略 => "X-Log-Id"
```

`HTTPLogID` 从该头取 logid（缺失则自动生成），注入请求 ctx、回写同名响应头，并作为每条审计记录的 `logid` 字段。用内置独立 server 启动（`Start`/`Run` 走 HTTP）时，还会通过同一个 `Logger` 输出每请求一行的**接入层 access 日志**（`logid`、`method`、`path`、`status`、`cost`、`user`），从而可按 `logid` 在 access 日志与审计日志之间串联同一次请求。把 handler 挂载进你自己的 server（`Handlers`）时不添加 access 日志——但仍会注入并回写 logid，供宿主自己的 access 日志串联。

## 目录结构

- `toolify.go` —— 对外入口：`Start`、`Handlers`、`Config`、`Logger`、`SetAuditLogger`、`RegisterToolMeta`。
- `runtime/` —— server 装配、鉴权、落盘存储、审计日志。
- `spillexplore/` —— 内置的 `spill_explore` 工具。
- `cmd/mcpgen/` —— 代码生成器（独立 module）。
- `cmd/listtools/` —— 打印已暴露工具与 schema 的调试小工具。
- `example/` —— 最小、可运行的端到端样例。

生成的 `*_gen.go` 一般不入库（构建时现场生成）；本仓库仅提交 `example/tools/`，好让样例开箱即构建。

## 安全

以 HTTP 提供服务且**未开启** `AuthzEnabled` 时，MCP 端点是无鉴权的——调用工具即执行你的 Go 代码。请绑定 loopback、置于带鉴权的反向代理之后，或开启鉴权。注意 `X-MCP-User` 会被直接信任，因此**必须由可信网关注入**，绝不能直接接受客户端传入。

## 许可

MIT，见 [LICENSE](./LICENSE)。欢迎贡献与 star。
