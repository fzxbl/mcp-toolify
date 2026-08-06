# mcp-toolify

**English** | [简体中文](README.zh-CN.md)

[![Go Reference](https://pkg.go.dev/badge/github.com/fzxbl/mcp-toolify.svg)](https://pkg.go.dev/github.com/fzxbl/mcp-toolify)
[![Go 1.25+](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![MCP](https://img.shields.io/badge/MCP-Model%20Context%20Protocol-6E56CF)](https://modelcontextprotocol.io)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Stars](https://img.shields.io/github/stars/fzxbl/mcp-toolify?style=social)](https://github.com/fzxbl/mcp-toolify/stargazers)

**Turn a plain Go function into an MCP tool with one comment — `// mcp:tool`, `go generate`, done.**

`mcp-toolify` is a code generator plus runtime for building [Model Context Protocol](https://modelcontextprotocol.io) servers in Go. Annotate an ordinary function's godoc with `// mcp:tool`, run `go generate`, and it becomes a fully-typed MCP tool — input struct, JSON schema, and registration all generated for you. No hand-written wrappers, no runtime reflection. The runtime adds the things real deployments need: **connection-level authorization**, **automatic spill of oversized results**, and **call auditing**.

> Write the function, tag it, generate. Your tool's schema, description, argument docs, and risk metadata all come straight from the code you already wrote.

Built in Go, official MCP SDK, stdio + Streamable HTTP. Drop it into Cursor, Claude, Comate, or any MCP client — or embed it into a host server you already run.

---

## Why mcp-toolify

Most ways to expose Go logic as MCP tools mean hand-writing a wrapper per function: an input struct, a JSON schema, argument descriptions, a handler that unpacks args and packs results, plus registration boilerplate. It drifts from the real function the moment you touch it, and it says nothing about risk, size, or auth. mcp-toolify nails five things:

- **1. Annotation-driven, zero boilerplate — the code *is* the spec.** Add `// mcp:tool` to a function's godoc and a standalone generator (`cmd/mcpgen`) emits a typed wrapper: input struct from the parameters, JSON-schema descriptions from `param:` lines, the tool description from the doc comment. Generated code calls your function directly — **no runtime reflection**, and the tool can never silently drift from the signature.
- **2. Oversized results spill to disk — the model context never blows up.** Every tool return is measured; anything over a configurable token budget is written to a temp file and returned as an MCP resource link + short summary instead of a giant payload. A built-in companion tool, `spill_explore` (`read` / `grep` / `schema` / `jq` over json/jsonl/text), lets the agent explore the blob on demand and pull only the lines it needs. Cross-machine deployments get a direct download URL for free.
- **3. Connection-level authorization, two layers.** Per-token read/write **risk ceilings** decide what a connection may ever do (and filter `tools/list` accordingly); `tools/call` additionally checks the **caller identity** against a per-risk allow-list. One agent can share a connection across many users, each still gated by who they are. Tools declare risk with `mcp:risk=low|medium|high` and write-intent with the `write` tag.
- **4. Standalone *or* embedded — share one server.** Run it over stdio or HTTP as its own process, or get two `http.Handler`s and **mount onto an HTTP server you already have**, sharing the port and lifecycle. External, hand-written tools can be registered onto the same server alongside the generated ones (just register their risk metadata).
- **5. No proprietary dependencies — clean, portable, auditable.** Only the official Go MCP SDK, `jsonschema-go`, `BurntSushi/toml`, and (for the built-in spill tool) `gojq`. The generator is its own module with just `golang.org/x/tools` + `yaml.v3`. Nothing else to trust.

Plus the machinery that makes the above reliable:

- **Honest schemas for tricky types.** `interface{}` parameters get an explicit half-restricted JSON schema (a type union) instead of the SDK's unconstrained empty node, so the model still gets type hints. Multiple return values are packed into a stable, named JSON object.
- **Interface parameters, handled.** A parameter the model can't build from JSON (an interface) is bound to a concrete type with `mcp:bind=param:Type`; `mcp:import=<path>` pulls in an external package for that type. Fully generic — no framework-specific special-casing.
- **Pluggable auditing.** Inject a `Logger` to record every call (user, tool, args, result, cost) as structured fields; with none set it falls back to the standard library.
- **Bounded, self-cleaning spill store.** Spill files live under a temp dir with per-category TTLs and a background GC; nothing accumulates forever.

---

## Quick start

Requires Go 1.25+.

**1. Annotate a function.**

```go
package greeter

// Greet builds a greeting.
//
// param: name — the name to greet
// param: excited — add an exclamation mark
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

**2. List your packages in `mcpgen.yaml`.**

```yaml
output:
  dir: ./tools
packages:
  - github.com/you/yourmod/greeter
  # optional: expose the built-in large-result explorer
  - github.com/fzxbl/mcp-toolify/spillexplore
```

**3. Generate the wrappers.**

```go
//go:generate go run github.com/fzxbl/mcp-toolify/cmd/mcpgen -config ./mcpgen.yaml
```

```bash
go generate ./...
```

**4. Start a server.**

```go
package main

import (
	"context"

	toolify "github.com/fzxbl/mcp-toolify"
	"github.com/you/yourmod/tools" // generated
)

func main() {
	_ = toolify.Start(context.Background(),
		toolify.Config{Transport: "stdio"}, tools.RegisterAll)
}
```

A runnable end-to-end sample lives in [`example/`](./example). Inspect the exposed tools (name + description + schema) with:

```bash
go run ./cmd/listtools -short
```

## Annotation markers

All live in the function's godoc comment:

- `mcp:tool` — expose this function (required).
- `mcp:name=<n>` — override the tool name (default `<pkg>.<snake_case_func>`).
- `mcp:tags=a,b` — tags for start-up filtering; the `write` tag marks a write operation for authz.
- `mcp:risk=low|medium|high` — risk level (default none), enforced by HTTP authz.
- `mcp:bind=<param>:<Type>` — bind an interface parameter (not JSON-constructible) to a concrete input type.
- `mcp:import=<path>` — import path for a package referenced by a `mcp:bind` type outside the source package.

`param: <name> — <desc>` lines become per-argument JSON-schema descriptions.

## Run over HTTP / mount onto an existing server

`toolify.Start` with `Config{Transport: "http", Addr: ":8080"}` runs a standalone HTTP server. To mount onto a server you already have:

```go
mcpH, spillH, err := toolify.Handlers(cfg, tools.RegisterAll)
mux.Handle("/mcp", mcpH)      // MCP endpoint (Streamable HTTP)
mux.Handle("/spill/", spillH) // large-result download endpoint
```

## Authorization

Set `Config.AuthzEnabled = true` (HTTP only) and point `Config.ConfigPath` at a TOML file:

```toml
identity_headers = ["X-MCP-User"]   # ordered headers for caller identity; first non-empty wins (top-level; omit => ["X-MCP-User"])

[spill]
max_result_tokens = 4000   # results above this estimate spill to disk; -1 disables

[[tokens]]
token = "..."          # Authorization: Bearer <token>
name = "readonly-agent"
applicant = "you"
read = "medium"        # max read risk; omit to disallow reads
# write = "low"        # max write risk; omit to disallow writes

[risk_allowlist]
high = ["alice"]       # identity values allowed to run high-risk tools
medium = ["bob"]
```

- Connection + `tools/list` are filtered by the **token's** ceilings only (what this connection can ever do).
- `tools/call` additionally checks the **caller** identity (resolved from `identity_headers` front-to-back, first non-empty wins; default `X-MCP-User`) against `risk_allowlist` for `medium`/`high` tools.

## Audit headers

Point `Config.ConfigPath` at the same TOML file to record chosen request headers into the audit log. This is audit-only and independent of `AuthzEnabled`:

```toml
[audit]
headers = ["X-Tenant-Id", "X-Client-Id"]
```

Each listed header becomes its own audit field, keyed by the lowercased header name (e.g. `X-Tenant-Id` → `x-tenant-id`); a missing header is logged as `-`. Omit the section (or leave it empty) to record no extra headers. Values are trusted as-is, so only enable this behind a trusted gateway and avoid listing credential headers (`Authorization`, `Cookie`).

## Request id & access log

Every HTTP request carries a **logid** for cross-log correlation:

```toml
[log]
logid_header = "X-Log-Id"   # header to read the incoming logid from; omit => "X-Log-Id"
```

`HTTPLogID` takes the logid from that header (or generates one when absent), injects it into the request context, echoes it back in the same response header, and records it as the `logid` field of every audit record. When you run the built-in standalone server (`Start`/`Run` over HTTP) it also emits a per-request **access log** (`logid`, `method`, `path`, `status`, `cost`, `user`) through the same `Logger`, so a request can be traced across the access log and the audit log by its `logid`. When you mount the handlers into your own server (`Handlers`), no access log is added — the logid is still injected and echoed so your host's access log can correlate.

## Layout

- `toolify.go` — public entry points: `Start`, `Handlers`, `Config`, `Logger`, `SetAuditLogger`, `RegisterToolMeta`.
- `runtime/` — server wiring, authz, spill store, audit logging.
- `spillexplore/` — the built-in `spill_explore` tool.
- `cmd/mcpgen/` — the code generator (its own module).
- `cmd/listtools/` — dev helper to dump exposed tools + schemas.
- `example/` — a minimal, runnable end-to-end sample.

Generated `*_gen.go` files are normally not checked in (regenerated on build); this repo commits `example/tools/` only so the sample builds out of the box.

## Security

When served over HTTP **without** `AuthzEnabled`, the MCP endpoint is unauthenticated — calling a tool runs your Go code. Bind to loopback or front it with an authenticating proxy, or enable authz. Note that `X-MCP-User` is trusted as-is, so it **must be injected by a trusted gateway**, never accepted from clients directly.

## License

MIT — see [LICENSE](./LICENSE). Contributions and stars welcome.
