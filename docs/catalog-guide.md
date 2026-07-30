# Catalog 编写指南

`config/catalog.yaml` 是 Gateway 的可信、版本化逻辑数据契约。Gateway 校验 YAML 后，会在启动、readiness 检查和每个新查询前，把其中声明的 Reporting View 列顺序与 PostgreSQL 通用类型同实时 Schema 做 Attestation；任何漂移都会关闭式拒绝。目录仍不是数据库元数据缓存，Reporting View 和只读角色权限必须由管理员共同维护。

## 顶层结构

Catalog 使用严格 YAML 解码，只允许一个文档，未知字段会导致启动失败。V4 顶层结构包含冻结发布物：

```yaml
catalog_version: "2026-07-21.2"
sources: []
snapshot_publications: []
scopes: []
products: []
approval_routes: []
budget_profiles: []
```

`catalog_version` 最长 128 字符，可包含字母、数字、点、下划线和连字符。任何会影响查询含义或授权的变更都应提升版本。ACTIVE 任务记录创建时的版本；当前运行 Catalog 版本不一致时，Gateway 会关闭式拒绝查询，防止配置热变更扩权。

## Source

当前只支持 `postgres`/`postgresql`：

```yaml
sources:
  - name: travel_demo
    datasource_id: taskgate-demo-travel
    type: postgres
    address: business-postgres
    port: 5432
    database: travel_demo
    user: gateway_reader
    postgres_major_version: 16
    schema_digest: 02b4a211cfbab7347fdce28e2dd76406b1118c5f18e1d2146cc2e85a38ccf1cc
    secretRef: GATEWAY_DB_PASSWORD
```

规则：

- `name` 是小写逻辑标识符：`[a-z_][a-z0-9_]*`。
- `datasource_id` 是稳定小写数据源身份，会由业务库 `reporting.datasource_attestation` 证明，并写入 Grant 与查询 Receipt。
- `address` 只写主机，不得包含空白、路径、`@` 或凭据。
- `port` 必须为 1–65535；数据库名和用户名必须是简单标识符。
- `postgres_major_version` 必须匹配运行时业务 PostgreSQL 主版本。
- `schema_digest` 是 Gateway 启动时要求的 Reporting View attestation 摘要，包含列顺序、通用 PostgreSQL 类型和 PostgreSQL 规范化后的 View 定义。
- `secretRef` 必须引用大写环境变量，可写 `GATEWAY_DB_PASSWORD` 或 `env:GATEWAY_DB_PASSWORD`。
- `password`、`passwd`、`pwd`、`dsn`、带 `user:password@host` 的地址等明文秘密会被语法树预检拒绝，错误不会回显秘密。

当前 Gateway 只实例化一个业务 Connector，因此同一任务的产品必须来自同一 `sources[]`。启动时 Gateway 按该 Source 和 `secretRef` 构造业务 DSN，并校验 `datasource_id`、数据库名、角色、PostgreSQL 主版本和 Reporting View Schema 摘要。

## Snapshot Publication（V4）

```yaml
snapshot_publications:
  - name: expense-summary-v1
    source: travel_demo
    source_namespace: travel.expense_summary
    snapshot: travel-demo-2026-v1
    ordinal_sidecar: taskgate_ordinal.expense_summary_v1
    sidecar_digest: <64-hex>
    dictionary_digest: <64-hex>
    manifest_digest: <64-hex>
```

Publication 是实际冻结的数据发布物，不是给可变表添加的标签。可发布的 compiler input 应声明受限的 `source_relation: reporting.<name>`；`cmd/snapshot-index` 从 `SNAPSHOT_POSTGRES_DSN` 使用只读角色，在同一 `REPEATABLE READ` 事务中验证 populated materialized view、NOLOGIN owner、每个唯一物理字段的 PostgreSQL type/collation/version，并以数据库 rows 覆盖 JSON candidate rows 后构建 sidecar、HOT/COLD dictionary 和 bundle。stdout 给出应写回 Candidate Catalog 的 digest；不要手工猜测 digest。Product 的 `source`、`reporting_view`、`fact_namespace`、`snapshot` 必须与 publication 构建来源及其 `source`、`source_namespace`、`snapshot` 一致。

Catalog 声明 publication 时，Gateway 必须配置 `GATEWAY_SNAPSHOT_ARTIFACT_DIR` 并严格激活全部 publication：目录/文件不可为 symlink，bundle 必须唯一，Catalog 与 artifact 的 schema/snapshot/dictionary/manifest/sidecar digest 必须一致，HOT 总量不得超过配置上限；sidecar 会逐行对照 HOT，COLD 会在只读 publication mount 上全文件流式核 manifest envelope、seal、长度和 SHA-256。COLD 的逐 Fact semantic roots 由发布前 builder/audit verifier 建立，bundle descriptor 自身不是信任根。任一失败都会阻止 Gateway 启动；详情见 [TaskGate V4](exposure-v4.md)。

只要 Catalog 声明 V4 publication 或任一可达 approval route 使用 V4，全部可达 route 都必须使用 `taskgate-exposure-v4`；V2/V3 和未启用 exposure 的 route 不能与它混合。成功激活会在 Control PG 留下单向 deployment marker。之后可以发布新的 V4 Catalog，但不能通过删除 publication、改回 legacy Profile 或换旧 Gateway binary 降级；这类启动或数据库写入会关闭式失败。

## Scope

Scope 定义可申请范围及其上界。当前支持枚举和日期范围：

```yaml
scopes:
  - name: department
    type: enum
    description: 允许访问的部门
    allowed_values: [销售部, 研发部, 财务部]

  - name: expense_date
    type: date_range
    description: 报销发生日期范围
    min: "2026-01-01"
    max: "2026-12-31"
```

- `enum` 必须提供非空、唯一的 `allowed_values`，不能提供 `min/max`。
- `date_range` 必须提供合法的 `YYYY-MM-DD` 最小/最大日期，不能提供 `allowed_values`。
- 某产品列入 `scopes` 的每个名称都是强制范围，并且必须同时是该产品发布的字段。TaskIntent 缺任一强制范围时，任务申请失败，而不是退化为无范围授权。
- 当前 Demo 的两个产品都强制 `department`；`expense_date` 是未绑定产品的扩展示例。

## Product

一个逻辑产品映射一个由管理员维护的 `reporting.*` View：

```yaml
products:
  - name: expense_summary
    source: travel_demo
    reporting_view: reporting.expense_summary
    description: 按月份、部门和费用类型汇总的差旅报销数据
    sensitivity: low
    snapshot: travel-demo-2026-v1
    snapshot_publication: expense-summary-v1
    entity_key: [month, department, expense_type]
    fact_namespace: travel.expense_summary
    stable_relation_role: expense_summary
    scopes: [department]
    allowed_functions: [date_trunc, to_char]
    allowed_operators: ["=", "<>", "<", "<=", ">", ">=", "+", "-", "*", "/"]
    allowed_aggregates: [sum, count, min, max]
    fields:
      - name: month
        type: text
        collation: en_US.utf8
        collation_version: "2.36"
        description: 月份，格式 YYYY-MM
      - name: department
        type: text
        collation: en_US.utf8
        collation_version: "2.36"
        description: 部门
      - name: total_amount
        type: numeric
        description: 总金额
```

规则与建议：

- `name`、字段名必须为小写逻辑标识符，且不得重复。
- `source` 必须存在。`reporting_view` 必须严格匹配未加引号的 `reporting.<lowercase_name>`，同一 View 不能发布两次。
- 必须提供描述、至少一个字段和至少一个强制 Scope；每个 Scope 必须对应发布字段。
- `snapshot` 是稳定、非空的数据发布版本。V4 还必须用 `snapshot_publication` 引用实际冻结、manifest-proven 的发布物；它不是自动采集的 MVCC transaction ID。
- V2 产品必须同时提供 `fact_namespace` 与 `stable_relation_role`。所有 `text`/`character`/`character varying` 字段还必须固定数据库实际报告的 deterministic collation 名称及版本；Gateway 会在每次 schema 证明中核对名称、版本和 `collisdeterministic`。数据库 libc/ICU 升级后应先提升 Catalog 版本并重新审批，而不是静默接受排序语义漂移。
- `entity_key` 必须是非空、不重复的已发布字段列表，并在该 snapshot 内唯一、稳定。Gateway 会在 exposure 路径内部读取这些字段用于 FactID，但不会把未获批的 key 字段返回客户端。
- 支持的敏感级别从低到高为 `public`、`low`、`medium`、`high`、`restricted`。
- 产品有效敏感级别取产品级与所有字段级标记的最高值。审批路由按有效级别选择，不能通过给产品标低级而降低高敏字段。
- 字段 `type` 必须是受支持的 PostgreSQL `information_schema.columns.data_type` 通用类型；Gateway 会逐列核对名称、顺序和类型。Catalog V1 不声称核对 `numeric` 精度/小数位、字符长度、Domain 或数组元素类型，因此拒绝 `numeric(10,2)` 等带修饰符写法；需要该边界时应在 Reporting View 中选用独立通用类型并提升协议版本。
- Reporting View 应只暴露业务所需字段。手机号、工资、银行卡等即使“不写入 Catalog”，若仍出现在 View 中，也不应向 `gateway_reader` 暴露。
- 函数和聚合必须是未限定的小写名，且不能是文件、网络、管理、睡眠等禁止函数；运行时还会经过 SQL AST 的危险函数硬拒绝表。
- Exposure V2 只允许 `count`、`sum`、`min`、`max`；`avg` 即使在传统 SQL allowlist 中存在，也不能进入 V2 计划。
- 运算符必须来自安全词汇表。允许列表是上限，不会使 AST 原本不支持的 SQL 特性变为可用。

## Approval Route

每个被产品实际使用的有效敏感级别都必须有唯一审批路由：

```yaml
approval_routes:
  - sensitivity: low
    mode: manual
    approver: bob
    budget_profile: summary-manual-v4
  - sensitivity: high
    mode: manual
    approver: bob
    budget_profile: detail-manual-v4
```

- 只允许 `mode: manual`；自动任务审批已关闭。
- 每条路由必须配置非空 `approver`；OA Demo 只接受 `bob`。
- `budget_profile` 必须引用已定义 Profile。
- 多产品申请以最高有效敏感级别路由。

## Budget Profile

```yaml
budget_profiles:
  - name: summary-manual-v4
    max_queries: 10
    max_rows: 500
    max_db_time: 30s
    query_timeout: 5s
    task_ttl: 30m
    max_release_facts: 1000
    max_influence_facts: 5000
    max_outcome_facts: 10
    exposure_profile_version: taskgate-exposure-v4
```

资源数值必须为正；可选 exposure 上限必须同时为零/省略或同时为正。
`query_timeout` 不能大于 `max_db_time`。时间使用 Go duration 字符串，如
`500ms`、`5s`、`15m`。含义：

| 字段 | 含义 |
|---|---|
| `max_queries` | 完成或数据库执行失败的允许查询次数上限 |
| `max_rows` | 所有成功结果累计行数上限 |
| `max_db_time` | 累计记账数据库时间上限 |
| `query_timeout` | 单次 PostgreSQL `statement_timeout` 上限 |
| `task_ttl` | 从批准生成 Grant 起的可用时长 |
| `max_release_facts` | 根任务族可新增的已交付结果 FactID 上限 |
| `max_influence_facts` | 根任务族可新增的 positive-output dependency FactID 上限；字段名为兼容标签 |
| `max_outcome_facts` | 根任务族可新增的规范化成功查询命题/结果 FactID 上限 |
| `exposure_profile_version` | 固定 FactID、lineage 和收费语义的版本标识 |

V4 的三个 exposure 上限必须同时为正；禁用时三者均为零且 Profile 为空。
V2 兼容 Profile 的 outcome 上限保持为零。默认 Catalog 启用 V4。
Profile 是经治理、允许被完整消耗的容量边界。Agent 不能选择更小预算；Gateway
按审批路由绑定整个 Profile。若完整额度并不安全，应由 Catalog 管理者降低并发布
新版本，而不能依赖 Agent 节约。
资源预算先预留再按实际量结算；exposure 则在结果产生后按相对根任务已知集合
的 novel facts 结算。资源预算达到硬上限会归档任务，exposure 超限会拒绝且
不释放当前结果；后续调用仍可选择更低成本或完全重复的已知事实。

## 最小完整示例

```yaml
catalog_version: "2026-07-21.2"

sources:
  - name: demo
    datasource_id: taskgate-demo-travel
    type: postgres
    address: business-postgres
    port: 5432
    database: travel_demo
    user: gateway_reader
    postgres_major_version: 16
    schema_digest: 02b4a211cfbab7347fdce28e2dd76406b1118c5f18e1d2146cc2e85a38ccf1cc
    secretRef: GATEWAY_DB_PASSWORD

snapshot_publications:
  - name: monthly-spend-v1
    source: demo
    source_namespace: demo.monthly_spend
    snapshot: demo-2026-v1
    ordinal_sidecar: taskgate_ordinal.monthly_spend_v1
    sidecar_digest: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    dictionary_digest: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    manifest_digest: cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc

scopes:
  - name: department
    type: enum
    description: 部门范围
    allowed_values: [销售部]

products:
  - name: monthly_spend
    source: demo
    reporting_view: reporting.monthly_spend
    description: 部门月度费用
    sensitivity: low
    snapshot: demo-2026-v1
    snapshot_publication: monthly-spend-v1
    fact_namespace: demo.monthly_spend
    stable_relation_role: monthly_spend
    entity_key: [month, department]
    scopes: [department]
    allowed_operators: ["=", ">", ">="]
    allowed_aggregates: [sum, count]
    fields:
      - name: month
        type: text
        collation: en_US.utf8
        collation_version: "2.36"
        description: 月份
      - name: department
        type: text
        collation: en_US.utf8
        collation_version: "2.36"
        description: 部门
      - name: amount
        type: numeric
        description: 金额

approval_routes:
  - sensitivity: low
    mode: manual
    approver: bob
    budget_profile: small-manual

budget_profiles:
  - name: small-manual
    max_queries: 5
    max_rows: 100
    max_db_time: 10s
    query_timeout: 2s
    task_ttl: 15m
    max_release_facts: 200
    max_influence_facts: 1000
    max_outcome_facts: 5
    exposure_profile_version: taskgate-exposure-v4
```

## 发布清单

1. 在 PostgreSQL 创建或更新 `reporting.*` View，只选择允许发布的列。
2. 只向专用只读角色授予目标 View 的 `SELECT`，确保对底层业务 Schema 无权限。
3. 冻结 snapshot，运行 `cmd/snapshot-index`，完整验证 bundle 后把计算出的 publication digests 写入 Candidate Catalog。
4. 更新 Catalog，确认 Product 与 publication 的 source/namespace/snapshot、逻辑字段、类型、Scope 和稳定 `entity_key` 一致。
5. 为产品最高有效敏感级别配置审批路由和允许完整消耗的预算 Profile；不要依赖 Agent “节约”来补救过大的 Profile。
6. 提升 `catalog_version`。版本切换会使旧 ACTIVE 任务拒绝查询，应先规划任务排空或重新申请。
7. 运行 `make verify`，再用 `docker compose up --build -d --wait` 启动；Catalog/artifact 非法时 Gateway 不会进入 Ready。
8. 分别用允许、越权字段、缺 Scope、未知 handle/ordinal、损坏 dictionary、物理表名和危险函数做负向验证。

不要在 Catalog 中写密码、DSN、OA Token、模型 Key 或任何真实查询结果。
