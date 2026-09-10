# Code Generation

English | [简体中文](code-generation.zh-CN.md)

Return to the [root README](../README.md).

This document explains how `cmd/mcpgen` wraps Go functions as MCP tools. The generator reads Go AST and `go/types` information and emits compilable wrappers; runtime execution does not depend on reflection over function signatures.

## Annotation syntax

Annotations must appear in the function's godoc (`//` comments). A tool needs at least one `// mcp:tool` line. Supported markers:

| Marker | Syntax | Purpose |
| --- | --- | --- |
| Tool marker | `mcp:tool` | Select the function for tool generation. The `=` form is also accepted, but its value is ignored. |
| Tool name | `mcp:name=<name>` | Override the default tool name. The value is written verbatim into generated code. |
| Labels | `mcp:labels=k=v,k2=v2` | Declare string labels for the tool. Multiple markers are merged; duplicate keys, empty entries, and invalid keys/values are errors. |
| Parameter binding | `mcp:bind=<param>:<Type>` | Specify the concrete Go type used for a generated input field, mainly for interface parameters. Separate multiple bindings with new lines or `;`. |
| External import | `mcp:import=<path>` | Add an import path to the generated file for external types used by `mcp:bind`. Separate multiple imports with multiple markers or `;`. |
| Parameter description | `param: <name> — <description>` | Generate a `jsonschema` description for an input field. `-` may also be used as the separator. |

`mcp:labels` entries are split at the first `=`; the key must be non-empty and may not contain `= ! ( ) ,` or whitespace. A value may not contain `= ! ( ) ,`, but may contain spaces. `mcp:bind` is split at the first `:`. A bare type name resolves to `src.<Type>` from the source package; an expression containing a dot is used verbatim, such as `auth.User`. A binding only changes the generated input-field type; it does not change the original function signature.

Example:

```go
// Lookup looks up an object.
//
// param: id — object ID
// mcp:tool
// mcp:name=objects.lookup
// mcp:labels=capability=read,risk=none
func Lookup(id string) (Object, error) { /* ... */ }
```

Every `mcp:` marker other than `mcp:tool` is validated. Unknown markers, the removed `mcp:tags` / `mcp:risk` markers, and empty `mcp:labels` make generation fail; errors include the source file and line number. Marker names are case-sensitive. Parameter descriptions only match parameters that actually exist in the function; an omitted description does not add default text.

## Tool metadata and input schema

The default tool name is `<package>.<function_name_snake_case>`, for example `greeter.Greet` becomes `greeter.greet`. An explicit `mcp:name` takes precedence. The description comes from godoc: after the conventional function-name prefix is removed, the first paragraph becomes the opening sentence and the remaining text is organized into paragraphs; ordinary line breaks fold into spaces and paragraphs are joined with ` | `. The description is written to `mcp.Tool.Description`.

One input struct is generated for each function:

* The Go parameter name is capitalized to form the field name.
* The JSON field name is the parameter name in snake_case.
* `param:` text is written into the `jsonschema` struct tag.
* Field types come from `go/types`; the source package is referenced through the `src` alias in the generated file, while other packages use their package name and are imported automatically.
* mcpgen preserves the original Go field type; the schema is primarily produced by the `jsonschema-go` reflector. The stable type levels documented here are:

  | Go type | JSON Schema type |
  | --- | --- |
  | `string` | `string` |
  | `bool` | `boolean` |
  | signed or unsigned integer | `integer` |
  | `float32` / `float64` | `number` |
  | slice / array | `array` |
  | map / struct | `object` |

  The schemas of concrete types such as `[]byte`, `time.Time`, and `json.RawMessage` are determined by the `jsonschema-go` reflector, not by an mcpgen-specific contract. The generator does not add `required` or business constraints; other schema details follow the generated result and the reflector.

An `interface{}` (including an empty interface nested recursively in a pointer, slice, array, map, or struct field) cannot be constructed from JSON directly as a concrete Go value. Without a binding, the generator uses `runtime.AnyInputSchema[T]()` for the input struct: an unconstrained node explicitly permits the six JSON types `object`, `array`, `string`, `number`, `boolean`, and `null`. This is only a schema union; the function still receives `interface{}`. If the interface needs a concrete structure, use:

```go
// Apply applies a patch.
//
// mcp:tool
// mcp:bind=patch:Patch
func Apply(patch interface{}) error { /* ... */ }
```

`Patch` is generated as `src.Patch`; for an external type, declare its import as well:

```go
// mcp:bind=user:auth.User
// mcp:import=example.com/project/auth
```

`mcp:bind` does not infer external imports and does not check whether the binding name corresponds to a parameter; an unmatched binding generates no input field. The binding type must compile in the generated file. A non-empty interface without a binding generally cannot be constructed correctly by JSON Schema/reflection and is unsupported input; use a concrete type or an explicit binding.

## Return values, errors, and generated files

The wrapper handler calls the source function directly. Return-value rules:

* No return values: the result is empty.
* One non-`error` return value: wrap it as `{"result": <value>}`.
* A single `error` return value: success has an empty result; a non-nil error becomes an MCP tool error.
* Two return values whose second value is `error`: wrap the first value as `{"result": <value>}`.
* Multiple return values: wrap them in a map. Named return values use snake_case keys; unnamed or `_` returns use `result0`, `result1`, and so on. If the last value is `error`, omit it from the map and turn a non-nil error into an MCP tool error.
* Two or more non-error return values also follow the map rule above.

A generated handler represents a business `error` as the error result of the MCP `CallToolResult`, rather than as a protocol-level error. Generated code also includes `Register_<Package>` for each source package and a global `all_gen.go`; registration filters tools through `RegisterOptions.Allow` and records tool labels in the runtime registry.

### Limitations

The generator only handles function declarations with `mcp:tool` and Go packages loadable by `go/packages`; package loading, type checking, and template formatting failures stop generation. It does not generate business validation or `outputSchema` for return values, and it does not guess a concrete structure for arbitrary `interface{}` values. Regenerate and compile-check after changing a function signature, package name, or external type.

## mcpgen.yaml and the generation command

The configuration is YAML and has only these fields:

```yaml
output:
  dir: ./tools
packages:
  - example.com/your/module/greeter
```

`packages` is the list of package paths to scan; `output.dir` is the output directory. The configuration path is supplied with `-config` and defaults to `mcpgen.yaml` in the current working directory. The output directory is resolved relative to the configuration file, then converted to an absolute path. A missing configuration file, invalid YAML, or failure to load any package exits; a missing output directory is created automatically.

A package normally contains:

```go
//go:generate go run github.com/fzxbl/mcp-toolify/cmd/mcpgen -config ./mcpgen.yaml
```

Then run `go generate`. The generator overwrites the corresponding `<package>_gen.go` and `all_gen.go`. By project convention, generated files are regenerated locally by consumers and should not be committed; generated output under the example directory is also ignored by `.gitignore`.

## cmd/listtools

`cmd/listtools` starts the example tools MCP server over an in-memory transport to inspect the names, descriptions, and schemas actually exposed:

```bash
go run ./cmd/listtools
go run ./cmd/listtools -short
go run ./cmd/listtools -json
```

By default it prints each tool's name, description, input schema, and output schema. `-short` prints the tool name and at most the first 100 bytes of the first description line per row, without schemas; truncation preserves valid UTF-8. `-json` prints the complete tool list, including schemas, as indented JSON and disables HTML escaping. If both are specified, `-json` takes precedence.

The command also supports `-enable` (comma-separated package allowlist), `-match` (label selector), `-grep` (tool-name substring), and `-call <name> -args <json>`. A syntax error in `-match` exits immediately; an empty selector filters nothing.
