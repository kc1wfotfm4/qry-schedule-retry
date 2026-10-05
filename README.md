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

登记一个作业。请求体为 JSON 对象，必填 `id`、`schedule`、`max_retries`，可选 `base_time`，额外字段忽略：

- `id`：1 至 64 个 ASCII 字母、数字、下划线或连字符，区分大小写。
- `schedule`：空白分隔的五段表达式。分钟、小时只接受 `*` 或整数（范围 0-59、0-23），日期、月份、星期只接受 `*`；保存时各段以单个空格连接。
- `max_retries`：0 至 100 的 JSON 整数。
- `base_time`：带时区的 RFC3339 字符串；缺省时取登记事务内的一次当前时间作为参考时刻。

`next_trigger_at` 按 UTC 计算严格晚于参考时刻的首个匹配分钟，秒为零。计算与保存处于同一事务，失败不留下记录。成功返回 HTTP 201：

```json
{"id":"nightly-1","schedule":"30 2 * * *","max_retries":3,"next_trigger_at":"2026-10-05T02:30:00Z","paused":false}
```

编号重复返回 HTTP 409（`job_exists`），原记录不变；表达式语法不符返回 HTTP 400（`invalid_schedule`）。

### `GET /api/v1/jobs/{id}`

返回登记时保存的作业数据（不重算时间），HTTP 200，响应字段与登记成功时相同。编号不存在返回 HTTP 404（`job_not_found`），路径编号非法返回 HTTP 400（`invalid_request`）。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。
