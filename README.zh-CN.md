# mcp-toolify

[English](README.md) | 简体中文

[![Go Reference](https://pkg.go.dev/badge/github.com/fzxbl/mcp-toolify.svg)](https://pkg.go.dev/github.com/fzxbl/mcp-toolify)
[![Go 1.25+](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![MCP](https://img.shields.io/badge/MCP-Model%20Context%20Protocol-6E56CF)](https://modelcontextprotocol.io)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

`mcp-toolify` 用于把普通 Go 函数转换为 [Model Context Protocol](https://modelcontextprotocol.io) 工具。它由代码生成器和运行时组成：生成器读取函数签名与 godoc，运行时负责通过 Streamable HTTP 对外提供 MCP 服务。项目依赖官方 Go MCP SDK，可独立运行，也可嵌入已有 HTTP 服务。

## 核心能力

- **注解驱动生成**：为函数添加 `// mcp:tool`，运行 `go generate`，即可生成类型化 wrapper、输入结构体、JSON Schema 和注册代码。
- **运行期无反射**：生成的 wrapper 直接调用目标函数，函数签名变化会在编译阶段暴露。
- **统一运行时**：提供 Streamable HTTP、工具注册、token 认证与 selector 准入、logid、owner routing 和中间件链。
- **插件化策略**：审计、大结果落盘及部署方自定义策略通过插件安装，不进入基座。
- **灵活部署**：支持独立启动、嵌入现有服务和多副本部署。

## 工作原理

最小闭环如下：

```text
// mcp:tool
    ↓ go generate
生成 wrapper、输入 struct、JSON Schema、RegisterAll
    ↓
toolify.New(..., tools.RegisterAll)
    ↓
Start(ctx) 或 Handlers()
```

## 快速开始

1. 定义工具函数：

```go
package greeter

// Greet 生成一句问候语。
//
// param: name — 要问候的名字
// param: excited — 是否添加感叹号
//
// mcp:tool
// mcp:labels=capability=read,risk=none
func Greet(name string, excited bool) string {
	msg := "Hello, " + name
	if excited { msg += "!" }
	return msg
}
```

2. 在 module 中创建 `mcpgen.yaml`：

```yaml
output:
  dir: ./tools
packages:
  - github.com/you/yourmod/greeter
```

3. 在同目录的 Go 文件中添加生成指令并运行：

```go
//go:generate go run github.com/fzxbl/mcp-toolify/cmd/mcpgen -config ./mcpgen.yaml
```

```bash
go generate ./...
```

5. 准备 `mcp.toml`。token 认证不能关闭，配置中必须至少包含一个 `[[tokens]]`：

```toml
[[tokens]]
token = "replace-with-a-random-secret"
name = "readonly-agent"
applicant = "your-id"
allow = ["capability=read,risk=none"]
deny = ["!capability", "!risk"]
```

6. 组装服务：

```go
package main

import (
	"context"
	"log"
	toolify "github.com/fzxbl/mcp-toolify"
	"github.com/you/yourmod/tools"
)

func main() {
	r := toolify.New(toolify.Config{Addr: ":8011", ConfigPath: "./mcp.toml"}, tools.RegisterAll)
	if err := r.Start(context.Background()); err != nil { log.Fatal(err) }
}
```

```bash
go generate ./example/...
go run ./example/cmd/server -addr :8011 -config ./example/conf/mcp.toml
```

```bash
curl -sS localhost:8011 \
  -H 'Authorization: Bearer replace-me-readonly' \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"greeter.greet","arguments":{"name":"world","excited":true}}}'
```

## 注解概览

注解写在导出函数的 godoc 中：

- `mcp:tool`：将函数暴露为工具。
- `mcp:name=<name>`：覆盖默认工具名；默认格式为 `<包名>.<函数名 snake_case>`。
- `mcp:labels=k=v,k2=v2`：声明不透明 labels，供注册过滤、token 准入和插件使用。
- `mcp:bind=<param>:<Type>`：将接口参数绑定到可由 JSON 构造的具体类型。
- `mcp:import=<path>`：引入 `mcp:bind` 使用的外部类型。
- `param: <name> — <description>`：生成参数的 JSON Schema 描述。

labels 是不透明键值，基座不解释自定义 key。`name` 和 `pkg` 是基座投影的内置 label，匹配时会覆盖工具声明的同名值，因此不应自行设置。

selector 采用 k8s 风格语义：单条 selector 内的条件是 AND，多条 selector 之间是 OR；`!=` 与 `notin` 对缺失 key 也会匹配；空 selector 匹配全部。完整生成规则见[代码生成文档](docs/code-generation.zh-CN.md)。

## 认证与工具准入

token 认证始终开启。`Config.ConfigPath` 必填，指向的 TOML 文件必须包含至少一个 `[[tokens]]`；缺少配置或 token 时服务拒绝启动。

每个 token 通过 `allow` 和 `deny` selector 控制工具访问：

- `deny` 优先于 `allow`。
- 两者都不匹配时拒绝。
- `tools/list` 与 `tools/call` 使用同一规则，工具可见性与可执行性保持一致。
- HTTP 请求使用 `Authorization: Bearer <token>`。

调用方提供的身份头默认不受信任。仅在可信网关已经认证用户并覆盖这些请求头时，才应启用 `trust_identity_header`。机器人或服务账号应使用 `identity = "fixed:<id>"` 将身份绑定到 token。

## 插件机制

插件是提供以下入口的普通 Go 包：

```go
func Install(r *runtime.Registry) error
```
安装顺序就是中间件的洋葱顺序，先安装的插件位于外层。推荐顺序为：

```text
audit ▸ 策略插件 ▸ spill
```
audit 位于最外层，用于异步、尽力而为地把调用上下文交给宿主注册的 Sink，详见 [audit 文档](plugins/audit/README.zh-CN.md)。spill 位于最内层，将超阈值结果写入本地文件并返回摘要与访问入口，详见 [spill 文档](plugins/spill/README.zh-CN.md)。

部署所需插件应通过 `required_plugins` 声明；缺少任一插件时启动失败。插件位于进程信任边界内，不是沙箱：插件可以改写 `Call.Tool` 和 `Call.Args`，改写内容会影响最终执行，因此只应安装经过审查的插件。开发接口与约束见[插件开发文档](docs/plugin-development.zh-CN.md)。

## 运行方式

独立运行时调用 `r.Start(ctx)`，由 runtime 创建并管理 HTTP server。

嵌入已有服务时调用 `r.Mount(prefix, mountFunc)`：宿主只说一次「挂在哪」，基座据此把 MCP 端点与全部插件路由交回来挂载。

```go
r := toolify.New(cfg, tools.RegisterAll)
defer r.RunStop(context.Background())
if err := r.Mount("/mcp", func(pattern string, h http.Handler) {
	mux.Handle(pattern, h) // 需要通配符的 router 在这里补：pattern+"*"
}); err != nil {
	log.Fatal(err)
}
```

MCP 端点落在 `prefix` 本身，插件路由落在 `prefix + "/plugin"` 之下。同一个前缀还决定插件给 agent 的绝对 URL 与副本间转发的目标路径，所以**不要在 mountFunc 里改写 pattern**（再套 `StripPrefix`、再加一层前缀）——要换挂载点就改 `Mount` 的参数。`Mount` 还会在启动期校验「插件登记的 owner 路由都有对应路由」，不一致直接返回 error。

嵌入模式退出前必须调用 `RunStop`，以释放插件持有的文件、连接和后台协程。需要完全自己组装 handler 时仍可用 `r.Handlers()`（那时没有挂载前缀，等于挂在根上）。更多生命周期与配置说明见[运行时文档](docs/runtime.zh-CN.md)。

## 多副本部署

有状态资源使用 owned ID 编码属主副本。工具调用通过 `RegisterOwnerRouted(tool, param)` 声明承载 ID 的参数；插件 HTTP 路由通过 `RegisterOwnerRoutedRoute(pattern, extractor)` 声明属主提取方式。请求只会转发到 peer 白名单中的副本，并在属主副本上重新执行 token 认证。

所有副本必须使用一致的路由前缀，并正确配置 `SelfAddr`（本副本直连 host:port，即副本身份）与 peer 列表或服务发现；交给外部的链接取 `PublicBaseURL`，它可以是域名/VIP。当前副本间转发使用明文 HTTP，并透传调用方的 Bearer token；跨机部署需要在 peer 之间增加 TLS，或改用内部凭据。详见 [owner routing 文档](docs/owner-routing.zh-CN.md)。

## 文档索引

- [代码生成](docs/code-generation.zh-CN.md)
- [运行时](docs/runtime.zh-CN.md)
- [插件开发](docs/plugin-development.zh-CN.md)
- [Owner routing](docs/owner-routing.zh-CN.md)
- [Audit 插件](plugins/audit/README.zh-CN.md)
- [Spill 插件](plugins/spill/README.zh-CN.md)

## 许可

MIT，见 [LICENSE](./LICENSE)。
