# mcp-toolify

**English** | [简体中文](README.zh-CN.md)

[![Go Reference](https://pkg.go.dev/badge/github.com/fzxbl/mcp-toolify.svg)](https://pkg.go.dev/github.com/fzxbl/mcp-toolify)
[![Go 1.25+](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![MCP](https://img.shields.io/badge/MCP-Model%20Context%20Protocol-6E56CF)](https://modelcontextprotocol.io)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Stars](https://img.shields.io/github/stars/fzxbl/mcp-toolify?style=social)](https://github.com/fzxbl/mcp-toolify/stargazers)

**Turn a plain Go function into an MCP tool with one comment — `// mcp:tool`, `go generate`, done.**

`mcp-toolify` is a code generator plus runtime for building [Model Context Protocol](https://modelcontextprotocol.io) servers in Go. Annotate an ordinary function's godoc with `// mcp:tool`, run `go generate`, and it becomes a fully-typed MCP tool — input struct, JSON schema, and registration all generated for you. No hand-written wrappers, no runtime reflection.

The runtime is a **minimal base**: MCP over Streamable HTTP, a tool registry with opaque labels, a k8s-style label-selector engine, token authentication and admission, request logids, owner routing for stateful tools, and **one** middleware chain. Everything else — auditing, spilling oversized results, quotas, human confirmation — is a **plugin** you install onto that chain. Two ship here (`plugins/audit`, `plugins/spill`); the rest live in their own modules. Plugins the deployment does not install are not in the binary.

Built in Go, official MCP SDK. Drop it into Cursor, Claude, Comate, or any MCP client — or embed it into an HTTP server you already run.

---

## Why mcp-toolify

Most ways to expose Go logic as MCP tools mean hand-writing a wrapper per function: an input struct, a JSON schema, argument descriptions, a handler that unpacks args and packs results, plus registration boilerplate. It drifts from the real function the moment you touch it, and it says nothing about risk, size, or auth. mcp-toolify nails six things:

- **1. Annotation-driven, zero boilerplate — the code *is* the spec.** Add `// mcp:tool` to a function's godoc and a standalone generator (`cmd/mcpgen`) emits a typed wrapper: input struct from the parameters, JSON-schema descriptions from `param:` lines, the tool description from the doc comment. Generated code calls your function directly — **no runtime reflection**, and the tool can never silently drift from the signature.
- **2. A middleware chain plugins hang off — the base stays small.** Every `tools/call` and `tools/list` goes through one onion chain of `func(next Handler) Handler`. Cross-cutting concerns are plugins installed with `r.Use(...)`; the base ships none of them and does not know what they mean. A deployment that must have one declares it in `required_plugins`, so forgetting to install it fails startup instead of silently dropping the behaviour.
- **3. Stateful tools across replicas — resources live on one replica, calls land anywhere.** Encode the owning replica into the resource id, declare which argument carries that id with `RegisterOwnerRouted(tool, param)`, and a call-level middleware reverse-proxies the whole `tools/call` to the owner — re-authorized there, restricted to a sibling allow-list, single hop. Your tool stays a plain local function.
- **4. Label-selector authorization, deny-by-default.** Tools carry opaque `mcp:labels=` key/values; each token carries k8s-style `allow` / `deny` label selectors. The very same selectors filter `tools/list` and gate `tools/call`, so visibility and executability can never diverge, and an unlabeled tool is denied.
- **5. Standalone *or* embedded — share one server.** Run it as its own HTTP process, or get the MCP handler plus the plugin routes and **mount onto an HTTP server you already have**, sharing the port and lifecycle. Hand-written tools can be registered onto the same server alongside the generated ones (just register their labels).
- **6. No proprietary dependencies — clean, portable, auditable.** The base needs only the official Go MCP SDK, `jsonschema-go` and `BurntSushi/toml`. The generator adds `golang.org/x/tools` + `yaml.v3` (codegen only) and the spill plugin adds `gojq` for its exploration tool. An external plugin's dependencies (a Redis driver, an approval client) enter your build only if you install that plugin.

Plus the machinery that makes the above reliable:

- **Honest schemas for tricky types.** `interface{}` parameters get an explicit half-restricted JSON schema (a type union) instead of the SDK's unconstrained empty node. Multiple return values are packed into a stable, named JSON object. A parameter the model cannot build from JSON (an interface) is bound to a concrete type with `mcp:bind=param:Type`; `mcp:import=<path>` pulls in an external package for that type.
- **Fail-closed configuration.** No tokens, an invalid selector, a missing required plugin, an unknown/mistyped config key, or a plugin whose startup self-check fails — all of them fail startup. A silently degraded server is worse than one that refuses to boot.

---

## Architecture: what the base keeps, what plugins own

```
HTTP request
  └ logid  ──►  header snapshot  ──►  token authentication (401)  ──►  owner routing
                                                                        └ MCP handler
                                                                            └ token admission (allow/deny)
                                                                                └ plugin chain: (audit) ▸ (approval) ▸ (quota) ▸ spill
                                                                                    └ execution terminus ──► your Go function
```

**The base (`runtime/`, re-exported by the root package) keeps exactly this:**

- MCP protocol over Streamable HTTP, **stateless** (every request re-reads `Authorization` and the identity headers, so one agent connection can serve many people).
- The tool registry: name, package, and **opaque** labels. The base never interprets a label key.
- The `selector/` engine (k8s label selector syntax) and nothing built on top of it except token admission.
- Token authentication (401 at the HTTP layer) plus admission: `allow` / `deny` selectors filtering both `tools/list` and `tools/call`.
- A request logid, injected into the context and echoed in the response header.
- Owner routing for stateful tools, for both `tools/call` arguments and plugin HTTP routes (`RegisterOwnerRouted`, `RegisterOwnerRoutedPath`, `WithOwnerRouting`, `NewOwnedID`, `OwnerOf`).
- One middleware chain plus the `Registry` that plugins talk to, and a single execution terminus. A call executes at most once: there is no replay path a plugin could re-enter.

**Plugins own everything else.** Two ship in this repo, ordinary packages you `Install` yourself:

| plugin | section | one-line job |
|---|---|---|
| `plugins/audit` | `[audit]` | every call is handed to your own `Sink` — asynchronously, best-effort, never blocking the return |
| `plugins/spill` | `[spill]` | oversized results go to disk, the model gets a summary + download URL |

Quotas and human confirmation are **external plugins** (separate modules, installed the same way) — their policy is a deployment's own judgement, not the base's. See *External plugins* below.

`go list -deps ./runtime/` contains no plugin: the dependency arrow only ever points from a plugin to the base.

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

**2. Create `mcpgen.yaml` in your own module** — list the packages to scan and where to write the generated wrappers. Paths are relative to this file's directory.

```yaml
output:
  dir: ./tools
packages:
  - github.com/you/yourmod/greeter
```

**3. Add a `//go:generate` directive** in a Go file next to that `mcpgen.yaml` (e.g. `gen.go`), then run it. `-config` resolves relative to that file's directory.

```go
//go:generate go run github.com/fzxbl/mcp-toolify/cmd/mcpgen -config ./mcpgen.yaml
```

```bash
go generate ./...
```

**4. Assemble a server: base + the plugins you want.**

```go
package main

import (
	"context"
	"log"

	toolify "github.com/fzxbl/mcp-toolify"
	"github.com/fzxbl/mcp-toolify/plugins/spill"
	"github.com/fzxbl/mcp-toolify/runtime"
	"github.com/you/yourmod/tools" // generated
)

func main() {
	r := toolify.New(toolify.Config{Addr: ":8011", ConfigPath: "./conf/mcp.toml"},
		tools.RegisterAll)
	// Installation order IS the onion order (outermost first).
	// External plugins (approval, quota) go between audit and spill — see *Plugin order*.
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

A runnable end-to-end sample lives in [`example/`](./example) — assembly in [`example/cmd/server/main.go`](./example/cmd/server/main.go), the config it actually loads in [`example/conf/mcp.toml`](./example/conf/mcp.toml):

```bash
go generate ./example/...   # generated wrappers are NOT checked in
go run ./example/cmd/server -addr :8011 -config ./example/conf/mcp.toml

curl -sS localhost:8011 -H 'Authorization: Bearer replace-me-readonly' \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call",
       "params":{"name":"greeter.greet","arguments":{"name":"world","excited":true}}}'
```

The startup log ends with the effective chain and the rules per token, e.g.
`[mcp] plugin chain (outer→inner): [spill]`. Calling `greeter.shout` (labeled
`capability=write,risk=high`) needs the ops token. The sample
writes spilled results to the fixed `/tmp/mcp-toolify-example/spill`; on a shared
machine change `[spill] dir` to a directory you own — spill refuses to start when the
directory is not owned by its own process.

Inspect the exposed tools with `go run ./cmd/listtools` (name + description + schema);
`-short` prints one line per tool without schemas, `-json` dumps everything as JSON.

## Annotation markers

All live in the function's godoc comment:

- `mcp:tool` — expose this function (required).
- `mcp:name=<n>` — override the tool name (default `<pkg>.<snake_case_func>`).
- `mcp:labels=k=v,k2=v2` — **opaque** labels (e.g. `capability=write,risk=high`). Used for start-up filtering (`Config.Match`), token authorization and plugin rules. Bare flags are written `k=true`. The base attaches no meaning to any key; your config decides what `capability` or `risk` means.
- `mcp:bind=<param>:<Type>` — bind an interface parameter (not JSON-constructible) to a concrete input type.
- `mcp:import=<path>` — import path for a package referenced by a `mcp:bind` type outside the source package.

`param: <name> — <desc>` lines become per-argument JSON-schema descriptions.

An unlabeled tool is not a neutral default: with the recommended `deny = ["!risk", "!capability"]` it is invisible and unexecutable. That is the intended failure mode for "someone added a tool and forgot to label it".

## Label selector syntax

One engine (`selector/`) drives token `allow`/`deny`, register-time `Config.Match`, and whatever selectors external plugins expose (quota rules, an approval rule). It is k8s label selector syntax, matched against a tool's labels **plus two projections the base adds**: `name` (full tool name) and `pkg` (source package).

| operator | example | matches |
|---|---|---|
| `=` | `capability=write` | key exists and equals |
| `!=` | `risk!=high` | key missing **or** value differs |
| `in (…)` | `risk in (low,medium)` | key exists and value is listed |
| `notin (…)` | `risk notin (high)` | key missing **or** value not listed |
| `key` | `risk` | key exists (any value) |
| `!key` | `!risk` | key **missing** |

- **Commas inside one selector are AND**: `capability=write,risk in (medium,high)`.
- **A list of selectors is OR**: `allow = ["capability=read", "capability=write,pkg in (greeter)"]`.
- `in` / `notin` need a space on both sides (`risk in (high)`, not `risk in(high)`) — a deliberate narrowing of k8s syntax.
- **Missing keys follow k8s semantics**, and that is the one thing to internalize: `!=` and `notin` match a tool that *lacks* the key. So `deny = ["!risk", "!capability"]` is not decoration — without it an unlabeled tool slips past `risk notin (high)`.
- A syntax error fails startup (register-time `Match` and token rules here; plugins are expected to do the same with their own selectors); it never degrades into "matches nothing" at request time.

## Authorization

Token authz is **always on**: point `Config.ConfigPath` at a TOML file containing at least one `[[tokens]]`. A missing config path, or a config with no tokens, fails startup — the framework will not serve an unauthenticated MCP endpoint. There is deliberately no switch to turn it off.

```toml
identity_headers = ["X-MCP-User"]      # ordered headers for caller identity; first non-empty wins
# trust_identity_header = true         # off by default: identity headers are IGNORED unless you opt in
# required_plugins = ["spill"]         # plugins that must be installed, else startup fails

[[tokens]]
token = "..."          # Authorization: Bearer <token>
name = "readonly-agent"   # token PURPOSE, the channel id plugins record; never the token value
applicant = "you"         # requester, kept for traceability, never logged
# identity = "fixed:ops-robot"   # pin Subject.ID for this token; beats every request header
allow = ["capability=read,risk in (none,low)"]
deny  = ["!risk", "!capability", "dangerous=true"]
```

- `deny` wins over `allow`; matching neither means **deny** (deny-by-default).
- The same selectors drive `tools/list` visibility *and* `tools/call` execution, so "invisible" and "not executable" can never diverge. The matching rule is resolved **once**, before the request enters the plugin chain, and locked into the context — a plugin cannot widen its own scope by rewriting `Subject`.
- **Caller identity is distrusted by default.** A client can put any name in a header, so `identity_headers` are ignored unless `trust_identity_header = true` (only correct behind a trusted gateway that authenticates the human and *overwrites* those headers). For service accounts, bind the identity to the token with `identity = "fixed:<id>"`, which wins over every header. Otherwise `Subject.ID` is empty, and plugins that judge per person (quotas, approval) are expected to refuse the call rather than treating "" as a user.
- Per-person authorization ("may this human run high-risk tools?") is **not** in the base — see [Writing your own plugin](#writing-your-own-plugin).
- Missing `token` / `name` / `applicant`, a duplicate token or name, a malformed selector, or an `identity` that is not `fixed:<id>` all fail startup.

`config.example.toml` is the annotated schema for every key in this document. Its plugin sections are kept **commented out** on purpose: the file is also loaded by a base with no plugins installed, and a section nobody claims fails startup (see below). For a config that is actually loaded with plugins, see `example/conf/mcp.toml`.

## Run over HTTP / mount onto an existing server

`toolify.New(cfg, tools.RegisterAll)` returns a `*Registry`; `r.Start(ctx)` runs a standalone HTTP server. To mount onto a server you already have:

```go
r := toolify.New(cfg, tools.RegisterAll)
// spill.Install(r) / yourplugin.Install(r) / ... — registration order == middleware order
mcpH, routes, err := r.Handlers()
mux.Handle("/mcp", mcpH)                    // MCP endpoint (Streamable HTTP)
for pattern, h := range routes {            // routes registered by plugins
	mux.Handle(pattern, h)
}
```

**Mount the MCP handler and the plugin routes under the same prefix on every
replica.** Owner routing reverse-proxies to the sibling replica **keeping the
original path and method**, so replicas that disagree about where things are
mounted silently break cross-replica plugin callbacks and spill downloads (the
forwarded request lands on a 404). Single-replica deployments can use any prefix.

**Let the host choose the plugin route prefix — via `Config.RoutePrefix`, not by
rewriting patterns at mount time.** A plugin only declares its own pattern
(`/spill/`); which prefix it hangs under is the host's call, e.g. collecting every
MCP path under one prefix so a single ACL rule covers all external entry points:

```go
cfg := toolify.Config{RoutePrefix: "/mcp/plugin", /* ... */}
// Routes() now yields "/mcp/plugin/spill/" — mount it verbatim.
```

Three places must agree with the mount point, and two of them are **not** in the
host's hands: the patterns from `Routes()`, the absolute URL a plugin hands to the
agent (`runtime.PublicURL`), and the target path a plugin uses when forwarding to
the owning replica (`runtime.RoutePath`). Adding the prefix yourself at mount time
only fixes the first: the spill download URL would still point at `/spill/<id>` and
cross-replica forwarding would still target the old path — both 404s that surface
only in a multi-replica deployment. So the base applies the prefix in all three, and
a malformed prefix (no leading slash, trailing slash, wildcard) fails startup rather
than degrading into a 404 that points nowhere near its cause.

`Handlers()` is idempotent — repeated calls return the same handler (and the same error), so plugin middleware never gets installed twice. Routes a plugin registers with `r.Route(...)` come back **already wrapped in token authentication**; a route that must be reachable without a token has to say so explicitly with `r.RoutePublic(...)`. When you mount by hand, remember to call `r.RunStop(ctx)` on shutdown so plugins can close files, connection pools and sweeper goroutines. `RunStop` is idempotent, and the base **also runs it for you when startup fails** (a failing config-key check, build hook, or `net.Listen`): by then every plugin's `Install` has already started sweeper goroutines and connection pools, and asking every caller to remember one cleanup per failure path is a contract that gets missed.
Once cleanup has run the `Registry` is spent: `Handlers()` and `Start()` refuse from then on.
Retrying (say, on a different port after `net.Listen` failed) must build a fresh `Registry` —
reusing a stopped one would serve traffic with already-closed pools and stopped sweepers,
so read-only tools would answer while every tool governed by that plugin stayed permanently denied.

## Plugin configuration

A plugin reads its own TOML section through `r.Config(&myCfg)`. Every key that section decoding claims is recorded; any key in the file that **nobody** claims fails startup. That check turns `[qouta]` or `on_eror = "deny"` from a silent default into a boot error — but it only works if plugins hold up two contracts, because `toml.MetaData.Undecoded()` can tell which keys went undecoded, not where the decoded ones went:

- **Never decode your section into a map.** `map[string]any` / `map[string]string` marks *every* key inside the section as decoded, so `[yourplugin] bakcend=... limmit=...` is swallowed whole and the plugin boots on defaults with `Handlers()` returning a nil error. Use a struct with named fields.
- **Declare every field your docs promise.** If the struct covers only part of the documented section, an operator who follows the docs gets a startup failure. The struct's field set *is* the section's public contract; it must not be narrower than the documentation.

Related and intentional: a config file that still carries a plugin's section while this deployment does not install that plugin **also** fails startup. From the base's point of view "wrote `[spill]` but never installed spill" and "misspelled the section name" are the same situation — in both, something the operator believes is in effect is not. So the config and the assembly must correspond one-to-one; the example's smoke test asserts exactly that.

## Plugin order

`r.Use` is the only injection point, and installation order is the onion order (outermost first). `audit` and `spill` ship here; the position of the usual external plugins is not a matter of taste:

```
audit ▸ (blocking approval) ▸ quota ▸ spill
```

- **audit outermost** — a call rejected by any inner plugin still leaves an audit record.
- **a blocking approval plugin outside quota** — a call waiting for a human has not executed anything, so it must not have spent a quota unit yet. The unit is charged once, on the approved execution.
- **…and outside spill** — a call still awaiting approval has no result to spill.
- **spill innermost** — it must see the untouched result to judge its real size.

## External plugins

Quotas and human confirmation were developed in this repository and then **moved out on purpose** (auditing moved out too, then came back once it turned out to depend on nothing but `runtime`). Two reasons:

- Their policy is a deployment's judgement, not the base's: how many high-risk calls a person gets per day, who may approve what. The base cannot hold an opinion about any of it.
- It keeps the rule *every plugin must be installable as an outside module* honest. Each of them was built and fully tested from a separate module with nothing but a `replace` directive — that is what proves the base's exported surface is enough, and that no built-in gets a special case.

What an external plugin may rely on — the complete list, and the list those three actually use:

- `r.Use` — the chain (one injection point, installation order = onion order).
- `r.Config` — its own TOML section, claimed exactly like a built-in's (see *Plugin configuration*).
- `r.Route` / `r.RoutePublic` — its own HTTP endpoints; `Route` gets the base's token authentication.
- `r.OnBuild` / `r.OnStop` — startup self-check, and goroutine/connection reclaim.
- `r.Named` — the name `required_plugins` checks.
- `runtime.RegisterOwnerRouted` / `RegisterOwnerRoutedPath` / `NewOwnedID` — anything stateful across replicas.
- `Call.Meta` — the inter-plugin channel; an outer plugin (typically the audit one) picks it up generically, which is how a plugin's context reaches the audit stream without the audit plugin knowing it exists.

### Blocking on something outside the process

An approval plugin blocks the calling goroutine until a human answers. That is supported, and the five rules below are what make it safe. They are properties of the *chain*, so they hold for any plugin of that shape:

1. **Do not call `next` until you are allowed to.** The safety invariant is positional: every failure path — reject, timeout, client disconnect, notify failure, process restart — must happen *before* `next` is reached, so "no approval" is always "not executed". There is no replay path and no "already confirmed" marker to forge; the caller's original in-flight request is the only thing that can execute.
2. **Bound the wait, and keep it clearly shorter than the client's per-call timeout.** Then the server decides the outcome and the model reads an explicit "not executed, re-issue if you still want it" instead of a dropped connection.
3. **Cap pending work globally *and* per person.** A blocked request holds a goroutine for the whole wait; without a per-person cap one caller in a retry loop locks everybody else out of every guarded tool.
4. **Make retries idempotent.** Same person, same tool, same arguments should join the existing pending record instead of asking a second time; an agent's automatic retry must not turn one action into a pile of approvals.
5. **Refuse an empty `Subject.ID` before booking anything.** "The same person" is meaningless for an anonymous caller — see the identity notes under *Authorization*.

Two limits worth stating out loud, because both are about the base:

- A plugin route registered with `r.Route` is **token-authenticated, not authorized per person**. If the decision body carries the approver's name, then knowing a pending id and holding a valid token is enough to answer on someone else's behalf. Treat the pushing service as part of your trusted path.
- **Pending state lives in one replica's memory.** Set `PublicBaseURL` and the peer allow-list, and register the callback prefix with `RegisterOwnerRoutedPath`, so the base proxies the callback back to the replica holding the waiter (see *Stateful tools across replicas*). A restart fails every waiting call — the safe direction, but a real user-visible failure worth alarming on.

## Auditing (plugins/audit)

> Full field reference, event fields, the `Sink` contract and the observability counters live in
> [`plugins/audit/README.md`](plugins/audit/README.md) (Chinese).

```toml
[audit]
headers = ["X-Tenant", "User-Agent"]  # credential headers are rejected at startup
queue_size = 4096                     # 0 means "default"; there is no unbounded option
flush_timeout = "3s"                  # drain budget on shutdown; adds to shutdown time
max_args_bytes = 1024                 # 0 means "default"; truncation cannot be turned off
max_result_bytes = 2048
```

```go
audit.OnEvent(func(e audit.Event) error { return myBackend.Write(e) })
```

- **Delivery is asynchronous and best-effort.** The middleware only enqueues on the return path; a single background worker hands events to your `Sink`s. It never blocks the call and never changes the result — a slow or broken audit backend cannot add its P99 to every MCP call.
- **There is deliberately no fail-closed mode.** Audit judges on the *return* path, where the tool has already run, so refusing to return cannot prevent any side effect; only the pre-execution gates (per-person allowlist, approval, quota) can. The price is stated in the plugin README rather than hidden: "every call leaves a record" degrades to best-effort, and a full queue or a `kill -9` loses events.
- **Drops are never silent**: counted, alarmed (throttled to one line per minute per class, with a suppressed/total tally), and exposed through `audit.ReadStats()` for the host's monitoring.
- **At least one `Sink` must be registered before serving traffic** — zero sinks fails startup, because "the plugin is installed but nothing lands" is exactly the silent failure `required_plugins` cannot see.
- Two blind spots, both about the base: token-authz denials never reach the plugin (that layer sits before the whole chain), and a `tools/list` event records the **unfiltered** tool list. Collect the base's throttled `准入拒绝` / `tools/list 过滤` log lines too if you audit who saw or tried what.

## Spilling oversized results (plugins/spill)

> Full field reference, defaults, host APIs and the `spill_explore` op list live in
> [`plugins/spill/README.md`](plugins/spill/README.md) (Chinese).

```toml
[spill]
dir = "/var/tmp/mcp-toolify/spill"
threshold_bytes = 65536      # bytes AFTER serialization; 0 means "default", never "off"
ttl = "30m"
gc_interval = "5m"
preview_bytes = 2048
max_file_mib = 64            # disk caps are in MiB, not bytes
max_total_mib = 512
on_error = "deny"
```

- Judged on the **serialized** size of the whole result — text, images, audio, embedded resources, resource links and `structuredContent` — because JSON escaping and base64 inflate the payload; error results (`isError`) spill too. Over the threshold, the result is written to disk and replaced by a summary + download URL.
- `/spill/<id>` is registered with `r.Route`, so the base wraps it in token auth (it serves raw tool output), and the plugin additionally checks **ownership** (the token purpose recorded at spill time, plus `Subject.ID` when the owner had an identity). Anyone else gets a bare 404 — not 403, and no owner hint.
- The directory is tightened to 0700, verified at startup, then held as a handle (`os.OpenRoot`), so swapping the path for a symlink afterwards cannot redirect reads or writes. A non-fatal periodic reconciliation warns if the directory identity changes — **do not restart the service for that alarm**; a restart is what would make the new handle land on the swapped path.
- The absolute URL in the summary comes from `PublicBaseURL`, which must be *this replica's* directly reachable address; a load-balancer entry point would send downloads to the wrong replica. Leaving it unset is a tolerable **degradation for spill** (ids stay in the legacy random form and a download landing on another replica 404s), unlike a blocking approval plugin, where a callback that cannot reach the issuing replica means the operation can never be approved.
- On shutdown the sweeper stops but files are kept (a download in flight should survive a restart); leftovers are reclaimed by the first sweep after the next boot.

### Using the store from your own code

The same store is exported, so a tool that *knows* its output is huge (a CSV export, a log scan) can put it there itself instead of returning a giant payload and letting the middleware cut it down:

```go
id, err := spill.Put("query.csv", spill.FormatText, csvBytes)  // whole blob in memory
w, err := spill.Create("scan.jsonl", spill.FormatJSONL)        // streaming writer, w.ID()
id, path, err := spill.CreatePath(spill.FormatText)            // third-party writer that only takes a filename
url := spill.URLFor(id)                                        // absolute download URL for the model
spill.SetDefaultDir(dir)                                       // host default when [spill] omits dir
```

- Return `spill.URLFor(id)` plus a short preview; the model reads slices through the `spill_explore` tool (`stat` / `read` by line offset / `grep` / `schema` / a `jq` filter for `json` and `jsonl`) and a human downloads the whole thing from `/spill/<id>`.
- `Put`/`Create` content is **owner-scoped** exactly like a spilled result. `CreatePath` is the escape hatch for writers that only accept a path (a logger, an external command): the plugin no longer controls the write, so the content is marked **shared** (any token-authenticated caller may download it) and the per-file byte cap does not apply — **bound your own writes**. Sibling files the writer creates next to it (`<id>.ext-text.wf`) are recognised as part of the same id, so TTL/quota do not leak.
- All of them return `spill.ErrNotInstalled` when the plugin is not installed; that keeps "the host forgot to install spill" a startup/first-call error instead of silent data loss.

## Writing your own plugin

A plugin is an ordinary package with `func Install(r *runtime.Registry) error`. Here is per-person authorization — deliberately *not* built in, because the data source (on-call roster, mail group, approval system) varies per organization:

```go
// Authorization by person: the data source can be an on-call roster or an approval system.
//
// NOTE: the decision is factored out into Allowed rather than inlined so it can be
// reused from your own approval / notification flow. Position matters for policies
// whose verdict changes over time (on-call, freeze windows): install this plugin
// INSIDE the approval plugin and it is evaluated when the approved call actually proceeds,
// which is the moment those policies are about — install it outside and a call
// booked while on call still executes when the approval lands after hours.
func Allowed(id string) error {
	if id == "" {
		return fmt.Errorf("%s", "no caller identity resolved, cannot judge per person")
	}
	if !onDuty(id) {
		return fmt.Errorf("%s is not on call and may not run this operation", id)
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

### The plugin contract

Everything below is enforced or relied upon by the base. The four built-in plugins are the reference implementations.

**Registry surface**

- `r.Use(mw)` is the **only** injection point; registration order is the onion order.
- `r.Named(name)` declares the plugin name for `required_plugins` and the startup log.
- `r.Config(&cfg)` is the **only** legitimate way to read configuration; see [Plugin configuration](#plugin-configuration) for the two contracts (no map decoding, declare every documented field).
- `r.OnBuild(fn)` runs a startup self-check after the base's own validation and just before the server starts serving; returning an error fails startup, and so does a panic (it is recovered and turned into a startup error — otherwise `sync.Once` would cache "no handler and no error" and the host would mount a nil handler). Use it — not `Install` — for "did the host register the callback I need?", because a callback registered *after* `Install` but before serving is legitimate. **Keep the runtime fallback as well**: `OnBuild` covers only what is knowable at startup, while a callback set back to nil at runtime is only caught by the fallback. The plugins that take a host callback (an audit sink, an approval notifier) are two-layered on purpose. A hook may only validate and log: calling `Use` / `Tool` / `Route` / `RoutePublic` / `Named` / `Config` / `OnBuild` from inside a hook **fails startup**, because at that moment each of them is silent — the chain is not wired yet (so `r.Use` would take effect but never appear in the already-printed `plugin chain` log), and `required_plugins` and the config-claim check have both run. `OnStop` is allowed: cleanup hooks are only used later, so registering one there really works.
- `r.Route(pattern, h)` for authenticated routes, `r.RoutePublic(pattern, h)` for deliberately unauthenticated ones. Two methods rather than a bool: a forgotten `public` only adds a layer of auth, a forgotten `false` removes one.
- `r.OnStop(fn)` for files, connection pools and sweeper goroutines. Register it *before* starting the goroutine, so a later plugin's `Install` failure still leaves it collectable.
- `r.Tool(add)` registers a plugin-owned MCP tool. Such a tool must also be registered in the label registry (`runtime.RegisterTool` / `toolify.RegisterToolMeta`) — an unlabeled tool is invisible and unexecutable by design, so skipping this looks like "my tool vanished".

**Things about `Call` that bite**

- `Subject.ID` is empty unless `trust_identity_header = true` or the token has a fixed identity. A plugin that judges per person must handle that **explicitly** and must not treat `""` as one user.
- `Call.Headers` already has `Authorization` / `Proxy-Authorization` / `Cookie` / `Set-Cookie` stripped. Do not go around it to the raw `*http.Request` for credentials.
- **`Call.Tool` and `Call.Args` are writable, and writes take effect**: the execution terminus writes them back into the synthesized request. Therefore **any decision made by tool name must be re-checked after rewriting**. Known instance: token admission (resolved from the token before the chain runs, so rewriting `c.Tool` cannot move it). When you add a plugin that branches on the tool name, ask yourself: does it still hold after a rewrite?
- `Call.Meta` is the inter-plugin channel; the base only knows `denied_by` / `deny_reason` (written by `DenyResult`). Prefix your private keys with the plugin name. An outer audit plugin is expected to pick the whole map up **generically**, so that is also how a plugin gets its own context into the audit stream without the audit plugin knowing it exists.

**Blocking on something outside the process**

- A middleware may block (waiting for a human, an approval system, a lock) as long as it blocks **before** calling `next`: that is what makes "did not complete" mean "did not execute". Do not try to run the tool first and undo it afterwards.
- Bound the wait with your own timer **and** honour `ctx.Done()`, and keep the bound clearly under the MCP client's per-call timeout, so the server produces an explicit verdict instead of a dropped connection.
- Cap concurrent waiters globally **and per person**; each waiter is a parked goroutine plus state. Without the per-person cap, one caller in a retry loop denies the tool to everyone else.
- Make the wait idempotent on `(Subject.ID, tool, normalized args)`: agents retry, and one action must not become a queue of pending approvals.
- In-memory waiters do not survive a restart. Fail them (the safe direction) and alarm; do not pretend they are still pending.
- The external approval plugin described under *External plugins* is the reference implementation of all five points.

**Discipline learned the hard way**

- Test injection points are unexported struct fields with `nil` meaning production — **no package-level mutable vars** (global state, cannot `t.Parallel()`, a missed restore pollutes other tests). The exception is a *host-facing* registration hook (`spill.SetDefaultDir`, `audit.OnEvent` / `audit.ReadStats`; an approval notifier in the external ones) which must be callable before any plugin instance exists.
- Callbacks you hand to the host get **defensive copies** when the contract says "do not rewrite this". Measured: an in-place redactor corrupted the real request bytes, because `Call.Args` and the SDK's `Arguments` share one backing array.
- Alarms on the request path are throttled edge-then-summary; "always logs" must not become "one line per request" (measured: 9598 lines in a 0.35 s shutdown window).
- Declare your effective policy in the startup log. It is the only place an operator can confirm that what they configured is in effect.
- Anything read from disk by an externally supplied id needs `os.OpenRoot` **plus** `Lstat` + `IsRegular` + `O_NOFOLLOW`: `os.Root` only guarantees "cannot escape the root", not "will not follow a relative symlink inside it". Without `Lstat`, a FIFO in that directory blocks the request goroutine forever.
- `runtime.ResetOwnerRoutedForTest` and `runtime.ResetOwnerRoutedPathsForTest` are **test-only**. Calling either at runtime clears a process-wide registry, which silently disables every owner-routed lookup and every receipt route. They are exported only because plugins outside `runtime` need them in tests.

### Every plugin must be externalizable

A plugin in this repository is a *reference implementation*, never a privileged one. The rule: **plugin production code may import only `mcp-toolify/runtime` and `mcp-toolify/selector`** — nothing under an internal path, and no cooperation from the base that an outside author could not obtain.

The reason is that an external plugin author's ceiling is exactly the base's exported API surface. The moment a built-in plugin reaches for something else, the base has quietly grown a special case, and "you can write this yourself" becomes false without anyone noticing. Whenever a plugin needs a new capability, the capability goes into the base in *generic* form — that is why owner routing takes a path extractor instead of knowing about any particular callback prefix, and why `Call.Meta` is passed through as an opaque map instead of growing an "approver" field.

A machine holds the line: `TestPluginsOnlyDependOnPublicPackages` (`runtime/externalizable_test.go`) shells out to `go list` over `plugins/...` and fails on any other module-internal import. It deliberately ignores test imports — cross-plugin *tests* are legitimate composition checks.

The audit, quota and confirm plugins are the rule applied to itself: all three were written in-tree, then moved out and installed back from their own modules. Doing it for real is what turned "an external plugin can do this" from a claim into a fact — and it is why the base grew `RegisterOwnerRoutedPath` and a generic `Call.Meta` instead of learning what a receipt or an audit record is. Audit then came back into this repo *because* the exercise showed it needs nothing but `runtime` — being bundled buys it no privilege, and it can be moved out again the same way.

## Request id (logid)

```toml
[log]
logid_header = "X-Log-Id"   # header to read the incoming logid from; omit => "X-Log-Id"
```

Every HTTP request carries a logid: taken from that header when **well-formed** (1-64 chars of `[A-Za-z0-9._:-]`), otherwise generated; injected into the request context, echoed back in the same response header, and exposed to plugins as `Call.LogID`. The value comes from the caller while framework and audit log lines are unquoted `key=value`, so accepting it verbatim would let callers inject forged fields (measured: `X-Log-Id: fake logid=deadbeef actor=admin` was accepted as one string). Malformed values are dropped, replaced by a generated one, and reported through a throttled warning.

There is no built-in access log — the host's HTTP server already has one, and an audit plugin correlates with it through the same logid. **Logids are not deduplicated**, though: a caller that always sends the same value breaks the one-to-one mapping between audit events and access log lines. Enforce uniqueness at the gateway if you need it.

## Stateful tools across replicas

Some tools produce a resource that physically lives on the replica that created it: a large result on local disk, an interactive session, a long-running job, a probe handle. In a load-balanced deployment a follow-up call (read it, poll it, cancel it) can land on a *different* replica and miss. The base solves this generically — no shared storage, no logic registration:

- **Encode the owner into the id** with `NewOwnedID()` (derived from `Config.PublicBaseURL`). Without a base URL, ids stay in the legacy random form and no routing happens.
- **Declare where the id appears**, once, at init time. There are two forms, because a follow-up can arrive either as a tool call or as a plain HTTP request:

  ```go
  // (a) a tools/call argument
  runtime.RegisterOwnerRouted("your.get_status", "job_id")
  runtime.RegisterOwnerRouted("your.cancel",     "job_id")

  // (b) an HTTP route your plugin registered with r.Route — the owner is in the path
  runtime.RegisterOwnerRoutedPath("/confirm/", func(r *http.Request) string {
      return strings.TrimPrefix(r.URL.Path, "/confirm/")
  })
  ```

- **The base does the rest.** `WithOwnerRouting` wraps the MCP endpoint (form (a) plus (b)); every route returned by `Registry.Routes()` is wrapped in `WithPathOwnerRouting` (form (b) only, no body buffering). If the extracted id is owned by a *remote* sibling on the allow-list, the request is reverse-proxied to that owner **with its original path and method preserved** (so `POST /confirm/x` and `GET /spill/x` both work) and the response streamed back; otherwise it passes through locally. A loop-guard header caps forwarding at a single hop, and the forwarded request is re-authorized on the owner, so routing grants no extra privilege.

Consequently `PublicBaseURL` is an **optional optimization for spill** (without it, a download that lands on the wrong replica simply 404s) but a **requirement for a blocking approval plugin** in a multi-replica deployment: an approval that cannot reach the issuing replica can never wake the blocked call, which then times out as "not executed".

Forward targets are restricted to a live sibling allow-list — `Config.Peers` for a static snapshot, or your own service discovery:

```go
toolify.SetPeerProvider(func() []string { return currentReplicaHostPorts() })
toolify.SetPeers([]string{"replica-a:8011", "replica-b:8011"})
```

An empty list (and no provider) denies all remote forwarding, preventing SSRF: a call for an unlisted remote owner is served locally (and simply misses) rather than proxied anywhere.

**Deployment precondition, not a detail:** replica-to-replica forwarding uses plain `http://` and passes the caller's `Bearer` token through as-is (`runtime/owner_routing.go`, and the same shape in `plugins/spill`). Anyone who can sniff the internal network sees that token — and with an approval plugin installed, that token is the credential for approving high-risk operations. **Deploy peers only inside one trusted network, and put TLS between them when they live on different hosts**, or replace the pass-through with an internal peer credential.

## Layout

- `toolify.go` — public entry points: `New`, `Config`, `Registry`, `RegisterToolMeta`, `WithOwnerRouting`, `RegisterOwnerRouted`, `RegisterOwnerRoutedPath`, `NewOwnedID`, `OwnerOf`, `SetPeers`, `SetPeerProvider`.
- `runtime/` — the base: MCP/HTTP wiring, tool registry, token authz and admission, middleware chain, execution terminus, logid, owner routing, `Registry`.
- `selector/` — the k8s-style label selector engine.
- `plugins/audit` — built-in plugin: async best-effort audit delivery to host `Sink`s.
- `plugins/spill` — built-in plugin: oversized results to disk (quotas / approval live in their own modules).
- `cmd/mcpgen/` — the code generator (`go run github.com/fzxbl/mcp-toolify/cmd/mcpgen`).
- `cmd/listtools/` — dev helper to dump exposed tools + schemas.
- `example/` — a runnable end-to-end sample: `cmd/server` (assembly), `greeter` (annotated tools), `conf/mcp.toml` (the config it loads).
- `config.example.toml` — annotated schema for every configuration key.

Generated `*_gen.go` files are **not** checked in (`/example/tools/` is git-ignored): run `go generate ./example/...` before building or testing a fresh checkout.

## Security

- **Authentication and admission.** Every request must carry a configured `Authorization: Bearer <token>`; anything else gets a 401 before reaching the MCP layer. A tool with no registered labels is neither visible nor executable (deny-by-default). Plugin routes are authenticated the same way unless they explicitly opt out with `RoutePublic`. `Call.Headers` has the credential headers stripped, so a plugin can never read or forward the raw token.
- **Identity.** Client identity headers are not trusted by default; enable `trust_identity_header` only behind a gateway that overwrites them, or pin `identity = "fixed:<id>"` per token.
- **Trust boundary: plugins are inside it, not sandboxed by it.** `allow` / `deny` constrain the **caller (the token)**, not plugins. Rewriting `Call.Tool` is a documented part of the contract, which means a plugin has **full authority over which tool ultimately executes** — measured with a probe: a weak token calls `probe.entry` (which it may), a plugin rewrites `c.Tool` to `demo.write` (which the token may **not** call), and the terminus executes it, because admission judged the pre-rewrite name. This is by design, not a defect. What the base *does* guarantee is narrower: the admission rule is resolved from the token before the chain runs, so it cannot be swapped from inside the chain. The only ways to constrain a plugin are code review and not installing plugins you do not trust.
- **Fail-closed startup.** No tokens, an invalid selector, a missing required plugin, an unclaimed config key, or a failing plugin build hook all refuse to boot.
- **Replica-to-replica traffic is plaintext with a pass-through Bearer token** — see the deployment precondition above.

## License

MIT — see [LICENSE](./LICENSE). Contributions and stars welcome.
