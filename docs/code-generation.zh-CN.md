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

## ctx 参数

函数或方法的第一个参数类型为 `context.Context`（参数名写成 `_` 也可以）时，生成的 handler 会把本次请求的 ctx 传给它。该参数不进入输入 struct 和 JSON Schema，不需要 `param:` 说明，也不算「参数无名」。传入的就是 handler 收到的 ctx：宿主在 `Mount` 外层放入的值、logid（`runtime.LogIDFromContext`）、调用人（`runtime.SubjectFromContext`）、请求头（`runtime.HeadersFromContext`）。本文档不对取消语义做承诺。

`context.Context` 出现在非第一个位置会导致生成失败。

## 方法工具

`mcp:tool` 也可写在导出命名类型的方法上，值接收者 `(t T)` 与指针接收者 `(t *T)` 均可，`T` 的底层类型不限。默认工具名为 `<包名>.<方法名 snake_case>`（不含类型名），`mcp:name` 仍然优先。方法工具绑定在宿主提供的实例上；不需要状态的工具请写成包级函数。

任一配置的包含方法工具时，`all_gen.go` 为每个接收者类型生成一个字段，字段名 `<PkgIdent>_<RecvType>`，类型统一为 `*<pkg>.<RecvType>`（值、指针接收者都一样），并生成 `NewRegistrar`；此时**不再生成** `RegisterAll`：

```go
// all_gen.go（生成）
type Impls struct {
	Greeter_Desk *src_greeter.Desk
}

func NewRegistrar(impls Impls) (runtime.Registrar, error)
```

可运行示例在 `example/greeter` 中定义接收者和带 ctx 的方法：

```go
type Desk struct{ Greeting string }

// mcp:tool
func (d *Desk) Welcome(ctx context.Context, name string) (string, error) { /* ... */ }
```

宿主在装配时交入实例：

```go
reg, err := tools.NewRegistrar(tools.Impls{
	Greeter_Desk: &greeter.Desk{Greeting: "Welcome"},
})
if err != nil { return err }
r := toolify.New(cfg, reg)
```

实例约定：

* 实例由宿主创建，也由宿主关闭（例如 `defer db.Close()`）；toolify 不创建、不关闭。
* 一个接收者类型对应一个实例，所有请求共用。
* 方法会被并发调用，实例上不要放按请求变化的可变状态。
* 字段为 nil 时 `NewRegistrar` 返回 error，如 `tools.Impls.Greeter_Desk 为 nil：工具 greeter.welcome 需要它`（工具名按源码顺序）。该检查与 `Enable` / `Match` 无关：即使用到它的工具都被过滤掉，实例仍然必填。

只有包级函数的包不受影响：`<package>_gen.go` 与 `RegisterAll` 的生成结果与之前完全一致。含方法工具的包，其 `Register_<Pkg>` 为每个接收者类型多一个 `*<pkg>.<RecvType>` 参数，按类型名排序。

## 生成期报错

下列写法在生成阶段直接报错，信息带 `file:line`，包级函数和方法一视同仁（报错中的英文关键短语如下）：

| 写法 | 报错包含 | 替代写法 |
| --- | --- | --- |
| 未导出的函数或方法 | `unexported function` | 导出它。 |
| 泛型函数 | `generic function` | 写一个非泛型的包装函数。 |
| 可变参数 | `variadic parameter` | 改成切片参数。 |
| 参数无名或为 `_`（首位 ctx 除外） | `unnamed parameter` | 给参数命名。 |
| 两个参数首字母大写或 snake_case 后字段名/JSON 名相同 | `parameter name collision` | 改参数名。 |
| `context.Context` 不在第一位 | `context.Context must be the first parameter` | 移到第一个参数。 |
| 接收者类型未导出 | `unexported receiver type` | 导出该类型，或改成包级函数。 |
| 泛型接收者 | `generic receiver type` | 改用非泛型的包装类型。 |
| 配置的两个包包名相同 | `duplicate package name` | 重命名其中一个包；一次运行内包名必须唯一。 |
| 最终工具名重复（跨所有包，已计入 `mcp:name`） | `duplicate tool name` | 用 `mcp:name=` 重命名其中一个。 |

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
