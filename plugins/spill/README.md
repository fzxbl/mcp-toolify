# spill —— 大结果落盘插件

超过阈值的工具返回值写进本地文件，返回值改写为「摘要 + 预览 + 可下载的 spill id/URL」，
避免把几十兆内容塞进模型上下文。同时对外提供一组宿主 API（`Put` / `Create` / `CreatePath`）
和一个探索工具 `spill_explore`。

- 链上位置：**最后安装**，位于洋葱最内层——返回时第一个被唤醒，拿到的是未经改写的原始结果，
  才能按真实大小判断是否落盘。装在 audit 之外会让审计记到大结果原文。
- 判定依据：**序列化后（wire）总字节**，不是 payload 字节（JSON 转义会膨胀、base64 膨胀 4/3，
  按 payload 判定属于低估）。估算恒 ≥ 真实字节，宁可略低于阈值的结果也落盘。

## 配置

```toml
[spill]
dir             = "/home/work/app/data/spill"  # 落盘目录
threshold_bytes = 32768                        # 落盘阈值，单位字节 = 32KiB；省略取 64KiB
ttl             = "6h"                         # 落盘文件保留时长（时长字符串）= 6 小时；省略取 30m
gc_interval     = "5m"                         # 回收扫描周期（时长字符串）= 5 分钟；省略取 5m
preview_bytes   = 2048                         # 摘要里保留的预览字节数 = 2KiB；省略取 2048
max_file_mib    = 64                           # 单文件上限，单位 MiB；省略取 64
max_total_mib   = 512                          # 目录总量上限，单位 MiB；省略取 512
on_error        = "deny"                       # 落盘失败时：deny（默认）| warn
```

### 字段

- `dir`（默认 `<系统临时目录>/mcp-toolify/spill`）：落盘目录。启动时权限收紧为 `0700`，
  目录不属于本进程或收紧失败则**拒绝启动**；校验通过后以句柄形式持有（`os.OpenRoot`），
  启动之后把路径换成软链既改不了写入位置也读不出别处的文件。宿主也可以用
  `spill.SetDefaultDir(dir)`（须在 `Install` 之前）给一个**运行期才知道**的目录兜底
  ——配置里显式写的 `dir` 优先。**多个部署建议各用独立目录。**
- `threshold_bytes`（默认 `65536` = 64KiB）：结果的序列化后总字节超过它就落盘。不能为负；
  `0` 表示取默认值——**没有**「关闭落盘」的写法（一个能关掉 spill 的开关只会在上下文被打满时
  才被发现它开着）。
- `ttl`（默认 `30m`，duration **字符串**）：落盘文件的保留时长，从最后一次写入算起。到期由
  回收协程删除。必须是正数：`0` 意味着刚落盘就算过期、结果永远取不回来。
- `gc_interval`（默认 `5m`，duration 字符串）：回收扫描周期。进程退出时只停回收协程、
  **不删文件**（正在下载的结果不该因一次重启消失），残留文件由下次启动的首轮扫描清掉。
  回收只动「本进程创建的 / id 属主是本副本的 / 无属主信息且已超 `ttl + 24h` 的」文件，
  多个部署共享同一 `dir` 时不会互删。
- `preview_bytes`（默认 `2048`）：改写后的摘要里保留多少字节预览。文本段取前若干字节；
  非文本段只写一行描述（类型 + MIME + 字节数），预览里不会出现 base64 原文。
- `max_file_mib`（默认 `64`，单位 **MiB**）：单个 spill 文件上限，超了直接失败（走 `on_error`）。
- `max_total_mib`（默认 `512`，单位 **MiB**）：落盘目录总量上限，超了**先按最旧优先淘汰**，
  腾不出空间才失败。配 `max_file_mib > max_total_mib` 会启动失败——那样单个文件就能顶穿总量
  上限，任何一次大结果都落不下来。

> 单位按量级分工：`threshold_bytes` / `preview_bytes` 量的是「一份结果多大」（KiB 级，用字节），
> `max_file_mib` / `max_total_mib` 量的是「最多占多少盘」（用 MiB）。四个字段都只接受**整数**，
> 没有 `"64MiB"` 这种带单位的写法（配成字符串会解码失败）。MiB 上限太大以致换算成字节溢出
> int64 时启动会失败，而不是静默把磁盘保护关掉。
- `on_error`（默认 `deny`）：落盘失败怎么办。`deny` = fail-closed，这次调用返回错误，
  **绝不把超过阈值的结果原样返回**；`warn` = 保留原结果 + 打告警日志（此时上下文会被撑一次）。
  没有「静默」这一档。注意 `deny` 的文案会劝退重试：落盘发生在返回路径上，**工具已经执行过了**。

## 下载端点

`GET /spill/<id>`，由基座套 token 鉴权（`Registry.Route`），并在此之上做**属主校验**：
落盘时记下 `Subject.Token`（用途名）与有身份时的 `Subject.ID`，非属主一律 **404**
（不是 403，也不回显属主地址——避免存在性与拓扑泄漏）。

- 摘要里的绝对 URL 依赖基座的 `PublicBaseURL`，它必须是**本副本可直连的地址**；配成负载
  均衡入口会让下载随机落到非属主副本。没配则摘要里只给本地路径，不给 URL。
- 多副本：id 内嵌产出该文件的副本地址。请求打到别的副本时，若该地址在 peer 白名单内则反向
  代理到属主副本（带环路保护头），否则 404。
- **已知风险**：副本间转发走明文 `http://` 且把调用方的 Bearer token 原样带给属主副本。
  副本跨机部署时请给 peer 之间加 TLS，或改用副本间的内部凭据。

## `spill_explore` 工具

插件自带的只读工具（labels：`capability=read` / `risk=none` / `plugin=spill`），让模型不下载
整份内容也能探索。token 规则按工具名 `spill_explore` 放行它；多副本下按 `id` 做 owner 路由。

入参：`id`、`op`、`line_offset`、`limit`、`pattern`、`jq_expr`、`depth`、`max_bytes`。
五种 `op`：

- `stat`（`op` 省略时的默认）：id / name / format / size / modified / download。
- `read`：从 `line_offset`（0 基）起读 `limit` 行，返回 `content` / `next_line_offset` /
  `eof` / `truncated`。
- `grep`：逐行正则匹配，返回 `行号:内容`（最多 2000 行）。
- `schema`：推断结构（`json` 解析整份、`jsonl` 取首行样本），`depth` 控制展开层数（默认 2）。
  `text` 不支持。
- `jq`：对 `json` / `jsonl` 执行 jq 表达式（`jsonl` 逐行执行）。`text` 不支持。

单次返回字节上限：`max_bytes` 默认 1MiB、最大 8MiB；单行最长 4MiB（超长行直接报错而不是静默
截断）。中间件自动落盘的内容格式是 `json`。

## 宿主 API

工具自己产出大结果、不想经中间件自动落盘时用它们（`import .../plugins/spill`）。
未 `Install`（或已 `OnStop`）时一律返回 `ErrNotInstalled`——刻意不「按需自动初始化」，
那会悄悄往系统临时目录写业务数据、且与配置给的目录不是同一个。

格式只有三种：`FormatJSON` / `FormatJSONL` / `FormatText`（决定下载的 Content-Type，也决定
`spill_explore` 能做哪些 op）。

- `Put(name, format, data) (id, err)`：一次写完。
- `Create(name, format) (*Writer, err)`：**增量写入**，`Create` 返回时 id 就已确定，可以先交给
  调用方、后台协程持续追加；写到一半也能被 `spill_explore` 读到。`Writer` 不是并发安全的。
- `PutFor(sub, ...)` / `CreateFor(sub, ...)`：同上，但把内容**绑定到调用主体**（同一 token 用途名
  ＋有身份时同一个人才能下载）。在工具处理函数里能拿到 `*runtime.Call` 时优先用它们——
  `Put` / `Create` 写的内容是**共享**的，任何通过 token 认证的调用方都能下载。
- `CreatePath(format) (id, path, err)`：只返回**文件路径**，给「只接受文件名」的第三方写入方
  （日志库、批量执行框架）。代价是本插件管不到写入过程：**没有单文件上限**，内容按共享处理。
  写入方顺带产生的兄弟文件（`<path>.wf` 之类）与本份内容共用 id、会一起被 TTL 回收，
  但只有 `path` 这一个文件能被下载与探索。
- `Open(id) (io.ReadSeekCloser, Info, err)`：宿主侧只读回取（例如把上一步输出喂给下一步），
  不必绕回 HTTP。
- `URLFor(id) string`：对外下载地址；没有可用的 `PublicBaseURL` 时返回**空串**（不是错误），
  拼进文案前请判空。
- `SetDefaultDir(dir)`：见上文 `dir`。

## 必须知道的语义

- **错误态结果（`IsError`）同样落盘**：一条几十兆的错误堆栈对上下文的伤害和成功结果没有区别。
  改写后保留 `IsError`，模型仍然知道这次调用失败了。
- **`structuredContent` 必然被丢掉**：生成的工具会把同一份 payload 同时放进 `Content` 与
  `structuredContent`，只改 `Content` 的话大结果照样过线进模型。
- **不落 `tools/list` 等非 `tools/call` 的结果**：把工具清单换成下载链接等于把它藏起来。
- **落盘内容是工具结果原文**，会在磁盘上留 `ttl` 那么久。工具返回值里若有敏感数据，目录权限
  （`0700`）、属主校验与 `ttl` 就是它的全部保护——把 `ttl` 配得很长要想清楚这一点。
- 落盘成功时本次调用的 `Call.Meta` 里会写下 `spill.id`（常量 `spill.MetaID`），未落盘时不写。

## 启动日志

生效配置只能从启动日志确认（`[mcp] spill:` 前缀）：`on_error` / `threshold_bytes`(序列化后字节) /
`ttl` / `gc_interval` / `preview_bytes` / `max_file_mib` / `max_total_mib` / `dir`，
以及一行**下载鉴权粒度**声明——基座默认不信任身份头（`Subject.ID` 为空），此时同一 token
用途名的调用方之间可以互相下载 `/spill/` 结果。

