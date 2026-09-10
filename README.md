# mcp-toolify

English | [简体中文](README.zh-CN.md)
[![Go Reference](https://pkg.go.dev/badge/github.com/fzxbl/mcp-toolify.svg)](https://pkg.go.dev/github.com/fzxbl/mcp-toolify)
[![Go 1.25+](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![MCP](https://img.shields.io/badge/MCP-Model%20Context%20Protocol-6E56CF)](https://modelcontextprotocol.io)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

`mcp-toolify` turns ordinary Go functions into [Model Context Protocol](https://modelcontextprotocol.io) tools. It consists of a code generator and a runtime: the generator reads function signatures and godoc comments, while the runtime serves MCP over Streamable HTTP. It uses the official Go MCP SDK and can run as a standalone server or be embedded in an existing HTTP service.

## Core capabilities

- **Annotation-driven generation**: add `// mcp:tool` to an exported function, run `go generate`, and generate a typed wrapper, input struct, JSON Schema, and registration code.
- **No reflection at runtime**: generated wrappers call the target function directly; signature changes are reported by the compiler.
- **Shared runtime**: Streamable HTTP, tool registration, token authentication, selector admission, logids, owner routing, and middleware are provided in one runtime.
- **Plugin policies**: auditing, large-result storage, and deployment-specific policies are installed as plugins rather than built into the base.
- **Deployment options**: run independently, mount into another HTTP server, or deploy multiple replicas.

## How it works

The basic flow is:

```text
// mcp:tool
    ↓ go generate
wrapper, input struct, JSON Schema, RegisterAll
    ↓
toolify.New(..., tools.RegisterAll)
    ↓
Start(ctx) or Handlers()
```

## Quick start

Requires Go 1.25 or later.

1. Define a tool function:

```go
package greeter

import "fmt"

// Greet builds a greeting.
//
// param: name — the name to greet
// param: excited — whether to add an exclamation mark
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

2. Create `mcpgen.yaml` in the module:

```yaml
output:
  dir: ./tools
packages:
  - github.com/you/yourmod/greeter
```

3. Add a generation directive in a Go file in the same directory:

```go
//go:generate go run github.com/fzxbl/mcp-toolify/cmd/mcpgen -config ./mcpgen.yaml
```

4. Prepare `mcp.toml`. Token authentication is always enabled, so the file must contain at least one `[[tokens]]` entry:

```toml
[[tokens]]
token = "replace-with-a-random-secret"
name = "readonly-agent"
applicant = "your-id"
allow = ["capability=read,risk=none"]
deny = ["!capability", "!risk"]
```

5. Assemble and run the server:

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
	if err := r.Start(context.Background()); err != nil {
		log.Fatal(err)
	}
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

The runnable example is under [`example/`](./example).

## Annotation overview

Annotations are written in the godoc comment of an exported function:
- `mcp:tool`: expose the function as an MCP tool.
- `mcp:name=<name>`: override the default name, which is `<package>.<function_name>` in snake case.
- `mcp:labels=k=v,k2=v2`: attach opaque labels used by registration filters, token admission, and plugins.
- `mcp:bind=<param>:<Type>`: bind an interface parameter to a concrete type that can be built from JSON.
- `mcp:import=<path>`: import a package needed by a `mcp:bind` type.
- `param: <name> — <description>`: add a description to the generated JSON Schema parameter.
Labels are opaque key-value pairs. The base does not assign meaning to custom keys. `name` and `pkg` are built-in projected labels; matching overwrites tool-provided values for those keys, so they should not be set manually.
Selectors use Kubernetes-style semantics: conditions in one selector are ANDed, selector entries in a list are ORed, and `!=` and `notin` also match missing keys. An empty selector matches every tool. See the [code generation documentation](docs/code-generation.md) for the complete generation behavior.

## Authentication and admission

Token authentication is mandatory. `Config.ConfigPath` must point to a TOML file containing at least one `[[tokens]]`; missing configuration or tokens causes startup to fail.
Each token controls access with `allow` and `deny` selectors:
- `deny` takes precedence over `allow`.
- A tool matching neither is denied.
- `tools/list` and `tools/call` use the same rules, so visibility and execution stay consistent.
- HTTP requests use `Authorization: Bearer <token>`.
Caller-provided identity headers are not trusted by default. Enable `trust_identity_header` only behind a gateway that authenticates the caller and overwrites those headers. Service accounts and robots should use `identity = "fixed:<id>"` to bind an identity to the token.

## Plugin mechanism

A plugin is an ordinary Go package that provides:

```go
func Install(r *runtime.Registry) error
```

Installation order is middleware onion order: the first installed plugin is outermost. The recommended order is:

```text
audit ▸ policy plugins ▸ spill
```

`audit` is normally outermost and delivers call context asynchronously to a host-registered sink. `spill` is normally innermost and writes oversized results to local storage before returning a summary and access URL. The bundled plugin documentation is available at [audit plugin](plugins/audit/README.md) and [spill plugin](plugins/spill/README.md).
Deployment-required plugins should be listed in `required_plugins`; startup fails if any is missing. Plugins run inside the process trust boundary and are not sandboxes: a plugin can rewrite `Call.Tool` and `Call.Args`, and those changes affect execution. Install only reviewed plugins. See the [plugin development documentation](docs/plugin-development.md).

## Running and embedding

For standalone operation, call `r.Start(ctx)` and let the runtime create and manage the HTTP server.
To embed the runtime in an existing service, call `r.Mount(prefix, mountFunc)`: the host states its mount point once, and the base hands back the MCP endpoint plus every plugin route.

```go
r := toolify.New(cfg, tools.RegisterAll)
defer r.RunStop(context.Background())
if err := r.Mount("/mcp", func(pattern string, h http.Handler) {
	mux.Handle(pattern, h) // routers that need a wildcard append it here: pattern+"*"
}); err != nil {
	log.Fatal(err)
}
```

The MCP endpoint lands on `prefix` itself; plugin routes land under `prefix + "/plugin"`. That same prefix also drives the absolute URLs plugins hand to agents and the inter-replica forwarding target, so **do not rewrite the pattern inside mountFunc** (no extra `StripPrefix`, no additional prefix) — change the `Mount` argument instead. `Mount` also verifies at startup that every owner-routed pattern has a matching route, returning an error when they disagree.

Embedded deployments must call `RunStop` during shutdown so plugins can release files, connections, and background goroutines. If you need to assemble handlers entirely yourself, `r.Handlers()` is still available (with no mount prefix, i.e. mounted at the root). See the [runtime documentation](docs/runtime.md) for lifecycle and configuration details.

## Multiple replicas

Stateful resources use owned IDs to encode the owning replica. Register a tool argument with `RegisterOwnerRouted(tool, param)`, or register a plugin route with `RegisterOwnerRoutedRoute(pattern, extractor)`. Requests are forwarded only to peers on the allow-list and are authenticated again on the owner replica.

All replicas must use the same route prefix and configure `SelfAddr` (this replica's direct `host:port`, i.e. its identity) plus peer addresses or service discovery correctly; links handed outward come from `PublicBaseURL`, which may be a domain or VIP. Replica forwarding currently uses plain HTTP and passes through the caller's Bearer token. For cross-host deployments, add TLS between peers or replace pass-through with an internal credential. See the [owner routing documentation](docs/owner-routing.md).

## Documentation index

- [Code generation](docs/code-generation.md)
- [Runtime](docs/runtime.md)
- [Plugin development](docs/plugin-development.md)
- [Owner routing](docs/owner-routing.md)
- [Audit plugin](plugins/audit/README.md)
- [Spill plugin](plugins/spill/README.md)

## License

MIT, see [LICENSE](./LICENSE).
