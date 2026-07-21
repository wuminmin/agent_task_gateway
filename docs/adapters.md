# OA 与数据源适配器接口

Gateway 将 OA 草稿创建和数据执行抽象为 Go 接口，但当前 HTTP 协议、Catalog 和 SQL 策略仍针对 Demo OA 与 PostgreSQL。替换适配器时必须保留 Gateway 的确定性授权边界，不能把敏感级别、预算或 SQL 放行交给外部系统。

## OA 出站接口

Gateway 只要求 OA Adapter 创建草稿：

```go
type ApprovalAdapter interface {
    CreateDraft(context.Context, DraftRequest) (DraftResponse, error)
}
```

内置客户端调用：

```http
POST http://oa-demo:8090/api/drafts
Authorization: Bearer <OA_SERVICE_TOKEN>
Content-Type: application/json
```

请求：

```json
{
  "task_id": "task_...",
  "requester": "alice",
  "objective": "查询销售部月度报销总额",
  "data_products": ["expense_summary"],
  "approved_columns": {"expense_summary": ["month", "department", "total_amount"]},
  "mandatory_scope": {"department": ["销售部"]},
  "sensitivity": "low",
  "budget": {
    "max_queries": 10,
    "max_rows": 500,
    "max_db_ms": 30000,
    "query_timeout_ms": 5000,
    "task_ttl_ms": 1800000
  },
  "approval_mode": "auto",
  "catalog_version": "2026-07-21.1",
  "callback_context": "callback_...",
  "authorization_snapshot_sha256": "<64 位十六进制 SHA-256>"
}
```

人工审批还会包含 `"approver":"bob"`。OA 必须返回 HTTP `201 Created`；响应不允许未知字段，`draft_id` 与 `url` 必须非空。Demo 同时返回 `state: draft`：

```json
{
  "draft_id": "oa_...",
  "state": "draft",
  "url": "http://127.0.0.1:8090/tasks/oa_..."
}
```

内置客户端超时 5 秒，响应体上限 64 KiB。创建失败时 Gateway 不创建本地任务；若 OA 已创建草稿而后续 SQLite 写入失败，当前 Demo 没有补偿删除协议，生产适配器应提供幂等创建和补偿/对账机制。

OA 展示 Gateway 已确定的产品、字段、强制范围、敏感级别、五维预算、审批方式、目录版本与快照 Hash。创建、提交和决定前都会重验 Hash；OA 不能自行降低审批路由、改变数据范围或预算。

## OA 回调接口

OA 向 Gateway 调用：

```http
POST http://gateway:8080/api/v1/oa/callback
Content-Type: application/json
X-OA-Event-ID: evt_...
X-OA-Timestamp: <Unix 秒>
X-OA-Signature: v1=<hex HMAC>
```

签名覆盖原始请求字节：

```text
HMAC-SHA256(OA_CALLBACK_SECRET, timestamp + "." + raw_request_body)
```

Body 契约：

```json
{
  "event_id": "evt_...",
  "task_id": "task_...",
  "draft_id": "oa_...",
  "status": "approved",
  "actor": "bob",
  "occurred_at": "2026-07-21T12:00:00Z",
  "catalog_version": "2026-07-21.1",
  "callback_context": "callback_...",
  "authorization_snapshot_sha256": "<与草稿一致的 SHA-256>",
  "approval_receipt": "receipt_..."
}
```

支持的状态与 actor：

| 状态 | 允许 actor | 状态变化 | 额外要求 |
|---|---|---|---|
| `submitted` | `alice` | `AWAITING_SUBMISSION → AWAITING_APPROVAL` | 无 |
| `approved`（自动） | `oa-auto` | `AWAITING_APPROVAL → ACTIVE` | `approval_receipt` 必需 |
| `approved`（人工） | Catalog 指定的 `bob` | `AWAITING_APPROVAL → ACTIVE` | `approval_receipt` 必需 |
| `rejected` | Catalog 指定的 `bob` | `AWAITING_APPROVAL → ARCHIVED(rejected)` | `approval_receipt` 必需 |

Gateway 会同时校验：

- Header 时间戳在当前时间前后 5 分钟内，签名使用常量时间比较。
- `occurred_at` 也在前后 5 分钟内。
- Header Event ID 与 Body 一致。
- task、draft、Catalog 版本、随机 callback context 与持久化任务一致。
- 回调快照 Hash 与本地 pending 一致，且本地 pending 的字段、范围、预算等重算 Hash 后仍一致。
- actor、审批模式和当前任务状态允许该动作。

`event_id + raw body SHA-256` 用于幂等：首次处理把审批事件、Grant、预算、状态和 HTTP 响应写在同一个 SQLite 事务；验证当前请求签名后，完全相同的已完成事件即使原始事件时间已过窗口或当前 Catalog 已更新，也返回首次保存的响应。复用 Event ID 但改变 Body 返回冲突。Gateway 重启会把单独 Claim 后未完成的事件标记为可重试。

OA Demo 对回调最多尝试 3 次。企业 OA 应采用持久化 Outbox、指数退避、告警和人工对账，不能依赖进程内 goroutine。

## OA Demo 页面与会话

- `GET/POST /login`：Alice/Bob 登录。
- `GET /tasks`、`GET /tasks/{draftID}`：按角色列出和查看草稿。
- `POST /tasks/{draftID}/submit`：仅 Alice，可提交自己的 draft。
- `POST /tasks/{draftID}/decision`：仅 Bob，可批准/拒绝分配给 Bob 的 pending 人工任务。
- 修改操作使用 CSRF Token；会话 Cookie 为签名值、`HttpOnly`、`SameSite=Lax`，有 TLS 时才设置 `Secure`。
- 密码启动时转换为 bcrypt Hash，明文从内存配置结构清除。

OA Demo 的用户与草稿都在内存中，重启即丢失；它只是协议演示，不是生产 OA。

## 数据源接口

Gateway 服务层使用：

```go
type DataConnector interface {
    Query(context.Context, dataconnector.QueryRequest) (dataconnector.Result, error)
    Ping(context.Context) error
}
```

`QueryRequest` 只包含经过 SQL 策略生成的 SQL、单次 `StatementTimeout` 与 `MaxRows`，刻意不提供客户端参数、DSN 或任意 Session 设置入口。结果包含逻辑列名、PostgreSQL类型 OID、二维行、行数、数据库耗时和截断标记。

内置 PostgreSQL Connector 使用 `pgx/v5`：

1. 从运行时 `POSTGRES_DSN` 建立最多 4 个连接的 Pool，并在启动时 Ping。
2. Connection Runtime Params 固定 `default_transaction_read_only=on`、`search_path=pg_catalog`、最大 `statement_timeout=5s`。
3. 每次 Query 再开启显式 `READ ONLY` 事务，并用事务本地 `set_config` 缩小超时、重设 `search_path`。
4. Connector 将请求行数限制压到自身 10,000 行硬上限以内；Demo 的任务 Profile 上限更低。
5. 错误对客户端只暴露稳定码，不回显 DSN、原始 SQL 或物理对象；内部 Cause 只供可信日志处理。

PostgreSQL 中的 `gateway_reader` 还有独立权限边界：只授予 `reporting.expense_summary` 与 `reporting.expense_detail` 的 `SELECT`，不授予 `legacy.*` 权限，并设置角色级只读和 5 秒超时。

## 新适配器要求

新增 OA Adapter 时：

- 保持 Draft 与回调的关联 ID、Catalog 版本、actor、审批凭证和幂等语义。
- 服务鉴权与回调验签使用独立凭据；不得信任浏览器字段决定敏感级别或预算。
- 回调先持久化再确认，支持重放、乱序、延迟和对账。

新增 Data Connector 时：

- 只接受策略产出的执行计划，禁止直接执行 Agent 输入。
- 独立实施只读事务/账号、Server-side Timeout、行上限和连接池上限。
- 返回稳定且不泄密的错误；不得把凭据、内部对象名或驱动诊断发给 MCP 客户端。
- 提供 Ready Ping 和取消传播；查询结果必须可确定地编码后加密。

当前 SQL 策略使用 PostgreSQL 专用 `pg_query_go/v6` AST 和 PostgreSQL CTE 渲染。支持 MySQL、SQL Server 等不仅是实现 `DataConnector`，还必须新增对应方言的 AST 策略、引用解析、Scope 注入和攻击语料测试。当前服务也只有一个 Connector，尚不支持多 Source 路由或跨源 Join。
