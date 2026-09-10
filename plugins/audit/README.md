# audit — Asynchronous, best-effort audit plugin

[简体中文](README.zh-CN.md) | English

The `audit` plugin sends the complete context of every MCP request to one or more host-registered sinks: actor, method, tool, arguments, result, latency, headers, and plugin denial metadata. The plugin does not choose the storage backend; a sink may write logs, Kafka, a warehouse, or an external HTTP audit service.

## Placement and delivery model

Install `audit` first so it is the outermost middleware plugin. On the return path it can therefore observe denials from inner plugins and the final result after an inner plugin such as `spill` rewrites it. It is not outside the runtime token gate: token admission is installed before all plugins.

Delivery is asynchronous and best-effort. The request path only attempts a non-blocking enqueue into a bounded queue; one background worker invokes sinks in order. Audit never blocks the MCP response and never changes its return value. There is intentionally no “wait until persisted” or “reject if audit failed” mode: the tool has already executed when audit runs, so fail-closed at this point cannot prevent side effects and would encourage unsafe retries.

The trade-off is explicit: queue overflow, `kill -9`, or disk/machine failure can lose events. Drops are counted, rate-limited in logs, and exposed through `ReadStats()`.

## Configuration

```toml
[audit]
headers          = ["X-Tenant", "User-Agent"]
queue_size       = 4096
flush_timeout    = "3s"
max_args_bytes   = 1024
max_result_bytes = 2048
```

| Field | Semantics |
| --- | --- |
| `headers` | Optional request headers copied into events. The source is `Call.Headers`, not `*http.Request`; the runtime has already removed credential headers. A configured but absent header is recorded as `"-"`. Empty names fail startup. `Authorization`, `Proxy-Authorization`, `Cookie`, and `Set-Cookie` are rejected at startup. |
| `queue_size` | Bounded queue length. Default `4096`; `0` selects the default; negative values fail startup. There is no unbounded-queue setting. |
| `flush_timeout` | Positive Go duration used to drain the queue during shutdown. Default `3s`. It adds directly to stop/restart time, so do not configure it excessively. Invalid or non-positive values fail startup. |
| `max_args_bytes` | Argument snapshot limit in bytes. Default `1024`; `0` selects the default. Negative values fail. Truncation cannot be disabled. |
| `max_result_bytes` | Result snapshot limit in bytes. Default `2048`; `0` selects the default. Negative values fail. Truncation cannot be disabled. |

Configuration errors fail startup rather than silently activating defaults.

## Sink API

```go
audit.OnEvent(func(e audit.Event) error {
    // Write to the host's log, queue, warehouse, or audit service.
    return nil
})
```

Register at least one sink before traffic starts: registration may happen before or after `Install`, but must happen before `Handlers()` or `Start()`. With no sink, startup fails. If all sinks are later removed, events are counted as failed and logged. Multiple sinks run in registration order; an error or panic in one sink does not prevent the remaining sinks from running. Sink errors and panics do not affect the MCP result. Sink code runs on the worker goroutine, so implement its own timeout and retry policy for remote backends.

`Event` is an owned value snapshot: labels are deep-copied, arguments/results are bounded strings, and metadata is flattened into a new map. It can be retained or passed to another goroutine without retaining large request objects or introducing a data race.

Use the optional redaction hook before truncation:

```go
audit.SetArgsRedactor(func(tool string, args []byte) []byte {
    // Return redacted JSON bytes.
    return args
})
```

The hook receives a plugin-owned copy, so in-place edits cannot mutate the real request arguments. A `nil` return means empty arguments. Do not put secrets in tool arguments unless a redactor removes them; the runtime does not understand field semantics.

## Event fields and limits

`Event` contains `At`, `LogID`, `Method`, `Tool`, `Labels`, `Subject`, `HasIdentity`, `Args`, `ArgsTruncated`, `Result`, `ResultTruncated`, `ResultErr`, `Cost`, `IsError`, `Err`, `DeniedBy`, `DenyReason`, `Headers`, and `Meta`.

- `At` is the time the call entered audit. `LogID` matches the HTTP response log ID and can join audit events with access logs. `Method` includes `initialize`, `ping`, notifications, `tools/list`, and `tools/call`; filter in the sink as needed. `Tool` is empty for non-call methods.
- `Labels` and `Subject` are entry-time snapshots. `ActorID()` returns `Subject.ID`, or `"(anonymous)"` (`AnonymousActor`) when no trusted identity exists. `HasIdentity` distinguishes the two cases. The default runtime does not trust client identity headers, so `Subject.ID` is commonly empty.
- `Args` is the original argument JSON after redaction and byte truncation. Truncation removes only an incomplete trailing UTF-8 rune; invalid bytes in the middle are preserved. Sinks targeting PostgreSQL text or strict JSON must normalize or escape invalid UTF-8 themselves.
- For `tools/call`, `Result` is a human/search-oriented JSON fragment and may be incomplete after truncation; do not unmarshal it. For `tools/list`, it is a valid JSON array of tool names, truncated by item count. `ResultErr` records serialization errors instead of silently dropping the result.
- `IsError` preserves the tool result error state. `Err` is the protocol-level error returned by the chain. `DeniedBy` and `DenyReason` are read from `Call.Meta`; they are empty when no plugin denied the call.
- `Headers` contains only configured headers, with `"-"` for a missing value.
- `Meta` is the generic pass-through of `Call.Meta`, flattened with `fmt.Sprintf("%v")`. It is capped at 32 keys and 256 bytes per value; keys are sorted before selecting the first 32 for deterministic cross-process snapshots. This is the only generic channel for other plugins to add audit context.

## Coverage limits and security

Token admission is outside the plugin chain. A `tools/call` rejected by the runtime token gate never reaches audit; the HTTP status remains 200 and no audit event is emitted. Likewise, `tools/list` filtering happens after this plugin records the response, so `Event.Result` contains the pre-filter full registry, not necessarily what the token saw. The runtime's rate-limited warning logs (`[mcp] warning: 准入拒绝 ...` and `tools/list 过滤 ...`) are currently the only signals for those cases and must be collected for permission auditing.

The runtime strips `Authorization`, `Proxy-Authorization`, `Cookie`, and `Set-Cookie` before audit; the plugin also rejects configuring them. Nevertheless, arguments may contain application secrets such as passwords. Use `SetArgsRedactor` or redesign the tool contract before sending events to long-term storage.

## Observability

```go
s := audit.ReadStats() // Enqueued, Dropped, Delivered, Failed
```

`ReadStats()` returns zero values when the plugin is not installed or has stopped, rather than stale counters.

- `Enqueued`: events successfully placed on the queue.
- `Dropped`: queue-full or shutdown drops. Persistent growth means the queue or sink throughput is insufficient.
- `Delivered`: events delivered successfully to every sink.
- `Failed`: at least one sink returned an error or panicked, or no sink remained.

Drop and sink-failure logs are rate-limited per class: the first event is immediate, then at most one per minute, with suppressed and cumulative counts.

Startup logs use the `[mcp] audit:` prefix and report the effective sink count, queue size, flush timeout, argument/result limits, and headers. Shutdown logs report `enqueued`, `delivered`, `failed`, and `dropped`. The worker is single-threaded to preserve per-replica delivery order.

## Shutdown

`OnStop` first closes the enqueue gate, then drains the queue within `flush_timeout`. Events remaining after the budget are counted as dropped. A stuck sink cannot hold shutdown indefinitely; the worker has an additional safety wait and logs if it does not exit in time.
