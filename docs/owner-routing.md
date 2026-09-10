[简体中文](owner-routing.zh-CN.md) | English

[Back to the root README](../README.md)

# Owner routing

## 1. When to use it

When a resource exists only on the replica that created it, a request after load balancing may land on the wrong replica. Examples include large results, interactive sessions, long-running task handles, and requests waiting for an out-of-band confirmation. Owner routing encodes the owning replica into the resource ID and forwards later tool calls or HTTP requests in one hop to that owner; it does not require shared storage.

```go
id := runtime.NewOwnedID()
owner, ok := runtime.OwnerOf(id)
```

`NewOwnedID` reads the process-level `SelfAddr`. If the process has not set it, the function returns an unowned random ID and `OwnerOf` returns `ok == false`, so owner routing does not happen. This is different from `Config` having an explicitly empty field. An independent `Start` falls back to the actual listening address after listening succeeds, so it can still generate owned IDs, but this is suitable only for same-host or local use.

The embedded `Handlers` mode cannot discover the host address automatically. The host must explicitly configure it or call the setter; otherwise IDs remain unowned and owner routing stays disabled. When it is set, `NewOwnedID` creates an ID containing this replica's `host:port`, and `OwnerOf` returns that owner.

## 2. Registering routing declarations

### `tools/call`

When tool arguments contain an owned ID, register the declaration before startup:

```go
runtime.RegisterOwnerRouted("jobs.get_status", "job_id")
runtime.RegisterOwnerRouted("jobs.cancel",     "job_id")
```

For a `POST` request whose JSON-RPC method is `tools/call`, runtime extracts the ID from the named argument.

### HTTP route

A plugin's own pattern should use:

```go
runtime.RegisterOwnerRoutedRoute("/jobs/", func(r *http.Request) string {
    return strings.TrimPrefix(r.URL.Path, runtime.RoutePath("/jobs/"))
})
```

`RegisterOwnerRoutedRoute(pattern, extractor)` accepts the plugin's own pattern. Runtime applies the mount prefix the host gave to `Registry.Mount` to form the actual request path, making it appropriate for an embeddable plugin whose prefix may change. It is the only entry point for path-shaped owner routing. An extractor returning an empty string means that this request does not participate in owner routing; use that to express "this replica can already answer" — the spill download endpoint returns an empty string when the file is present locally, instead of taking a detour through the owner.

This is the only registration entry for the path shape. The prefix is applied **at match time**, and when the host mounts at the root it is the empty string — "prefixed" and "mounted at the root" are the same code path, so no separate "register by bare absolute path" function is needed. That function existed once; with a prefix in place it never matched, and the misregistration surfaced only in a multi-replica deployment. It has been removed. Because the conversion happens at match time rather than at registration time, the order of registration versus `Mount` declaring the prefix does not matter — a plugin may register in `init` and still match the prefixed actual path.

When a pattern is registered but no plugin route matches it (for example `/spil/` instead of `/spill/`), `Mount` returns an error and fails startup: such a mistake is symptomless on a single replica and only shows up as a silent 404 during cross-replica forwarding.

An extractor may take the ID from anywhere in the request: the path (recommended), a query parameter, a header, or even an encrypted body (confirm's receipt ID lives inside the card payload). If it reads the body, it must handle two things itself: **bounding** the read (body size is decided by the outside world, so a bare `io.ReadAll` is a free memory amplifier) and **restoring** it (`r.Body = io.NopCloser(bytes.NewReader(b))`). The base deliberately does not buffer the body for the path shape; without the restore the owner replica receives an empty request body, while a single-replica deployment behaves perfectly.

## 3. Paths, URLs, and handlers

```go
runtime.RoutePath("/jobs/")       // mount prefix + "/plugin" + "/jobs/"
runtime.PublicURL("/jobs/" + id)  // PublicBaseURL (defaults to http://<SelfAddr>) + RoutePath(...)
```

Use `RoutePath` for the actual route path and forwarding target. Use `PublicURL` for an absolute address supplied to the caller. Without any outward address it returns an empty string; callers must check for that case.

The two addresses have different jobs and must not be conflated. `SelfAddr` is the **replica identity** — a bare `host:port` used for ID ownership and peer dialing, so it must be directly reachable and must never be a load-balancer address. `PublicBaseURL` is the **outward entry** handed to agents, humans and callback platforms; it is used only by `PublicURL` and may be a domain or VIP — the owner lives in the ID, so a request landing on any replica is proxied to the owner. When `PublicBaseURL` is unset it is derived as `http://<SelfAddr>`.

The base wraps both entry points itself, so the host has nothing to do: the MCP handler `Registry.Mount` hands over goes through an in-package "MCP shape" wrapper (both tool-argument and path extraction), and the plugin routes go through `WithPathOwnerRouting` (path shape only).

The two are not merged because of POST body buffering: the MCP shape must read the whole body into memory to parse JSON-RPC and find the ID in a tool argument, whereas the shape and size of a plugin route's POST body are not decided by the base (an upload route is perfectly legitimate), so buffering it unconditionally is a free memory amplifier. A plugin route that must cross replicas puts the ID in the path. Only `WithPathOwnerRouting` is exported — plugin tests use it to reproduce the production-shaped entry; the MCP one has no caller outside the base.

Path-routed requests preserve their original path and method, so `GET`, `POST`, `DELETE`, and other callbacks can be forwarded.

Forwarding uses a single-hop loop-prevention marker; an already forwarded request is not forwarded again. The remote replica performs token authentication again, so owner routing grants no additional permission. Streaming responses remain streaming. When the owner is unreachable, forwarding answers `404` and logs the owner address on the server side only: any caller with a valid token can trigger that path, and echoing the owner would disclose internal topology. Every forward logs one `[mcp] owner routing:` line (method, path, owner, logid) — cross-replica forwarding otherwise leaves no trace on the forwarding replica, which is exactly what you need when asking "where did that callback go?".

When no tool is registered for argument-shaped routing, the MCP shape passes the request through without reading the body: that read is a full copy paid on every POST, and "this deployment only uses the path shape" is a common case.

This is the single forwarding implementation in the base. Plugins declare an extractor and never proxy themselves — the spill download endpoint used to carry its own reverse proxy and loop-guard header, and it no longer does.

## 4. Replica allowlist and timing

Forwarding targets may come only from the peer allowlist:

```go
Config{Peers: []string{"replica-a:8011", "replica-b:8011"}} // static
toolify.SetPeerProvider(func() []string {                   // dynamic discovery
    return currentReplicaHostPorts()
})
```

Static `Config.Peers` and `SetPeerProvider` override each other; whichever is set later wins. With no provider and an empty allowlist, all remote forwarding is rejected. The request is handled locally, preventing an ID from becoming an arbitrary proxy target.

`SelfAddr`, `PublicBaseURL`, the peers/provider, and owner declarations are process-level state and must be configured before the process starts accepting requests. `ResetOwnerRoutedPathsForTest` is a test cleanup function only and must not be called at runtime; clearing it in production silently disables every callback path.

## 5. Deployment constraints

- Every replica must use the same mount prefix (one binary with one configuration satisfies this).
- The mount prefix has a single source: the `prefix` the host passes to `Registry.Mount(prefix, mount)`. The base then keeps three things in agreement: the patterns `Mount` hands to the host, URLs produced by `PublicURL`, and forwarding targets produced by `RoutePath`. The prefix must start with `/`, must not end with `/`, and must not contain `*`, `?`, ASCII spaces, or tabs; an invalid prefix makes `Mount` return an error without mounting a single route.
- When embedding an existing HTTP server, explicitly set `SelfAddr`. It must be a `host:port` directly reachable from this replica, never a load-balancer address (set `PublicBaseURL` for the links handed outward). Only an independent start without an explicit value falls back to the actual listening address, and that is suitable only for same-host or local use.
- The host must not rewrite the patterns `Mount` hands over (except to append the wildcard suffix that routers such as ghttp require): a pattern is already an outward absolute path, and rewriting it makes the forwarding target disagree with the actual route.

## 6. Security boundary

The current runtime has no direct configuration entry for TLS transport, peer credentials, or a custom proxy transport. Inter-replica forwarding uses plaintext `http://` and passes through the caller's Bearer token unchanged. Therefore peers should be deployed only on the same trusted network. Across an untrusted network, provide TLS or a network proxy outside runtime, or add internal authentication yourself; the peer allowlist and owned IDs do not provide transport security. Because the token travels with the request, anyone able to monitor that network can obtain calling privileges.

Plugins are inside the process trust boundary, not in a sandbox. A plugin can register owner declarations, affect path forwarding, and rewrite `Call.Tool` and `Call.Args` through the public API; the public API is not a substitute for code review. Install only plugins that have been reviewed and fit this trust boundary.
