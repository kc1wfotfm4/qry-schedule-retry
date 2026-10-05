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

登记一个作业。请求体为 JSON 对象：

| 字段 | 必填 | 规则 |
|---|---|---|
| `id` | 是 | 1 至 64 个 ASCII 字母、数字、下划线或连字符，区分大小写 |
| `schedule` | 是 | 五段表达式，空白分隔，首尾空白忽略 |
| `max_retries` | 是 | 0 至 100 的 JSON 整数 |
| `base_time` | 否 | 带时区的 RFC3339 时间；缺省时取登记事务内的一次当前时间 |

`schedule` 的分钟、小时段只接受 `*` 或对应范围（0-59、0-23）内的
ASCII 整数；日期、月份、星期段只接受 `*`。保存时统一以单个空格连接各段。
`next_trigger_at` 按 UTC 计算严格晚于参考时刻的首个匹配分钟（秒为零），
计算与保存处于同一事务，失败不留下记录。

成功返回 HTTP 201：

```json
{
  "id": "nightly",
  "schedule": "30 2 * * *",
  "max_retries": 3,
  "next_trigger_at": "2026-10-05T02:30:00Z",
  "paused": false
}
```

### `GET /api/v1/jobs/{id}`

返回 HTTP 200 与登记时相同的作业数据；查询不重算 `next_trigger_at`，
服务重启后仍返回原值。

## 错误码

| HTTP | `code` | 触发情形 |
|---|---|---|
| 400 | `invalid_request` | JSON 损坏、不是对象、必填字段缺失、字段类型/范围/时间格式不符、路径编号非法 |
| 400 | `invalid_schedule` | `schedule` 语法不被支持 |
| 404 | `job_not_found` | 查询的编号不存在 |
| 409 | `job_exists` | 编号已存在，原记录保持不变 |
| 503 | `storage_unavailable` | 存储不可用 |

额外字段一律忽略。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 只说明业务原因，不包含 SQL、堆栈或文件路径。
