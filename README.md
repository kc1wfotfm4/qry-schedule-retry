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

## 作业状态

所有返回作业状态的接口都给出同一完整结构：

```json
{
  "job": "nightly",
  "schedule": "30 2 * * *",
  "maxRetries": 3,
  "retries": 0,
  "paused": false,
  "nextRunAt": "2026-10-03T02:30:00Z",
  "attempts": [
    {"sequence": 1, "outcome": "success", "attemptedAt": "2026-10-03T02:30:00Z"}
  ]
}
```

- `schedule`：UTC 五字段 cron（分 时 日 月 周），支持列表、范围、`/` 步长与月份/周名称；日与周同时受限时按 Vixie cron 的 OR 语义匹配。
- `retries`：当前连续失败的重试计数，`success` 会清零。
- `nextRunAt`：RFC3339 UTC；时间字段一律以此格式收发。
- `attempts`：按 `sequence` 升序的尝试记录。

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

创建作业。请求体：

```json
{"job": "nightly", "schedule": "30 2 * * *", "maxRetries": 3}
```

- `job`：非空字符串；`schedule`：UTC 五字段 cron；`maxRetries`：大于等于 0 的整数。
- 成功返回 201 和完整状态；`nextRunAt` 取事务当前时刻之后的首个匹配时刻，初始 `paused` 为 `false`。
- 重名返回 409，code 为 `job_exists`。
- 正文非法（含未知字段）或任一字段非法返回 400，code 为 `invalid_request`。

### `GET /api/v1/jobs/{job}`

返回 200 和完整状态；作业不存在返回 404，code 为 `job_not_found`。

### `POST /api/v1/jobs/{job}/attempts`

记录一次尝试。请求体：

```json
{"sequence": 1, "outcome": "success", "attemptedAt": "2026-10-03T02:30:00Z"}
```

- `sequence`：从 1 开始的整数；`outcome`：`success` 或 `failure`；`attemptedAt`：RFC3339 UTC。
- `success`：清零 `retries`，并按 `attemptedAt` 重算 `nextRunAt`。
- `failure` 且未达 `maxRetries`：`retries` 递增，`nextRunAt` 设为 `attemptedAt + 2^retries` 分钟后。
- `failure` 达到上限：保存尝试，暂停作业（`paused=true`），不推进原调度。
- 作业不存在返回 404（`job_not_found`）；输入非法返回 400（`invalid_request`）；作业暂停返回 409（`job_paused`）；`sequence` 重复返回 409（`sequence_conflict`）。
- 状态更新与尝试写入在同一事务原子提交，并发重复 `sequence` 仅一条落库。

### `POST /api/v1/jobs/{job}/pause`

```json
{"paused": true}
```

把 `paused` 设为新状态，返回 200 和完整状态；不改变尝试记录、`retries` 与 `maxRetries`。输入非法返回 400（`invalid_request`），作业不存在返回 404（`job_not_found`）。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 只说明业务原因，不包含 SQL、堆栈、语句片段或路径。
