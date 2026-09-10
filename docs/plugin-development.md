[简体中文](plugin-development.zh-CN.md) | English

[Back to the root README](../README.md)

# Plugin development

## 1. Plugin shape and dependency boundary

A plugin is an ordinary Go package with this entry point:

```go
func Install(r *runtime.Registry) error
```

In-tree and external plugins use the same public interface; neither has extra privileges. Production code in a plugin may depend only on the public `runtime` and `selector` packages. It must not depend on internal base packages or on other plugins. The repository test `TestPluginsOnlyDependOnPublicPackages` (`runtime/externalizable_test.go`) checks production dependencies under `plugins/...`; test imports are outside its scope.

## 2. The public Registry plugin surface

`Registry` exposes the following plugin APIs:

| Method | Purpose |
| --- | --- |
| `Named` | Declares the plugin name; used by `required_plugins` checks and startup logs |
| `Config` | Decodes and claims the plugin's own TOML configuration section |
| `Use` | Registers `func(next runtime.Handler) runtime.Handler` middleware |
| `Tool` | Registers MCP tools supplied by the plugin |
| `Route` / `RoutePublic` | Registers HTTP routes requiring token authentication or explicitly unauthenticated routes |
| `OnBuild` | Registers pre-start validation hooks |
| `OnStop` | Registers shutdown cleanup hooks |

Configuration must be decoded into a named struct. Do not use `map[string]any` or another map as a fallback. Every field promised by the documentation must be declared in the struct; otherwise the claiming check reports valid configuration as unclaimed. Keys in the configuration file that are claimed by neither the base nor a plugin, and a plugin section without its plugin installed, both fail startup.

A plugin-owned tool must also be registered with labels:

```go
r.Tool(addTool)
runtime.RegisterTool(runtime.ToolInfo{
    Name: "example.status", Pkg: "example",
    Labels: map[string]string{"capability": "read", "risk": "none"},
})
```

A runtime tool without a label registration is invisible and cannot execute under admission rules. Generated tools get their labels from generated code; handwritten or runtime plugin tools must not omit this step.

## 3. Middleware order

The order of `r.Use` calls is the entry order: the earlier installer is the outer layer and the later installer is the inner layer. A common arrangement is:

```text
audit ▸ decision policies ▸ waiting/side-effect policies ▸ spill
```

- Put audit on the outside so that inner-layer denials are recorded.
- Decision-only policies, such as admission checks, should run before policies that wait or produce side effects.
- Policies that wait for a human decision, consume quota, or write external counters are waiting/side-effect policies. Do not call `next` until permission is granted; otherwise the operation may execute before it is revoked.
- Put spill on the inside so it sees the final raw result and can evaluate its size.

These are relative ordering constraints. When several plugins have the same category, order them according to their actual side effects and policy dependencies.

## 4. Build and shutdown lifecycle

`OnBuild` runs after the base has loaded configuration, checked configuration claims, verified required plugins, and completed its own validation, but before handlers are assembled and requests are accepted. Hooks run in registration order. Returning an error or panicking fails startup; a panic is converted into a startup error and its stack is logged.

`OnBuild` is for validation and logging only. A hook must not call `Use`, `Tool`, `Route`, `RoutePublic`, `Named`, `Config`, or `OnBuild`. The current base compares observable registration state and rejects changes, but plugins must not rely on that comparison to catch every ineffective or failed call. `OnStop` is the cleanup exception: registering cleanup from a stop hook is allowed by the shutdown semantics.

Register `OnStop` before starting background goroutines, opening connection pools, or creating other resources, so a later `Install` failure can still clean them up. The host calls:

```go
r.RunStop(context.Background())
```

`RunStop` is idempotent, and the base also runs it once on startup failure. A Registry that has been cleaned up must not be reused for a startup retry; create a new Registry and install the plugins again.

## 5. The `Call` contract

Middleware receives `*runtime.Call`. The source, mutability, and important constraints of each field are:

| Field | Source | Mutability and important constraints |
| --- | --- | --- |
| `Method` | JSON-RPC request method | Read-only to the plugin; commonly branch on `tools/call` and `tools/list` |
| `Tool` | Tool name from `tools/call` | Writable; the execution terminus uses the rewritten value. Policies that inspect the tool name must account for later rewrites |
| `Labels` | Tool registration metadata, including projected `name`/`pkg` | Read-only; used by selectors or plugin policies; the base only performs matching |
| `Args` | Raw JSON arguments from `tools/call` | Writable; the execution terminus uses the rewritten value. Make a defensive copy before handing it to another component |
| `Headers` | Snapshot of HTTP request headers | Read-only contract; credentials are removed. Plugins must not modify it or obtain credentials from the original request |
| `Subject` | Calling principal resolved during token authentication | Readable; `ID` is empty by default, and `Token` is the token purpose name, not the token value. Per-user policies must reject an empty identity |
| `LogID` | Request identifier generated or received by the HTTP log-ID layer | Readable; use it to correlate requests and logs |
| `Meta` | Plugin communication map created by runtime or written by plugins | Writable; use base constants for cross-plugin keys and prefix private keys with `<plugin-name>.` |
| `Tools` | Base-injected tool lookup function | Callable, but check for nil first; useful for plugins that need to inspect the tool list |

`DenyResult` writes `denied_by` and `deny_reason` into `Meta` and returns an MCP `IsError` result, allowing callers to see a business-level denial reason. Base token admission is resolved before the plugin chain, so rewriting `Tool` in a plugin does not change the admission decision already made.

## 6. Blocking plugins

Plugins that wait for human confirmation, an external system, or a lock should follow these rules:

1. Complete all waits and denial paths before `next`; denial, timeout, client cancellation, notification failure, and process restart must not reach `next`.
2. Set a server-side wait limit clearly shorter than the MCP client's single-call timeout, and handle `ctx.Done()`.
3. Set both a global pending limit and a per-user limit so one caller cannot consume all waiting capacity.
4. Make requests idempotent by `(Subject.ID, tool, normalized args)`; retries should join an existing pending record instead of creating another approval.
5. Reject an empty `Subject.ID` before registering a pending wait.
6. Keep waiting state in memory, and fail with an alert after restart; do not assume a pending request survives a restart.

A plugin HTTP callback registered with `r.Route` receives token authentication, not per-user authorization. Anyone holding a valid token and knowing a pending identifier may submit a decision on behalf of another person, so the callback service must stay within the trusted path. In a multi-replica deployment, owner routing must also send the callback to the replica holding the waiting state; see [Owner routing](owner-routing.md).

## 7. Implementation practices

- Prefer instance fields or explicit dependencies for test injection, and avoid package-level mutable variables. A host-registered hook that must be set before plugin instance creation is an exception.
- Make defensive copies of inputs handed to the host or another component, especially `Call.Args`, which may share its backing array with an SDK buffer.
- Rate-limit and aggregate request-path warnings; do not print an unbounded log line for every request.
- State the effective policy in startup logs (for example, thresholds, limits, rules, and dependencies), so operators can confirm that configuration took effect.
- For local-file path constraints, symlink checks, regular-file checks, and other spill-specific security details, see the [spill documentation](../plugins/spill/README.md) rather than copying them into generic plugin rules.
