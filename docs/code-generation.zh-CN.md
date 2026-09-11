# 代码生成

[English](code-generation.md) | 简体中文

返回[根 README](../README.zh-CN.md)。

本文说明 `cmd/mcpgen` 如何把 Go 函数包装为 MCP 工具。生成器读取 Go AST 和
`go/types` 信息，输出可编译的 wrapper；运行期不依赖函数签名反射。

## 注解语法

注解必须写在函数的 godoc（`//` 注释）中。一个工具至少需要一行
`// mcp:tool`。支持的标记如下：

| 标记 | 语法 | 作用 |
| --- | --- | --- |
| 工具标记 | `mcp:tool` | 选择该函数生成工具。也接受带 `=` 的形式，但值不参与生成。 |
| 工具名 | `mcp:name=<name>` | 覆盖默认工具名。值按原文写入生成代码。 |
| 标签 | `mcp:labels=k=v,k2=v2` | 给工具声明字符串标签。多个同名标记会合并；重复 key、空项、非法 key/value 会报错。 |
| 参数绑定 | `mcp:bind=<param>:<Type>` | 为参数指定生成输入字段的具体 Go 类型，主要用于接口参数。多条标记用多行或 `;` 分隔。 |
| 外部导入 | `mcp:import=<path>` | 把 import path 加入生成文件，供 `mcp:bind` 中的外部类型使用。多条标记或 `;` 分隔。 |
| 参数说明 | `param: <name> — <description>` | 为输入字段生成 `jsonschema` 描述。分隔符也可以是 `-`。 |

`mcp:labels` 的 `k=v` 以第一个 `=` 分隔；key 不能为空且不能包含
`= ! ( ) ,` 或空白，value 不能包含 `= ! ( ) ,`，但允许空格。`mcp:bind`
按第一个 `:` 分隔，裸类型名会解析为源包的 `src.<Type>`；含点号的类型表达式
按原文使用，例如 `auth.User`。绑定只影响生成输入字段类型，不改变原函数签名。

示例：

```go
// Lookup 查询对象。
//
// param: id — 对象 ID
// mcp:tool
// mcp:name=objects.lookup
// mcp:labels=capability=read,risk=none
func Lookup(id string) (Object, error) { /* ... */ }
```

`mcp:tool` 之外的 `mcp:` 标记会被校验。未知标记、空的 `mcp:labels`
都会使生成失败；报错包含源文件和行号。标记名区分大小写。参数说明只匹配函数实际
存在的参数，未写说明不会补默认文本。

## 工具元数据和输入 Schema

默认工具名是 `<包名>.<函数名 snake_case>`，例如 `greeter.Greet` 生成
`greeter.greet`。显式 `mcp:name` 优先。描述来自 godoc：去掉函数名约定前缀后，
第一段作为首句，其余正文按段落整理；普通换行折叠为空格，段落之间使用 ` | `。
描述会写入 `mcp.Tool.Description`。

每个函数生成一个输入结构体：

* Go 参数名首字母大写成为字段名；
* JSON 字段名是参数名的 snake_case；
* `param:` 内容写入 `jsonschema` struct tag；
* 输入字段类型来自 `go/types`，源包以生成文件中的 `src` 别名引用，其他包使用
  包名并自动加入其 import；
* mcpgen 保留原 Go 字段类型；schema 主要由 `jsonschema-go` reflector 生成。当前
  文档只约定以下稳定的类型层次：

  | Go 类型 | JSON Schema 类型 |
  | --- | --- |
  | `string` | `string` |
  | `bool` | `boolean` |
  | 带符号或无符号整数 | `integer` |
  | `float32` / `float64` | `number` |
  | slice / array | `array` |
  | map / struct | `object` |

  `[]byte`、`time.Time`、`json.RawMessage` 等具体类型的 schema 由
  `jsonschema-go` reflector 决定，不作为 mcpgen 自定义契约。生成器不自行添加
  required 或业务约束，其他 schema 细节也以生成结果和 reflector 为准。

`interface{}`（包括递归出现在指针、切片、数组、map 或结构体字段中的空接口）不能
直接由 JSON 构造为具体 Go 值。未绑定时，生成器为输入结构设置
`runtime.AnyInputSchema[T]()`：无类型约束节点显式允许
`object`、`array`、`string`、`number`、`boolean`、`null` 六种 JSON 类型。
这只是 schema 联合，函数仍收到 `interface{}`。若接口实际需要具体结构，应使用：

```go
// Apply 应用补丁。
//
// mcp:tool
// mcp:bind=patch:Patch
func Apply(patch interface{}) error { /* ... */ }
```

这里的 `Patch` 会生成成 `src.Patch`；外部类型则同时声明导入：

```go
// mcp:bind=user:auth.User
// mcp:import=example.com/project/auth
```

`mcp:bind` 不会自动推断外部 import，也不会检查绑定名是否对应某个参数；未匹配的
绑定不会产生输入字段。绑定类型必须能在生成文件中编译。非空接口若未绑定通常不能
由 JSON Schema/反射正确构造，属于不支持的输入，应改为具体类型或显式绑定。

## 返回值、error 和生成文件

wrapper 的 handler 直接调用源函数。返回值规则如下：

* 无返回值：结果为空；
* 一个非 `error` 返回值：包装为 `{"result": <value>}`；
* 只有一个 `error` 返回值：成功结果为空，非 nil error 转为 MCP 工具错误；
* 两个返回值且第二个是 `error`：第一个值包装为 `{"result": <value>}`；
* 多个返回值：包装为 map。命名返回值使用 snake_case 作为 key；未命名或 `_`
  使用 `result0`、`result1` 等。末位是 `error` 时不放入 map，非 nil error 转为
  MCP 工具错误；
* 两个或更多非 error 返回值也按上述 map 规则处理。

生成的 handler 将业务 `error` 作为 MCP `CallToolResult` 的错误结果，而不是返回
协议级 error。生成代码还包括每个源包的 `Register_<Package>` 和总的
`all_gen.go`；注册时按 `RegisterOptions.Allow` 过滤工具，并把工具 labels 登记
到运行时注册表。

### 限制

生成器只处理带 `mcp:tool` 的函数声明和可被 `go/packages` 加载的 Go 包；包加载、
类型检查、模板格式化失败都会终止生成。它不生成业务校验、不为返回值生成
`outputSchema`，也不把任意 `interface{}` 自动猜成某个具体结构。函数签名、包名或
外部类型改变后应重新生成并编译检查。

## mcpgen.yaml 和生成命令

配置是 YAML，字段只有：

```yaml
output:
  dir: ./tools
packages:
  - example.com/your/module/greeter
```

`packages` 是要扫描的包路径列表；`output.dir` 是输出目录。配置路径由
`-config` 指定，默认是当前工作目录的 `mcpgen.yaml`。输出目录相对于配置文件所在
目录解析，再转为绝对路径；配置文件不存在、YAML 无法解析或任一包加载失败都会
退出。输出目录不存在时自动创建。

通常在包目录放置：

```go
//go:generate go run github.com/fzxbl/mcp-toolify/cmd/mcpgen -config ./mcpgen.yaml
```

随后运行 `go generate`。生成器会覆盖对应的 `<包名>_gen.go` 和
`all_gen.go`。项目约定生成物由消费者本地重新生成，不应入库；示例目录的生成
输出也由 `.gitignore` 忽略。

## cmd/listtools

`cmd/listtools` 使用示例工具的内存传输启动 MCP server，用于检查实际暴露的名称、
描述和 schema：

```bash
go run ./cmd/listtools
go run ./cmd/listtools -short
go run ./cmd/listtools -json
```

默认输出每个工具的名称、描述、input schema 和 output schema。`-short` 每行输出
工具名和最多 100 字节的首行描述，不输出 schema；截断时保持合法 UTF-8。`-json`
以缩进 JSON 输出完整工具列表（含 schema），并关闭 HTML 转义。两者同时指定时
`-json` 优先。

该命令还支持 `-enable`（逗号分隔的包白名单）、`-match`（label selector）、
`-grep`（工具名子串）以及 `-call <name> -args <json>`；`-match` 语法错误会
直接退出，空 selector 不过滤。
