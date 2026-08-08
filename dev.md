# Task-bound Agent Data Gateway 开发说明

## 目标与边界

本项目提供一个本地、单 Gateway 实例的数据访问 Demo。Agent 先显式申请目的、产品、字段和 Scope；Gateway 绑定 Catalog 预定义的完整预算与期限，经 OA 审批后，普通 Agent 使用 PostgreSQL 报表 `SELECT`。启用 exposure 时，Gateway 先将 SQL 无损 lowering 为 canonical QueryPlan，再进入唯一可信执行与记账链路。Gateway 内没有模型客户端、Prompt 或自然语言翻译器。

不建设新的 OA、BI 或数据仓库，不复制生产数据库，不安装 PostgreSQL 扩展，不管理 Agent Skill，也不负责图表渲染。

## Compose 拓扑

```text
docker compose
├── gateway:8082
├── oa-demo:8092
├── control-postgres:5432   -> host 127.0.0.1:25433
├── business-postgres:5432  -> internal business-data network only
├── result-object-store:9000 -> internal result-storage network only
└── result-object-store-init -> private bucket + bucket-scoped user
```

- `control-postgres` 使用 `control-pg-data`，保存任务、Grant、预算、result artifact 元数据、回调幂等与审计链；不保存 Parquet bytes 或结果行。
- `business-postgres` 使用 `business-pg-data`，保存 `travel_demo`、`legacy.*` 与 `reporting.*`。
- `result-object-store` 使用 `result-object-data`，保存 Gateway 客户端侧加密的 staging 与规范 Parquet 对象。bucket 必须私有且禁用 versioning，否则删除可能只产生 delete marker，无法实现 TTL/purge 的实体删除语义。
- 两个 PG 的数据库、账号、密码、端口与 Volume 独立。
- 旧 `gateway-data` Volume 不再挂载，也不会自动删除。
- Gateway 与 OA 为非 root、只读根文件系统；宿主机入口只绑定回环地址。只有 Gateway 同时加入隔离的 `business-data` 网络；OA 和控制库没有到业务库的网络路由。`compose.debug.yaml` 是唯一发布业务库回环端口的、明确标记为非论文部署的 override。

系统控制库由 `gateway_control` 连接，DSN 通过 `CONTROL_POSTGRES_DSN` 注入。业务库由受限角色 `gateway_reader` 连接，Gateway 按 Catalog Source 和 `secretRef` 构造 DSN，并校验业务库内的 `datasource_id`。

## MCP 2.0 接口

查询者工具：

- `list_data_products()`
- `describe_data_product(name)`
- `get_sql_capabilities()`
- `request_data_task(objective, data_products, columns, scopes)`
- `list_my_tasks(state?, cursor?)`
- `get_task_status(task_id)`
- `wait_for_approval(task_id, timeout_seconds?)`
- `get_task_context(task_id)`
- `query_sql(task_id, request_id, sql)`
- `get_query_result(task_id, query_id)`
- `preview_result(result_id, offset?, limit?)`
- `deliver_result(result_id, format="parquet")`
- `get_budget(task_id)`
- `list_receipts(task_id, cursor?)`
- `complete_task(task_id, summary?)`
- `revoke_task(task_id, reason)`

`execute_plan(task_id, request_id, plan, output_format?)` 保留为 SDK、内部测试、基准、调试和确定性工作流的高级入口，不在普通 query Principal 的 `tools/list` 中默认列出。它仍进入与 `query_sql` lowering 相同的 QueryPlan 编译、SQL policy、provenance 和结算链路。

审计员工具：

- `list_audit_events(task_id?, actor?, event_type?, cursor?)`
- `get_audit_receipt(receipt_id)`

`serverInfo.version` 固定为 `2.1.0`。Alice 只能访问自己的任务与结果；Carol 只读审计凭证，不读取原始行。

### 任务申请

`objective`、`data_products`、`columns`、`scopes` 都是必填。每个产品必须有非空字段列表；Gateway 不自动选择全字段，也不推断缺失 Scope。Agent 应先从 `list_data_products` 读取 Scope 定义、枚举值和日期边界，必要时用 `describe_data_product` 读取字段 collation、Catalog stable role 和允许函数/聚合/运算符，并用 `get_sql_capabilities` 读取 `taskgate-reporting-sql-v1` 边界。

Catalog 决定敏感级别、审批路由、完整预算和 TTL。Agent 不提交预算：Gateway 选择对应 Profile 并把全部额度绑定到审批 Manifest。OA 的显式人工收缩仍属于授权决定，不能由 Agent 或运行时校准逻辑触发。

### QueryPlan

`internal/queryplan` 定义确定性结构：

```text
QueryPlan
├── product
├── from.scan | from.join_many[sources<=16] | from.union_distinct
├── columns[]
├── aggregates[{function,column,alias}]
├── filters[{column,op,value}]
├── group_by[]
├── order_by[{column,direction}]
└── limit
```

编译器验证产品、字段、聚合、别名、过滤运算符/literal、排序引用和 Limit。SQL lowering 先将 alias 映射为 Catalog 稳定角色，再构造 2–16 源的 connected INNER equi-join graph；graph 可为任意形状，每条 edge 包含一个或多个 column-to-column equality predicate。Nodes、edges 和 predicates 规范排序后转换为现有 `join_many`，再 deterministic binary fold 为现有二元代数，并验证 join key 类型、collation 和版本。`MaxJoinSources=16` 是限制生成 SQL、provenance 和 PostgreSQL planning work 的 operational complexity/DoS ceiling，因此 10 表 Join 在受支持范围内；请求仍受 1 MiB MCP 请求体、AST 白名单校验和现有资源预算/超时/行数上限约束。编译后的 SQL 仍必须通过完整 PostgreSQL AST 策略；`execute_plan` 不是策略旁路。

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
- artifact schema/ACL/摘要：`JSONB`；对象键、双哈希、key ID、ETag 和生命周期状态是元数据。
- `BYTEA` 只继续用于回调证据和迁移期 legacy 密文兼容，不用于新 Parquet 结果。
- 预算、计数、审计序号：`BIGINT`。
- 参数：PostgreSQL `$1`、`$2` 占位符。

关键并发规则：

- 状态转换锁定任务行。
- 回调 Claim/完成锁定回调行，审批处理同时锁定任务行。
- 预算预留和结算锁定任务、预算及查询记录。
- 正常 Gateway 生产路径把资源/exposure 结算、`PENDING` artifact 元数据、V8 回执和审计放在同一控制事务中；规范对象在 commit 后幂等提升。
- 审计追加先 `SELECT ... FOR UPDATE` 锁定单行 `audit_chain_head`，在同一事务生成连续序号、写事件和更新链头。

V8 有两种持久化时序语义。正常路径产生 **co-committed V8**，即回执与上述
settlement evidence 同事务提交。兼容恢复路径只在 query/artifact/registration
audit 已存在而 receipt 行缺失时产生 **recovered V8 attestation**：Gateway 先验证
原始 registration event，再对不可变的历史证据补签，因此它的 `signed_at` 可以晚于
原 settlement，不能表述为“该 Receipt 本身与 settlement 同时提交”。两者验证同一
PENDING intent，均不证明 AVAILABLE；正常 Compose 路径只走前一种。

Gateway 当前仍只允许单实例部署。行锁保证请求并发安全，但业务 SQL 执行跨越控制库事务边界；多实例必须先实现分布式任务租约。

## SQL 与数据面

`query_sql` 的入口分流和统一执行链路如下：

```text
所有权 / ACTIVE / Grant / TTL / Catalog 与实时 Schema Attestation
  -> 所有 Grant: taskgate-reporting-sql-v1 AST -> canonical QueryPlan
  -> 从 QueryPlan 重新生成 visible SQL + provenance companion
  -> 生成 SQL 再经 pg_query_go/v6 AST 白名单
  -> 获批字段与 Scope CTE
  -> 外层剩余行数 LIMIT
  -> 控制 PG 按 (task_id, request_id) 幂等预留
  -> exposure miss: 业务 PG 同快照 READ ONLY + statement_timeout
  -> provenance / V4 ordinal / 三维根账本结算
  -> Gateway 生成 Parquet，分块 AES-GCM 加密并上传 private staging
  -> 控制 PG 结算、PENDING artifact 元数据、V6 Receipt、审计事件
  -> 幂等创建 canonical 对象 -> consumed/AVAILABLE
  -> 普通响应仅 result_id + schema/行列数/TTL/计费摘要
```

Agent SQL 只能引用未限定的逻辑产品名。物理 `reporting.*`、`legacy.*`、系统对象、多语句、写操作、递归 CTE、锁、通配符、危险函数、参数和未知 AST 特性均关闭式拒绝。Exposure SQL 还必须能无损 lowering；Gateway 不会通过删除 predicate、忽略 alias 或把 outer join 改为 inner join 来“修复”查询。

SQL 解析、授权或 QueryPlan lowering 失败在 connector 和 `ReserveBudget` 之前返回结构化、可修复的错误；它们不扣 queries/rows/DBMS，也不扣 release/dependency/outcome。原始 SQL 派生的 request digest 只用于审计和幂等，FactID 与 semantic replay 使用 canonical QueryPlan/`plan_digest`。数据库已开始执行后的 timeout 或故障继续使用现有 failure-settlement 规则。

业务数据库还有独立防线：

- `gateway_reader` 仅拥有 Datasource Attestation 表和两个 Reporting View 的 `SELECT`。
- 角色显式为非 owner、非 superuser、无 `BYPASSRLS`，角色、连接和事务均为只读。
- `search_path=pg_catalog`。
- Connector 超时和 10,000 行硬上限。
- Catalog 固定 Reporting View 的列顺序和 PostgreSQL 类型；启动、readiness 和新查询前的 Attestation 检测漂移并关闭式拒绝。

## 加密、预算与恢复

最终可见投影编码为 Parquet，Gateway 在上传前使用分块 AES-256-GCM 加密，AAD 绑定 task/result/frame ordinal。加密 staging 和规范原件只保存在 TaskGate 管辖的 S3/MinIO 私有 bucket；Control PG 只保存 `result_id`、对象位置、schema、行列数、ACL、TTL、哈希、ETag、key ID、Receipt 和状态。明文 Parquet SHA-256 与密文对象 SHA-256 在读取/提升时重验。

规范对象成功创建就是完整结果的消费/释放边界；`AVAILABLE/consumed_at` 是该事件的持久状态。Agent 是否调用 `preview_result`、是否调用 `deliver_result`、是否真的下载或下载多少次，都不会再计费、撤销计费或改写 Receipt。`deliver_result` 返回的是 Gateway 签名短 TTL capability URL，由 Gateway 鉴权并流式解密，不是 S3 对象直链。非 loopback 的下载基址必须使用 HTTPS；运维、反向代理和 APM 必须对 URL 查询参数中的 `token` 脱敏。

查询执行前预留 1 次查询、剩余允许行数和 DB 时间，同一任务同时最多有一个预留。必填 `request_id` 在任务内唯一；同 ID、同摘要只观察首次持久状态，同 ID、不同摘要关闭式拒绝。结算释放未使用额度，达到任何硬上限后归档为 `budget_exhausted`。运行期结算失败使 readiness 返回失败，后台幂等重试。

启动恢复会：

- 将 RESERVED 查询按完整预留保守计费并标记 `INDETERMINATE`，该 `request_id` 不得自动重执行。
- 扫描结算已提交但 canonical promotion 未完成的 `PENDING` artifacts，在 staging 与已提交哈希/ETag 证据一致时，使用确定性对象键幂等恢复为 `AVAILABLE/consumed`；不重执行 SQL、不退款、不生成第二份 Receipt。后台 sweep 会继续恢复。staging 丢失或 canonical 证据冲突时不会自动放弃/退款，readiness 必须 fail closed，直到运维恢复正确对象证据或执行受审修复。
- 将 PROCESSING 回调改为 `RETRYABLE`。
- 归档已过期任务。

artifact 到达自身 `expires_at` 后，metadata lookup、preview 和 delivery 都关闭。TTL 或管理员 purge 先删除 canonical bytes，再将 Control 元数据置为删除终态；查询、Receipt 与审计保留。active legal hold 阻止 TTL/手动 retention 清理，但不延长 artifact `expires_at`、不继续开放读取，也不替代独立的 key-erasure 审批。

staging 不能配置一刀切的按时间 bucket lifecycle，因为结算已提交的 `PENDING` intent 可能仍依赖旧 staging 恢复。后台 orphan sweeper 在 `GATEWAY_RESULT_STAGING_ORPHAN_TTL` 宽限后查询 Control，只删除无 PENDING/可恢复 query 引用的 staging；同样按确切 upload ID abort 超过宽限的旧 S3 multipart fragments，不会影响仍可恢复的 promotion。

当前仍是研究原型：Connector/可见结果到 Parquet 的部分路径仍会在内存中持有完整结果，不是全程有界流式管线。`preview_result` 每次最多 100 行，且默认拒绝读取密文大于 64 MiB 的 artifact（可由 `GATEWAY_RESULT_PREVIEW_MAX_BYTES` 调整）。生产级百万行路径还需要有界流式 Parquet writer/reader、容量基准和独立并发配额。

## 代码结构

```text
cmd/gateway             HTTP、MCP、依赖组装与 readiness
cmd/oa-demo             最小 OA 页面与签名回调
cmd/mcp-probe           官方 MCP SDK 协议探针
internal/catalog        YAML 解析、边界与授权解析
internal/control        PostgreSQL 控制存储、迁移、锁、加密与审计
internal/resultartifact Parquet 编码、分块加密、S3/MinIO 对象与完整性验证
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
| 结果对象存储 | `GATEWAY_OBJECT_STORE_ENDPOINT`、`GATEWAY_OBJECT_STORE_REGION`、`GATEWAY_OBJECT_STORE_BUCKET`、`GATEWAY_OBJECT_STORE_ACCESS_KEY`、`GATEWAY_OBJECT_STORE_SECRET_KEY`、`GATEWAY_OBJECT_STORE_PATH_STYLE` |
| 结果交付/保留 | `GATEWAY_PUBLIC_BASE_URL`、`GATEWAY_DELIVERY_SIGNING_KEY`、`GATEWAY_RESULT_DELIVERY_TTL`、`GATEWAY_RESULT_DOWNLOAD_TIMEOUT`、`GATEWAY_RESULT_DOWNLOAD_CONCURRENCY`、`GATEWAY_RESULT_RETENTION_TTL`、`GATEWAY_RESULT_PROMOTION_TIMEOUT`、`GATEWAY_RESULT_STAGING_ORPHAN_TTL`、`GATEWAY_RESULT_TEMP_DIR`、`GATEWAY_RESULT_PREVIEW_MAX_BYTES` |
| Gateway/OA | `GATEWAY_DATA_KEY`、Gateway/OA Ed25519 Key ID 与密钥、Alice/Carol Token、OA Token、Callback HMAC Secret |

Compose 内部生成 `CONTROL_POSTGRES_DSN`，业务库连接由 `config/catalog.yaml` 的 Source 字段和 `GATEWAY_DB_PASSWORD` 构造；直接启动二进制时必须设置控制库 DSN 以及 Catalog `secretRef` 指向的密码环境变量。

## 测试与验收

统一命令：

```bash
make verify
```

验证包含：

- Docker 构建阶段的 `gofmt`、`go vet`、编译与无需数据库的测试。
- 隔离 Compose 控制/业务 PG。
- `CONTROL_TEST_POSTGRES_DSN` 下每测试创建唯一 Schema，测试完成后 `DROP ... CASCADE`。
- 真实 PG 的迁移、重启恢复、artifact 元数据不可变 Trigger、回调重放、并发预算、审计链并发与故障 Trigger。
- 对象存储的 private bucket、加密 Parquet 往返、PENDING promotion、幂等重试、preview/delivery 和下载不改写计费状态。
- `go test -race ./...`。
- 官方 MCP Client、自动/人工审批、拒绝、预算耗尽、Gateway 重启、身份隔离与业务账号最小权限 E2E。
- 默认只发布控制 PG 的回环端口；集成测试和显式 `compose.debug.yaml` 调试会临时发布业务 PG 回环端口。

不要让本地单元测试共享固定 Schema；这会使并行测试互相污染。数据库故障注入使用临时 PostgreSQL PL/pgSQL Trigger，并在测试清理时删除。

## 数据生命周期

`docker compose down` 保留两个 PG Volume、snapshot artifacts、加密 spool 和 `result-object-data`；`docker compose down --volumes` 删除当前 Compose 项目的这些 Volume，包括规范加密 Parquet。旧版遗留的 `gateway-data` Volume 不再由当前 Compose 管理，不应在迁移时自动删除。系统控制数据不从旧存储自动导入。
