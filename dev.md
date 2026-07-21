# Task-bound Agent Data Gateway 全新项目实施方案

## 1. 项目目标与边界

在一个空目录中全新创建项目：

- 项目名：`taskbound-agent-data-gateway`
- Go Module：`taskbound.local/agent-data-gateway`
- 分支：`main`
- 不复制旧代码、文档、数据库扩展或 Git 历史。
- 执行 `git init -b main`，不配置远程仓库。
- 使用 Go 1.25，宿主机只要求 Docker 与 Docker Compose，不要求安装 Go。

产品定位：

```text
本地 Codex / 第三方 Agent
            │ MCP
            ▼
Task-bound Agent Data Gateway
    ├── 任务边界与预算
    ├── OA 审批编排
    ├── SQL 安全审计
    ├── 只读数据访问
    └── 查询结果与审计凭证
```

明确不做：

- 不建设新的 OA、BI、数据仓库或独立智能看数前端。
- 不安装 PostgreSQL 扩展、Hook 或 `shared_preload_libraries`。
- 不同步、复制或改造企业生产数据库。
- 不在 Gateway 内管理 Agent Skill；Skill 由 Codex 等 Agent 平台管理和版本化。
- 不负责图表渲染；Gateway 返回结构化数据，Codex 和 Skill 决定分析、排版及图表。
- Demo 只实现 PostgreSQL 和最小 OA 适配器，但保留其他数据库、OA、CRM 的接口扩展点。

## 2. 架构与完整业务流程

### Docker Compose 服务

```text
docker compose
├── gateway:8080       Go Gateway、MCP、策略、DeepSeek、审计
├── oa-demo:8090       Go 最小 OA 登录与审批页面
└── postgres:5432      普通 PostgreSQL 16 业务数据库
```

持久化：

- Gateway 使用内嵌 SQLite 保存控制数据，挂载 `gateway-data` Volume。
- PostgreSQL 使用独立 `pg-data` Volume，只存演示业务数据。
- SQLite 开启 WAL、外键和事务；Gateway v1 只运行一个实例。
- 查询结果使用 AES-256-GCM 加密后保存，密钥由 `GATEWAY_DATA_KEY` 注入。
- 所有密码、Token、DeepSeek Key 放入 Git 忽略的 `.env`，仓库只提供 `.env.example`。

### Codex 接入

项目生成 `.codex/config.toml`：

```toml
[mcp_servers.taskbound_gateway]
url = "http://127.0.0.1:8080/mcp"
bearer_token_env_var = "TASKBOUND_GATEWAY_TOKEN"
required = true
startup_timeout_sec = 10
tool_timeout_sec = 120
```

Demo 提供两个 Gateway 身份：

- Alice：任务申请者、查询者，只能访问自己的任务、结果和凭证。
- Carol：审计员，只能读取审计事件和查询凭证，默认不能读取原始敏感结果。

Bob 是 OA 审批人，不通过 MCP 查询数据。

### 任务生命周期

```text
AWAITING_SUBMISSION
        │ Alice 在 OA 提交
        ▼
AWAITING_APPROVAL
        │ 自动审批或 Bob 人工审批
        ▼
ACTIVE
        │ 完成、预算耗尽、过期或撤销
        ▼
ARCHIVED
```

归档任务保留 `terminal_reason`：

- `completed`
- `budget_exhausted`
- `rejected`
- `expired`
- `revoked`
- `failed`

每次状态变化写入不可更新的审计事件，并建立逐事件 SHA-256 Hash Chain。

### 客户旅程

1. Alice 在本地 Codex 提出数据任务。
2. Codex 调用 Gateway 的 `request_data_task`。
3. DeepSeek 只把自然语言转换为候选 `TaskIntent`。
4. Gateway 根据 YAML 目录确定数据产品、敏感级别、审批方式、预算和期限。
5. Gateway 调用 OA Demo 创建草稿，并向 Codex返回 OA URL。
6. Alice 登录 OA 并提交：
   - 汇总任务：OA 自动批准。
   - 员工明细任务：进入 Bob 的审批队列。
7. OA 使用签名回调通知 Gateway。
8. Codex通过 `wait_for_approval` 短轮询；若会话已结束，用户回到原线程说“继续”。
9. ACTIVE 状态下，Codex调用自然语言查询或直接 SQL 查询。
10. Gateway 审计 SQL、执行只读查询、扣减预算、保存结果和凭证。
11. Agent 调用 `complete_task`，或预算/期限触发终止，Gateway 自动归档。

Gateway不承诺向已关闭的 Codex 会话主动推送消息。

## 3. 核心接口与数据契约

### MCP 工具

查询者工具：

- `list_data_products()`：列出逻辑数据产品、字段、说明和敏感等级。
- `request_data_task(objective, data_products?, requested_budget?)`：创建任务及 OA 草稿。
- `list_my_tasks(state?, cursor?)`
- `get_task_status(task_id)`
- `wait_for_approval(task_id, timeout_seconds)`：最长等待45秒。
- `get_task_context(task_id)`：返回批准的数据范围、预算和期限。
- `query_data(task_id, question, output_format?)`：自然语言查询。
- `query_sql(task_id, sql)`：专家或 Skill 直接提交 PostgreSQL SELECT。
- `get_query_result(task_id, query_id)`
- `get_budget(task_id)`
- `list_receipts(task_id, cursor?)`
- `complete_task(task_id, summary?)`

审计员工具：

- `list_audit_events(task_id?, actor?, event_type?, cursor?)`
- `get_audit_receipt(receipt_id)`

所有工具返回结构化 JSON，包含 `trace_id`。策略拒绝返回稳定错误码和安全说明，不泄露物理表名、DSN、密码或内部 Token。

### TaskGrant

审批通过后生成不可扩权的任务授权：

```text
TaskGrant
├── task_id
├── subject
├── purpose
├── approved_products
├── approved_columns
├── mandatory_scope
├── sensitivity_ceiling
├── budget
├── expires_at
├── catalog_version
└── approval_receipt
```

后续每次查询只能缩小 TaskGrant，不能扩大数据产品、字段、范围、期限或预算。

### DeepSeek 边界

- 使用官方 DeepSeek API，默认模型由 `DEEPSEEK_MODEL=deepseek-v4-flash` 配置。
- DeepSeek 只接收用户问题和脱敏后的 YAML 逻辑目录，不接收数据库密码或查询结果。
- 自然语言任务输出严格 `TaskIntent` JSON。
- 自然语言查询输出严格 `QueryPlan` JSON，不直接执行模型生成的 SQL。
- 本地校验失败后只允许一次 JSON 修复重试，仍失败则关闭式拒绝。
- 敏感等级、审批路由、授权、预算和 SQL 放行永远由 Go 确定性代码决定。
- DeepSeek 不可用时，自然语言工具返回 `MODEL_UNAVAILABLE`；策略、OA、审计和已批准任务的直接 SQL 路径不依赖 DeepSeek。

### YAML 数据目录

配置包含：

- `sources`：PostgreSQL 地址、端口、数据库和 `secretRef`，禁止明文密码。
- `products`：逻辑名称、物理 Reporting View、字段、类型、说明和敏感等级。
- `scopes`：允许的部门、日期等任务范围。
- `approval_routes`：按最高敏感等级决定自动或人工审批。
- `budget_profiles`：查询数、累计行数、数据库时间、单次超时和任务 TTL。
- `catalog_version`：写入任务与查询凭证，保证配置变更可追溯。

启动时完整校验 YAML；配置非法时 Gateway 拒绝进入 Ready 状态。任务解析只依赖内存中的 YAML，不查询 PostgreSQL 元数据。

### SQL 安全模型

使用 `pg_query_go/v6` 对 SQL 做 PostgreSQL AST 解析，禁止正则表达式充当安全判断。

允许：

- 单条 `SELECT`
- 非递归 `WITH ... SELECT`
- YAML 允许的函数、运算和聚合
- 已批准的逻辑数据产品和字段

拒绝：

- 多语句、DDL、DML、`COPY`、`CALL`、`DO`、`SET`
- `SELECT INTO`、锁定语句、递归 CTE
- `pg_catalog`、`information_schema`、物理 Schema 和未发布对象
- `SELECT *`
- 文件、网络、管理、睡眠和未列入白名单的函数
- 未批准字段、数据产品或跨任务访问
- 参数占位符和客户端自定义会话变量

执行前为每个逻辑数据产品生成受 TaskGrant 约束的 CTE：

```sql
WITH expense_detail AS (
  SELECT <approved_columns>
  FROM reporting.expense_detail
  WHERE <mandatory_scope>
)
SELECT *
FROM (
  <validated_agent_select>
) AS __taskbound_result
LIMIT <remaining_row_budget>;
```

Agent SQL 只能引用逻辑 CTE 名称，不能引用物理 View。执行使用 `pgx/v5`、只读事务、只读数据库角色、`statement_timeout` 和剩余行数限制。

### OA 接口

Gateway 调用：

```text
POST http://oa-demo:8090/api/drafts
Authorization: Bearer <service-token>
```

Gateway传入任务摘要、敏感级别、审批模式和公共回调信息。OA 不判断数据敏感性。

OA 回调：

```text
POST http://gateway:8080/api/v1/oa/callback
X-OA-Event-ID
X-OA-Timestamp
X-OA-Signature: v1=<hmac>
```

签名算法固定为：

```text
HMAC-SHA256(secret, timestamp + "." + raw_request_body)
```

Gateway拒绝超过5分钟的时间戳，按 `event_id` 幂等处理重放。

### 演示数据与审批规则

PostgreSQL 初始化确定性差旅报销测试数据：

- `legacy.*`：员工和报销业务表，Gateway角色无访问权限。
- `reporting.expense_summary`：
  - 月份、部门、费用类型、总金额、申请数。
  - 低敏汇总数据。
  - Alice 提交 OA 后自动审批。
- `reporting.expense_detail`：
  - 报销单号、员工编号、姓名、部门、日期、类型、金额、城市、用途、状态。
  - 员工级明细数据。
  - 必须由 Bob 人工批准。
- 手机号、工资、银行卡等字段不进入 Reporting View，也不进入 YAML 目录。

预算默认值：

| 类型 | 查询次数 | 累计行数 | 累计DB时间 | 单次超时 | TTL |
|---|---:|---:|---:|---:|---:|
| 汇总自动审批 | 10 | 500 | 30秒 | 5秒 | 30分钟 |
| 明细人工审批 | 5 | 100 | 15秒 | 5秒 | 15分钟 |

同一任务查询串行执行，预算在 SQLite 事务中预留和结算，避免并发超支。合法查询达到硬上限后先返回本次允许范围内的结果，再自动归档为 `budget_exhausted`。

## 4. 实现结构与提交顺序

### Go 组件

两个可执行程序：

- `gateway`：HTTP、MCP、任务状态机、策略、DeepSeek、SQLite、PostgreSQL、OA适配器。
- `oa-demo`：服务端 HTML、登录、草稿提交、自动审批、Bob审批及签名回调。

核心接口：

- `Translator`
- `CatalogProvider`
- `PolicyEngine`
- `ApprovalAdapter`
- `DataConnector`
- `ControlStore`
- `ResultCipher`
- `Clock`

使用标准库 `net/http`、`html/template`、`log/slog`，HTTP 路由使用轻量 `chi`。SQLite 通过 `database/sql`，迁移脚本嵌入二进制。

OA Demo 使用服务端页面，不引入 React/Vue。登录 Cookie 设置 `HttpOnly`、`SameSite=Lax`，修改操作使用 CSRF Token，密码只保存 Hash。

容器使用多阶段构建：

- Go Builder 中启用 cgo 编译 `pg_query_go`。
- 运行阶段使用精简 Debian 镜像，非 root 用户运行。
- Gateway 与 OA 容器启用只读根文件系统，只给数据目录和 `/tmp` 写权限。
- Gateway、OA、PostgreSQL均配置 Healthcheck。
- Gateway端口和 OA 端口只绑定 `127.0.0.1`。

### SQLite 数据

至少包含：

- principals
- tasks
- task_grants
- approval_events
- budget_ledger
- query_records
- encrypted_query_results
- audit_events
- callback_idempotency

查询凭证记录：

- task/query/actor
- 原始请求摘要
- SQL 指纹
- catalog version
- 结果行数与耗时
- 扣减前后预算
- 结果 SHA-256
- 策略判定
- 时间戳、前序 Hash 和当前 Hash

### Git 提交顺序

每个提交必须能够构建并通过当阶段测试：

1. `chore: initialize clean Go and Compose project`
2. `feat: add domain model, YAML catalog and task state machine`
3. `feat: add SQLite control store, budgets and audit chain`
4. `feat: add PostgreSQL connector and AST SQL policy`
5. `feat: add DeepSeek structured translation`
6. `feat: add OA demo and signed approval callbacks`
7. `feat: expose authenticated MCP tools for Codex`
8. `test: add end-to-end demo scenarios and documentation`

最终工作区必须干净，无远程仓库，无旧资产。

## 5. 测试、验收与交付

### 自动化测试

单元测试：

- YAML 缺字段、重复产品、非法 View、明文密码时启动失败。
- DeepSeek 合法 JSON、非法 JSON、一次修复及失败关闭。
- 敏感等级合并时采用最高等级。
- 状态机拒绝越级、重复及过期回调。
- HMAC 错误、超时和重放攻击。
- 预算并发、行数封顶、超时及自动归档。
- SQL 攻击语料：多语句、注释绕过、写操作 CTE、系统表、危险函数、通配符和越权字段。
- SQLite重启恢复、结果加解密和审计 Hash Chain 校验。

Compose 集成测试：

1. 汇总任务提交后自动审批并成功查询。
2. 明细任务在 Bob 批准前查询被拒绝。
3. Bob 批准后明细查询成功。
4. Bob 拒绝后任务归档且永远不可查询。
5. 连续查询耗尽预算后自动归档。
6. Gateway重启后任务、预算、结果和审计记录仍存在。
7. `gateway_reader` 对 `legacy.*` 和写操作没有权限。
8. 错误 Bearer Token 无法初始化 MCP。
9. Alice 无法读取其他主体任务，Carol只能读取审计凭证。
10. DeepSeek 不可用时自然语言失败，但确定性组件保持可用。

使用官方 Go MCP Client 对 `/mcp` 做协议级集成测试，不把真实 Codex 会话作为自动测试依赖。

统一验证命令：

```bash
make verify
```

该命令在 Docker 中完成格式检查、静态检查、单元测试、Compose 集成测试和镜像构建。

### 人工验收流程

```bash
cp .env.example .env
# 填入 DEEPSEEK_API_KEY，并生成本地演示密钥
docker compose up --build -d

export TASKBOUND_GATEWAY_TOKEN=<Alice token>
codex
```

Codex测试提示：

1. “申请查询销售部各月份差旅报销总额，并等待审批。”
2. 打开 OA URL，以 Alice 登录并提交；确认自动批准。
3. “继续，按月份查询并分析变化。”
4. “申请查询销售部员工报销明细。”
5. Alice 提交后，以 Bob 登录 OA 审批。
6. 回到原 Codex 线程说“继续”，执行自然语言和直接 SQL 查询。
7. 重复查询直到预算耗尽，确认任务自动归档。
8. 切换 Carol Token，查询完整审计事件和凭证链。

交付文档包括：

- 一页式架构与安全边界。
- Docker Compose 启动说明。
- Codex MCP 配置和演示脚本。
- YAML 数据产品编写指南。
- OA 与数据源适配器接口说明。
- SQL 安全规则与已知限制。
- 威胁模型和生产化差距。

### 固定假设

- Demo 使用本地 Codex，不支持 Codex Cloud 访问 localhost。
- PostgreSQL 是普通旧业务数据库，不承担 Gateway 控制面存储。
- 生产 OAuth 2.0/OIDC、主流 OA/CRM 连接器、多实例控制库和密钥管理系统属于后续版本。
- Skill 名称和版本可作为不可信客户端元数据写入审计记录，但不参与授权。
- Gateway只允许只读分析，不提供任何数据修改能力。
