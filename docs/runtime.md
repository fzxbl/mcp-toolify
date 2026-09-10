# Runtime

English | [简体中文](runtime.zh-CN.md)

Return to the [root README](../README.md).

The runtime package provides `Registry`, the HTTP MCP handler, token authentication, label admission, logids, plugin middleware, and plugin HTTP routes. After plugins are installed, the base performs configuration and registration checks before accepting requests.

## Configuration

`toolify.Config` is an alias of `runtime.Config`:

| Field | Semantics |
| --- | --- |
| `Addr` | HTTP listen address, such as `:8080`; empty means listen on a system-assigned port. |
| `SelfAddr` | This replica's **directly dialable** `host:port`, such as `10.1.2.3:8011`. It is the replica identity (ID ownership and peer dialing), so it must not be a load-balancer address. When empty, standalone startup falls back to the actual listen address. |
| `PublicBaseURL` | The **outward** entry address, such as `https://mcp.example.com`, used only by `PublicURL` to build absolute links; a domain or VIP is fine. Derived as `http://<SelfAddr>` when empty. |
| `ConfigPath` | Path to the base TOML containing at least one `[[tokens]]`. Empty, unreadable, or tokenless configuration fails startup. |
| `Enable` | Package-name allowlist applied during registration; empty means no filtering. |
| `Match` | Label selector applied during registration; empty or all-whitespace means no filtering, while syntax errors fail startup. |
| `RequiredPlugins` | Plugin names that must be installed; unioned with top-level `required_plugins` from the configuration file. Missing plugins fail startup. |
| `Peers` | Static `host:port` allowlist of sibling replicas; `SetPeerProvider` may provide it dynamically, and the later setter replaces the earlier one. |

`ConfigPath` and at least one token are startup prerequisites; the runtime has no configuration switch for disabling token authentication. See [`config.example.toml`](../config.example.toml) for a complete field example.

### Process-wide state

The following state is shared by the process rather than isolated per `Registry`: the mount prefix (given by `Mount`), owner-routed tool declarations, owner-routed path declarations, and the peer allowlist/provider. A process should assemble one mutually compatible configuration; use separate processes when different prefixes, owner declarations, or peer sets are needed. Call `Config.Peers`, `SetPeerProvider`, and owner-route registration before traffic begins.

### Base TOML sections

Top-level keys include `identity_headers`, `trust_identity_header`, `required_plugins`, `[log]`, and `[[tokens]]`. After decoding the base, the runtime checks that every configuration item is claimed: unknown keys, misspelled sections or fields, and plugin sections without a corresponding installed plugin all fail startup. Plugins should decode their sections through `Registry.Config` into named structs; falling back to a map weakens spelling checks.

* `identity_headers` is an ordered list of request headers; the first non-empty value wins. When omitted or empty, it defaults to `["X-MCP-User"]`. It is read only when `trust_identity_header = true`.
* `trust_identity_header` defaults to `false`. Enable it only when a trusted gateway has authenticated the caller and overwrites these headers; the base does not implement per-person authorization based on them.
* `required_plugins` is an array of plugin names and is merged with `Config.RequiredPlugins`.
* `[log].logid_header` specifies both the inbound and outbound logid header; omitted or empty defaults to `X-Log-Id`.
* For each `[[tokens]]`, `token`, `name`, and `applicant` must be non-empty; `token` and `name` must be unique. `allow` and `deny` are selector-string arrays, with OR semantics within each array.
* `identity` is optional and accepts only `fixed:<non-empty id>`. Once bound, request identity headers are never read for that token, even when `trust_identity_header` is enabled; without a fixed identity and without trusted headers, `Subject.ID` is empty.

Tokens are supplied as `Authorization: Bearer <token>`; a bare token is also accepted. An unconfigured or unrecognized token returns HTTP 401, and errors never echo the token. `TokenConfig.Name` becomes `Subject.Token`—a purpose name, not token ciphertext. `Applicant` is retained only for configuration traceability.

## Tool admission and selectors

Admission looks only at labels in the tool registry. A match in any `deny` rule rejects the request; otherwise it must match at least one `allow` rule. No allow match also rejects (default deny). The same rule controls visibility in `tools/list` and execution through `tools/call`; a tool with no registered labels is unavailable in both. Admission runs before the plugin chain.

Selector syntax and semantics:

* Comma-separated conditions inside one selector are AND.
* Multiple selectors in an `allow` or `deny` list are OR.
* Supported forms are `key=value`, `key!=value`, `key in (a,b)`, `key notin (a,b)`, bare `key` (present), and `!key` (absent).
* `in` / `notin` must have one space on each side, and values must be parenthesized.
* A key may not contain operator characters or whitespace; a value may not contain `= ! ( ) ,`, but may contain spaces.
* Like Kubernetes, `!=` and `notin` match a missing key as well. To prevent a newly added, unlabeled tool from being allowed, pair related allow rules with a fallback such as `deny = ["!<key>"]`.
* An empty selector (empty or all whitespace) matches everything; however, a zero-value `Selector{}` not constructed through `Parse` matches nothing.
* During registration, `Match` receives the built-in `pkg` projection (the package name). Runtime tool labels also include built-in `name` (tool name) and `pkg`; these projections override same-named custom values and should not be set manually.

Registration-time filtering is determined jointly by `Enable` and `Match`. A valid selector that matches no tools registers zero tools and reports registered/filtered counts in the startup log.

## Registry lifecycle

Standalone:

```go
r := toolify.New(cfg, tools.RegisterAll)
// plugin.Install(r)
err := r.Start(ctx)
```

Embedded in an existing HTTP server:

```go
r := toolify.New(cfg, tools.RegisterAll)
h, routes, err := r.Handlers()
if err != nil { log.Fatal(err) }
mux.Handle("/mcp", h)
for pattern, handler := range routes {
    mux.Handle(pattern, handler)
}
defer r.RunStop(context.Background())
```

`New` creates the registry and registers generated tools; plugins then call `Named`, `Use`, `Route` / `RoutePublic`, `Config`, and so on to install themselves. `Start` listens itself and blocks until the context is canceled or the server exits; `Handlers` does not listen and only returns the handler and routes. Both perform base configuration loading, config claiming, required-plugin, selector, and plugin build-hook checks.

With the current implementation, assembling `Handlers` and running build hooks are idempotent: middleware is installed only once, and each build hook runs at most once in registration order. A hook error or panic fails startup and prevents later hooks from running. Hooks should only validate and log; they must not call `Use`, `Tool`, `Route`, `RoutePublic`, `Named`, `Config`, or `OnBuild`. Registry changes from these calls are rejected. `OnStop` may be registered from a hook.

`RunStop` runs cleanup hooks and is idempotent; a panic in one cleanup hook does not prevent later hooks. The base also cleans up automatically when startup fails. A registry whose cleanup has run cannot be started, mounted, or assembled with `Handlers` again; retry by creating a new registry and reinstalling plugins. Duplicate `Route` patterns—including duplicates across `Route` and `RoutePublic`—also fail startup.

## HTTP routes and prefixes

`Route(pattern, h)` registers a plugin route requiring token authentication; `RoutePublic(pattern, h)` explicitly registers an unauthenticated route, suitable for health checks. The patterns `Mount(prefix, mountFunc)` hands to the host are already prefixed actual paths; the host must mount them verbatim and must not rewrite or re-prefix them. Authenticated routes are not exposed until token authentication is ready.

The layout is fixed: the MCP endpoint lands on `prefix` itself (`/` for an empty prefix) and plugin routes land under `prefix + "/plugin"`. That same prefix drives plugin addresses generated by `RoutePath` / `PublicURL` and forwarding target paths for owner routing — all three come from one place, and only the `Mount` argument can change it. All replicas must use the same prefix. `Mount` additionally verifies that every owner-routed pattern has a matching route, failing startup when they disagree. `SelfAddr` must be the directly dialable `host:port` of this replica (a load-balancer address makes every replica look identical); links handed outward come from `PublicBaseURL`, which may be a domain or VIP.

## logid

Every HTTP request is processed by `HTTPLogID`: it reads the configured header (default `X-Log-Id`) and, if missing or invalid, generates a 16-character lowercase hexadecimal value. A valid value is 1 to 64 bytes and may contain only `A-Z a-z 0-9 . _ - :`. The value is written to the same response header, placed in context, and exposed to plugins through `Call.LogID`. Invalid input is discarded and a throttled warning is emitted; valid input is not deduplicated, so repeated caller-supplied logids cannot guarantee one-to-one event correspondence.

## Plugin security boundary

Plugins run inside the process trust boundary and are not sandboxes. Middleware may read and modify `Call`; `Call.Tool` and `Call.Args` are written back into the synthesized MCP request before downstream execution, so changing the tool name or arguments changes what is actually executed. Plugins must be reviewed and must not turn untrusted input directly into tool selection or argument rewriting. `Call.Headers` is a snapshot, and the base strips `Authorization`, `Proxy-Authorization`, `Cookie`, and `Set-Cookie`; plugins must not put credentials back into logs or results. Shared `Call.Meta` keys should use agreed constants or a plugin-name prefix.

Replica ownership, `RegisterOwnerRouted`, `RegisterOwnerRoutedRoute`, peer allowlists, and forwarding boundaries for stateful plugins are covered in the [owner routing document](owner-routing.zh-CN.md) and are not repeated here.
