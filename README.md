# qry-schedule-retry

把作业的调度周期、可执行时间窗、重试策略与历次尝试记录成可查询的服务，支持按作业查看下次触发时间、失败重试次数与暂停状态。

## 运行要求

- Go 1.26 或以上
- SQLite（本服务自带存储，不需要外部数据库）

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`。可用环境变量覆盖：

| 变量 | 默认值 | 用途 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_PATH` | `qry-schedule-retry.db` | SQLite 数据库文件路径 |

## 时间约定

- 调度表达式为 UTC 五字段 cron：`分 时 日 月 周`，支持 `*`、列表、`-` 区间、`/` 步长以及 `JAN…DEC`、`SUN…SAT` 名称；日与周日都受限时按 cron 标准“或”规则匹配。
- 请求中的时间（`attemptedAt`）必须是 RFC3339 且偏移为 UTC（`Z` 或 `+00:00`）；响应时间一律以 RFC3339 UTC 输出。

## 已公开的入口

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

### `POST /api/v1/jobs`

创建作业。请求字段：

- `job`：非空字符串，重复时返回 409 `job_exists`
- `schedule`：合法的 UTC 五字段 cron 表达式
- `maxRetries`：大于等于 0 的整数

成功返回 201 与完整状态，`nextRunAt` 取事务中当前时刻之后的首个匹配时刻，初始 `paused` 为 `false`、`failureRetries` 为 0：

```json
{"job":"nightly","schedule":"0 2 * * *","maxRetries":2,"failureRetries":0,"paused":false,"nextRunAt":"2026-10-04T02:00:00Z","attempts":[]}
```

### `GET /api/v1/jobs/{job}`

返回 200 与完整状态（含按 `sequence` 升序的全部尝试）；作业不存在返回 404 `job_not_found`。

### `POST /api/v1/jobs/{job}/attempts`

记录一次执行尝试。请求字段：

- `sequence`：从 1 开始的整数，同一作业内重复返回 409 `sequence_conflict`
- `outcome`：`success` 或 `failure`
- `attemptedAt`：RFC3339 UTC 时间戳

处理规则在同一事务内原子提交：

- `success`：清零失败重试次数，并按 `attemptedAt` 重算下次 cron 触发时刻。
- `failure`：失败重试次数尚未达到 `maxRetries` 时递增计数，并把下次时刻保存为 `attemptedAt + 2^当前失败重试次数 分钟`（第 1 次 +2 分钟、第 2 次 +4 分钟，依此类推）；达到上限时本次尝试照常保存，作业转为暂停且不推进原调度。
- 暂停中的作业拒绝新尝试，返回 409 `job_paused`。

### `POST /api/v1/jobs/{job}/pause`

请求体 `{"paused": true|false}`，把作业设置为新的暂停状态并返回 200 与完整状态；尝试记录、最大重试次数与失败重试次数均不受影响。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 只说明业务原因，不包含 SQL、堆栈、语句片段或路径。

| HTTP | code | 触发场景 |
|---|---|---|
| 400 | `invalid_request` | 请求体或任一字段非法 |
| 404 | `job_not_found` | 作业不存在 |
| 409 | `job_exists` | 创建时作业名重复 |
| 409 | `sequence_conflict` | 尝试的 sequence 已存在 |
| 409 | `job_paused` | 向暂停的作业提交尝试 |
| 503 | `storage_unavailable` | 存储不可用（健康检查） |
