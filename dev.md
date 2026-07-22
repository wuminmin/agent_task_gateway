# Task-bound Agent Data Gateway 开发说明

## 目标与边界

本项目提供一个本地、单 Gateway 实例的数据访问 Demo。Agent 先显式申请目的、产品、字段、Scope、预算和期限，经 OA 审批后，才可提交声明式 QueryPlan 或 PostgreSQL `SELECT`。Gateway 内没有模型客户端、Prompt 或自然语言翻译器。

不建设新的 OA、BI 或数据仓库，不复制生产数据库，不安装 PostgreSQL 扩展，不管理 Agent Skill，也不负责图表渲染。

## Compose 拓扑

```text
docker compose
├── gateway:8082
├── oa-demo:8092
├── control-postgres:5432   -> host 127.0.0.1:25433
└── business-postgres:5432  -> internal business-data network only
```

- `control-postgres` 使用 `control-pg-data`，保存任务、Grant、预算、加密结果、回调幂等与审计链。
- `business-postgres` 使用 `business-pg-data`，保存 `travel_demo`、`legacy.*` 与 `reporting.*`。
- 两个 PG 的数据库、账号、密码、端口与 Volume 独立。
- 旧 `gateway-data` Volume 不再挂载，也不会自动删除。
- Gateway 与 OA 为非 root、只读根文件系统；宿主机入口只绑定回环地址。只有 Gateway 同时加入隔离的 `business-data` 网络；OA 和控制库没有到业务库的网络路由。`compose.debug.yaml` 是唯一发布业务库回环端口的、明确标记为非论文部署的 override。

系统控制库由 `gateway_control` 连接，DSN 通过 `CONTROL_POSTGRES_DSN` 注入。业务库由受限角色 `gateway_reader` 连接，DSN 通过 `POSTGRES_DSN` 注入。

## MCP 2.0 接口

查询者工具：

- `list_data_products()`
- `request_data_task(objective, data_products, columns, scopes, requested_budget?)`
- `list_my_tasks(state?, cursor?)`
- `get_task_status(task_id)`
- `wait_for_approval(task_id, timeout_seconds?)`
- `get_task_context(task_id)`
- `execute_plan(task_id, request_id, plan, output_format?)`
- `query_sql(task_id, request_id, sql)`
- `get_query_result(task_id, query_id)`
- `get_budget(task_id)`
- `list_receipts(task_id, cursor?)`
- `complete_task(task_id, summary?)`
- `revoke_task(task_id, reason)`

审计员工具：

- `list_audit_events(task_id?, actor?, event_type?, cursor?)`
- `get_audit_receipt(receipt_id)`

`serverInfo.version` 固定为 `2.0.0`。Alice 只能访问自己的任务与结果；Carol 只读审计凭证，不读取原始行。

### 任务申请

`objective`、`data_products`、`columns`、`scopes` 都是必填。每个产品必须有非空字段列表；Gateway 不自动选择全字段，也不推断缺失 Scope。Agent 应先从 `list_data_products` 读取 Scope 定义、枚举值和日期边界。

Catalog 决定敏感级别、审批路由、最大预算和 TTL。客户端 `requested_budget` 只能缩小 `max_queries` 与 `max_rows`，不能扩大 Profile 上限。

### QueryPlan

`internal/queryplan` 定义确定性结构：

```text
QueryPlan
├── product
├── columns[]
├── aggregates[{function,column,alias}]
├── filters[{column,op,value}]
├── group_by[]
├── order_by[{column,direction}]
└── limit
```

编译器验证产品、字段、聚合、别名、过滤运算符/literal、排序引用和 Limit。编译后的 SQL 仍必须通过完整 PostgreSQL AST 策略；`execute_plan` 不是策略旁路。

## 任务、审批与 Grant

```text
AWAITING_SUBMISSION --submitted--> AWAITING_APPROVAL
AWAITING_APPROVAL   --approved-->  ACTIVE
AWAITING_APPROVAL   --rejected-->  ARCHIVED(rejected)
ACTIVE              --complete-->  ARCHIVED(completed)
ACTIVE              --budget/TTL-> ARCHIVED(...)
```

Gateway 从认证主体构造版本化 `AuthorizationManifestV1`，其中包含产品、字段、Scope、敏感级别、全部预算、TTL、Agent/人类身份、Callback Context 以及 Catalog 版本和精确文件摘要。Gateway 与 OA 使用 RFC 8785 规范化和带域分隔的 SHA-256 重算 Manifest 摘要。OA 可批准、拒绝或收缩产品、字段、Scope、期限和预算；协议验证还保证敏感级别绝不提高。最终 `TaskGrantV1` 携带 OA Ed25519 `ApprovalReceiptV1`。回调 HMAC 只负责传输认证与防重放，回调还校验 Header/Body Event ID、双时间窗口、actor、状态和 callback context。

回调幂等、任务转换、Grant/预算创建位于同一控制 PG 事务，并对回调/任务行加锁。`task_grants` 的 UPDATE/DELETE 由 PostgreSQL Trigger 拒绝。

## 控制 PostgreSQL

启动入口 `control.Open` 使用 `database/sql` 与 pgx stdlib 驱动：

1. 连接 `CONTROL_POSTGRES_DSN` 并 Ping。
2. 用事务 advisory lock 串行化嵌入式迁移。
3. 应用 PostgreSQL Schema。
4. 执行中断查询、未完成回调和过期任务恢复。

Schema 类型约定：

- 时间：`TIMESTAMPTZ`，应用写入 UTC 微秒精度。
- 结构化内容：`JSONB`。
- 密文、Nonce、回调响应：`BYTEA`。
- 预算、计数、审计序号：`BIGINT`。
- 参数：PostgreSQL `$1`、`$2` 占位符。

关键并发规则：

- 状态转换锁定任务行。
- 回调 Claim/完成锁定回调行，审批处理同时锁定任务行。
- 预算预留和结算锁定任务、预算及查询记录。
- 结果落库与结算在同一控制事务中完成。
- 审计追加先 `SELECT ... FOR UPDATE` 锁定单行 `audit_chain_head`，在同一事务生成连续序号、写事件和更新链头。

Gateway 当前仍只允许单实例部署。行锁保证请求并发安全，但业务 SQL 执行跨越控制库事务边界；多实例必须先实现分布式任务租约。

## SQL 与数据面

`query_sql` 和 QueryPlan 编译结果共用以下链路：

```text
所有权 / ACTIVE / Grant / TTL / Catalog 与实时 Schema Attestation
  -> pg_query_go/v6 AST 白名单
  -> 获批字段与 Scope CTE
  -> 外层剩余行数 LIMIT
  -> 控制 PG 按 (task_id, request_id) 幂等预留
  -> 业务 PG READ ONLY + statement_timeout
  -> 控制 PG 结算、密文结果、Ed25519 QueryReceiptV1、审计事件
```

Agent SQL 只能引用未限定的逻辑产品名。物理 `reporting.*`、`legacy.*`、系统对象、多语句、写操作、递归 CTE、锁、通配符、危险函数、参数和未知 AST 特性均关闭式拒绝。

业务数据库还有独立防线：

- `gateway_reader` 仅拥有两个 Reporting View 的 `SELECT`。
- 角色显式为非 owner、非 superuser、无 `BYPASSRLS`，角色、连接和事务均为只读。
- `search_path=pg_catalog`。
- Connector 超时和 10,000 行硬上限。
- Catalog 固定 Reporting View 的列顺序和 PostgreSQL 类型；启动、readiness 和新查询前的 Attestation 检测漂移并关闭式拒绝。

## 加密、预算与恢复

结果编码为 JSON 后使用 AES-256-GCM 加密；随机 Nonce 和 Ciphertext 存入 `BYTEA`。AAD 绑定 `task_id/query_id`，另存明文 SHA-256 并在读取时验证。

查询执行前预留 1 次查询、剩余允许行数和 DB 时间，同一任务同时最多有一个预留。必填 `request_id` 在任务内唯一；同 ID、同摘要只观察首次持久状态，同 ID、不同摘要关闭式拒绝。结算释放未使用额度，达到任何硬上限后归档为 `budget_exhausted`。运行期结算失败使 readiness 返回失败，后台幂等重试。

启动恢复会：

- 将 RESERVED 查询按完整预留保守计费并标记 `INDETERMINATE`，该 `request_id` 不得自动重执行。
- 将 PROCESSING 回调改为 `RETRYABLE`。
- 归档已过期任务。

## 代码结构

```text
cmd/gateway             HTTP、MCP、依赖组装与 readiness
cmd/oa-demo             最小 OA 页面与签名回调
cmd/mcp-probe           官方 MCP SDK 协议探针
internal/catalog        YAML 解析、边界与授权解析
internal/control        PostgreSQL 控制存储、迁移、锁、加密与审计
internal/queryplan      声明式 QueryPlan 与确定性编译
internal/sqlpolicy      PostgreSQL AST 策略与 Scope 渲染
internal/dataconnector  只读 pgx 业务连接器
internal/gateway        MCP 工具、任务、查询与 OA 回调
internal/testpostgres   每测试独立 Schema 的真实 PG Fixture
db/control-init         控制库应用角色初始化
db/init                 业务演示 Schema、View 与只读角色
scripts                 Compose/race/E2E 验收
```

## 配置

仓库只提交 `.env.example`，本地 `.env` 不纳入版本控制。核心变量：

| 领域 | 变量 |
|---|---|
| 控制库 | `CONTROL_POSTGRES_PORT`、`CONTROL_POSTGRES_DB`、`CONTROL_POSTGRES_ADMIN_PASSWORD`、`CONTROL_DB_PASSWORD` |
| 业务库 | `POSTGRES_PORT`、`POSTGRES_DB`、`POSTGRES_USER`、`POSTGRES_PASSWORD`、`GATEWAY_DB_PASSWORD` |
| Gateway/OA | `GATEWAY_DATA_KEY`、Gateway/OA Ed25519 Key ID 与密钥、Alice/Carol Token、OA Token、Callback HMAC Secret |

Compose 内部生成 `CONTROL_POSTGRES_DSN` 和 `POSTGRES_DSN`；直接启动二进制时必须显式设置这两个 DSN。

## 测试与验收

统一命令：

```bash
make verify
```

验证包含：

- Docker 构建阶段的 `gofmt`、`go vet`、编译与无需数据库的测试。
- 隔离 Compose 控制/业务 PG。
- `CONTROL_TEST_POSTGRES_DSN` 下每测试创建唯一 Schema，测试完成后 `DROP ... CASCADE`。
- 真实 PG 的迁移、重启恢复、密文、不可变 Trigger、回调重放、并发预算、审计链并发与故障 Trigger。
- `go test -race ./...`。
- 官方 MCP Client、自动/人工审批、拒绝、预算耗尽、Gateway 重启、身份隔离与业务账号最小权限 E2E。
- 默认只发布控制 PG 的回环端口；集成测试和显式 `compose.debug.yaml` 调试会临时发布业务 PG 回环端口。

不要让本地单元测试共享固定 Schema；这会使并行测试互相污染。数据库故障注入使用临时 PostgreSQL PL/pgSQL Trigger，并在测试清理时删除。

## 数据生命周期

`docker compose down` 保留两个当前 PG Volume；`docker compose down --volumes` 删除它们。旧版遗留的 `gateway-data` Volume 不再由当前 Compose 管理，不应在迁移时自动删除。系统控制数据不从旧存储自动导入。
