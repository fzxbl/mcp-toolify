# spill — Large-result spill plugin

[简体中文](README.zh-CN.md) | English

The `spill` plugin writes tool results above a threshold to local files and replaces the response with a summary, preview, and spill ID/download entry point. This keeps multi-megabyte results out of the model context. It also exposes host APIs (`Put`, `Create`, `CreatePath`, `Open`, and `URLFor`) and the read-only `spill_explore` tool.

## Placement and size decision

Install `spill` last, as the innermost plugin. On return it is then the first middleware awakened and sees the untouched result, allowing it to decide from the actual serialized size. Installing it outside `audit` would make audit observe the rewritten summary rather than the original large result.

The threshold is based on serialized wire bytes, not payload bytes. JSON escaping and base64 can expand data; the estimator covers text, images, audio, embedded resources, resource links, metadata, annotations, and `structuredContent`, and returns an intentionally conservative upper bound. Unknown or unserializable content is treated as very large. It short-circuits once over threshold.

Error-state tool results (`IsError`) are spilled too and retain `IsError` after rewriting. Only successful `tools/call` results enter the decision; protocol errors, nil results, and non-call results such as `tools/list` are not spilled. The rewritten response drops `structuredContent` deliberately: generated tools may duplicate the payload there, so changing only `Content` would still leak the large payload.

## Configuration

```toml
[spill]
dir             = "/home/work/app/data/spill"
threshold_bytes = 32768
ttl             = "6h"
gc_interval     = "5m"
preview_bytes   = 2048
max_file_mib    = 64
max_total_mib   = 512
on_error        = "deny"
```

| Field | Semantics |
| --- | --- |
| `dir` | Storage directory. If omitted, uses `SetDefaultDir` when set, otherwise `<system temp>/mcp-toolify/spill`. At startup it is created/secured as `0700`, must be owned by the process, and is held through `os.OpenRoot`; later symlink replacement cannot redirect storage. Explicit configuration wins over `SetDefaultDir`. |
| `threshold_bytes` | Serialized-result threshold; results strictly larger are spilled. Default `65536` (64 KiB); `0` selects the default; negative values fail. There is no disable-spill setting. |
| `ttl` | Positive Go duration from the last write. Default `30m`; zero, negative, or invalid values fail. |
| `gc_interval` | Positive scan duration. Default `5m`; invalid or non-positive values fail. Shutdown stops GC but does not delete files; the next startup's initial scan handles leftovers. |
| `preview_bytes` | Text preview budget in bytes. Default `2048`; `0` selects the default; negative values fail. Non-text content is described by type/MIME/size rather than exposing base64. |
| `max_file_mib` | Per-file hard limit in MiB. Default `64`; negative values fail. Writes beyond the limit fail. |
| `max_total_mib` | Directory-wide hard limit in MiB. Default `512`; negative values fail. Oldest files are evicted first; failure is returned if space cannot be made. |
| `on_error` | `deny` (default, fail-closed) or `warn`. Any other value fails startup. |

`max_file_mib` must not exceed `max_total_mib`. MiB values are integers, not strings such as `"64MiB"`; values that overflow int64 during conversion fail startup rather than disabling quota protection.

`deny` logs the storage failure and returns an error without returning the oversized original. The error intentionally omits storage details and warns that the tool may already have executed; callers must not blindly retry. `warn` logs the failure and returns the original result, knowingly allowing one oversized response. Neither mode is silent.

## Download endpoint and security

The plugin registers `GET /spill/<id>` (typically `/mcp/plugin/spill/<id>` after host mount) through `Registry.RoutePublic` — **no Authorization required**. A browser can open the link directly. Protection is the unguessable spill ID (random segment; owned IDs also embed the producing replica), directory mode `0700`, and TTL. Download no longer checks call-subject ownership: browsers have no MCP Subject, and that check would 404 every direct link.

The summary's absolute URL comes from `PublicBaseURL` (derived from `SelfAddr` when unset) plus the download path frozen at Mount/`OnBuild`. Freezing the path prevents a later empty `routePrefix` from producing a bare `/spill/<id>` while routes remain under `/mcp/plugin/spill/`. A domain or VIP is fine for `PublicBaseURL`: the owner lives in the ID, so a download landing on any replica is proxied to the owner. `SelfAddr` itself must stay this replica's directly dialable `host:port` because it is the replica identity. With multiple replicas, the owned ID embeds the producing replica address; requests received by another replica are proxied by the base's owner routing (`RegisterOwnerRoutedRoute`) before reaching this handler — only when that address is in the peer allowlist, with loop protection, and never when the file is already present locally. The plugin carries no forwarding implementation of its own. Inter-replica forwarding currently uses cleartext `http://`; use TLS between peers for cross-host deployments.

Files contain the original tool result for the full TTL. Directory mode `0700`, file mode `0600`, unguessable IDs, and TTL are the protection boundary. Use a short TTL for sensitive data and separate directories per deployment where possible.

## `spill_explore`

The plugin registers a read-only tool with labels `capability=read`, `risk=none`, and `plugin=spill`. Token rules must allow the tool by name `spill_explore`. In multi-replica mode, `id` owner routing sends exploration to the producing replica.

Input fields are `id`, `op`, `line_offset`, `limit`, `pattern`, `jq_expr`, `depth`, and `max_bytes`. If omitted, `op` is `stat`. Supported operations:

- `stat`: returns `id`, `name`, `format`, `size`, `modified`, and `download`.
- `read`: reads up to `limit` lines from zero-based `line_offset`, returning `content`, `next_line_offset`, `eof`, and `truncated`.
- `grep`: applies a regular expression and returns `line:content` matches, up to 2,000 lines.
- `schema`: infers a bounded structure; `json` parses the whole value and `jsonl` uses its first non-empty line. `text` is unsupported.
- `jq`: evaluates a gojq expression for `json` or each `jsonl` record. `text` is unsupported.

`max_bytes` defaults to 1 MiB and is capped at 8 MiB. A scanner accepts lines up to 4 MiB; longer lines fail rather than being silently truncated. For JSON, `schema`/`jq` may need the whole document within the budget; use `grep` or the download endpoint when it is too large. Explore errors cover missing/expired content and invalid operation or arguments.

The stored format for middleware results is `json`.

## Host API

The content format is one of `FormatJSON` (`json`), `FormatJSONL` (`jsonl`), or `FormatText` (`text`). It controls download `Content-Type` and supported exploration operations:

```go
id, err := spill.Put("export", spill.FormatJSONL, data)
w, err := spill.Create("export", spill.FormatJSONL)
_, _ = w.Write(chunk)
_ = w.Close()
rc, info, err := spill.Open(id)
url := spill.URLFor(id)
spill.SetDefaultDir(dir) // before Install, only when [spill].dir is absent
```

- `Put` writes a complete payload. `Create` allocates an ID immediately and supports incremental writes; content is readable while `status=running`. `Writer` is not concurrency-safe.
- `Put`/`Create` mark content as shared in the file header. `PutFor`/`CreateFor` still record a subject for audit, but the public download endpoint no longer enforces subject ownership — possession of the ID is enough.
- `CreatePath` returns an ID and path for third-party writers that only accept filenames. The plugin cannot enforce the write process or per-file limit; sibling files such as `.wf` share the ID and TTL, but only the returned path is downloadable/explorable.
- `Open` returns an `io.ReadSeekCloser` over the payload plus `Info`; `URLFor` returns an empty string, not an error, when no usable outward address exists. The path prefix is the value frozen at Mount/`OnBuild`.
- `Info` includes `ID`, `Name`, `Format`, payload `Size`, `ModTime`, and `Shared`. `ModTime` drives TTL.
- Before `Install`, or after `OnStop`, storage APIs return `ErrNotInstalled`; the package never lazily creates a store in the system temporary directory.

Each format uses the following media type: JSON `application/json; charset=utf-8`, JSONL `application/x-ndjson; charset=utf-8`, and text `text/plain; charset=utf-8`.

## Lifecycle, quotas, and garbage collection

`Install` creates the store, registers the stop hook, starts GC, registers `spill_explore`, owner-routes it by `id`, and registers `/spill/`. GC removes expired eligible files. It does not delete files merely because the process stopped, so an in-progress download survives a restart. Shared directories are protected by deleting only files created by this process, files whose ID belongs to this replica, or ownerless files older than `ttl + 24h`; separate directories are still recommended. Total quota enforcement uses oldest-first eviction, and both automatic middleware writes and host `Writer.Close` participate in the same ledger.

## Metadata and startup observability

When middleware spills a result, it writes the ID to `Call.Meta` under `spill.MetaID` (`"spill.id"`); it is absent when no spill occurs. The summary contains the estimated serialized size, threshold, ID, retention, a download URL when available (public link, openable in a browser), otherwise the local replica path, plus a bounded non-base64 preview.

Startup logs use `[mcp] spill:` and report effective `on_error`, serialized-byte threshold, TTL, GC interval, preview size, file/total MiB quotas, and directory. A separate startup line reports that the download endpoint is public (no Authorization) and prints the actual path (e.g. `/mcp/plugin/spill/`). Storage failures use `[mcp] spill error:` and include log ID, tool, size, and strategy; the caller-facing `deny` error does not reveal directory details.
