# SQL 安全规则与已知限制

## 决策链

普通 Agent 默认使用 `query_sql`。Resource-only Grant 保留现有安全 SQL policy；exposure-enabled Grant 先按 `taskgate-reporting-sql-v1` 无损 lowering 为 canonical QueryPlan。高级 `execute_plan` 不在普通 `tools/list` 中列出，但与 SQL lowering 共用后半段信任边界：

```text
Agent SQL
  → 所有权、任务状态、签名 Grant、TTL、Catalog/Schema 与 task View binding
  → resource-only: pg_query_go/v6 安全 SQL policy
  → exposure: PostgreSQL AST 解析 → canonical QueryPlan
  → 重新生成 visible + ordinal companion SQL
  → 语句/对象/字段/函数/运算符/特性白名单
  → 为每个执行产品生成 TaskGrant 约束 CTE（semantic root 为每查询派生的 terminal internal grants）
  → 外层 LIMIT = 剩余累计行预算
  → exact request replay / task-scoped View binding precheck
  → Control PostgreSQL 以 (task_id, request_id) 幂等检查并预留资源与 exposure evidence
  → 业务 PostgreSQL 显式只读 REPEATABLE READ 事务 + statement_timeout
  → 同事务重发现已绑定 View closure 并核 revision，再执行 visible/companion
  → 推导 exact bitmap effect，ANDNOT + popcount 后一次 CAS 三维 root head
  → 同事务保存结果/materialization、V6 Ed25519 回执和审计，commit 后释放
```

安全判断不使用正则或注释剥离来猜测 SQL。解析失败或出现未识别 AST 节点时关闭式拒绝。Exposure 路径不直接执行 Agent 原始 SQL；lowering 成功后只执行 QueryPlan 重新生成并再经 policy 的 SQL。

`query_sql` 和 `execute_plan` 都要求客户端提供任务内唯一的 `request_id`。相同 ID/相同请求摘要只观察首次持久化结果或状态；相同 ID/不同摘要返回冲突，绝不再次执行或消费预算。

## Resource-only SQL 兼容片段

- 恰好一条 PostgreSQL `SELECT`。
- 非递归 `WITH ... SELECT`、子查询、Join、`UNION ALL`、分组、排序及策略白名单支持的表达式。
- 只引用本次 Grant 中的未限定逻辑产品名，例如 `expense_summary`。
- 只引用获批字段或查询内确定生成的别名。
- 函数、聚合与运算符必须处于当前获批产品 Catalog 允许列表和引擎安全集合内。
- 白名单标量类型的普通 Cast，例如 `CAST(amount AS numeric)`。
- 显式选择列。普通 `SELECT *` 和 `product.*` 禁止；聚合参数中的 `count(*)` 是特意允许的例外。
- 字符串、布尔、数字和 `NULL` 等普通常量。QueryPlan 的 literal 由 Go 编译器转义，不把客户端值当 SQL 片段。

下例在 SQL policy 的 resource-only 兼容模式中合法：

```sql
SELECT department, expense_type, sum(total_amount) AS amount
FROM expense_summary
GROUP BY department, expense_type
ORDER BY amount DESC
```

这个兼容片段不定义 exposure FactID，也不会扩大下面的 reporting SQL profile。

## Exposure reporting SQL profile

`taskgate-reporting-sql-v1` 是可无损转换为 canonical QueryPlan 的闭合子集：

- 恰好一条 `SELECT`，只引用 Grant 中的未限定逻辑产品名和获批字段；
- 单产品 projection、literal filter 合取、`GROUP BY`、`ORDER BY`、`LIMIT/OFFSET` 和 `COUNT/SUM/MIN/MAX`；
- 2–16 个不同 Catalog 稳定角色组成的任意 connected INNER equi-join graph 形状；每条 edge 必须有一个或多个 column-to-column equality predicate；
- SQL table alias 先映射为 Catalog stable role；JoinGraph 的 nodes、无向 edges、edge 内 predicates 及 equality 两端按稳定字段 ID 规范排序并转换为现有 `join_many`，再 deterministic binary fold 为现有二元 Join 代数。Filter 合取同样按语义规范化；展示列顺序仍保持用户可见语义。

16-source 上限是限制生成 SQL 宽度、provenance 行和 PostgreSQL planning work 的 operational complexity/DoS ceiling，不限制 16 个 source 以内的 graph 形状，并显式覆盖 10 表 Join。整个 MCP 请求体仍限为 1 MiB，SQL 仍须通过 PostgreSQL parser 和 AST 白名单/结构校验，执行仍受获批资源预算、statement timeout 和行数上限约束。

内部计划和 FactID 始终使用稳定字段 ID；响应层单独保存并恢复原 SQL 的输出 alias 与 target-list 顺序（包括列/聚合交错）。这些展示元数据保存在 Control PG 的 artifact schema/查询元数据中，加密 Parquet bytes 保存在 TaskGate 对象存储中；因此 `get_query_result`、幂等 replay 和 V4 semantic replay 返回与首次调用一致的列名、列值顺序、`query_plan`、`sql_profile` 和 `plan_digest`。Semantic replay 在复用结果前按稳定语义列身份重排；旧结果缺少该身份时按 cache miss 重新执行，不猜测映射。普通 query/get 仅返回 `result_id` 和摘要，不因这些展示元数据而返回完整行。

Self-join、outer/cross/non-equality join、断开的 join graph、子查询、CTE、set operation、窗口函数、`HAVING`、`SELECT DISTINCT`、位置式 group/order 引用和多输入分页不可 lowering。Gateway 不会删除 predicate、忽略不可表达的输出语义，也不会把 `LEFT JOIN` 改为 `INNER JOIN`。

## Nested View definition profile

Phase B 还会编译 Catalog semantic root 的 PostgreSQL View definition。这个 profile
与 Agent-facing `taskgate-reporting-sql-v1` 分开，并且更窄：它的目标只是判断整个
dependency closure 能否无损 flatten 为现有 typed SPJG/`join_many` QueryPlan，不会
把任意 PostgreSQL View 交给 exposure compiler。

支持的 View definition 仅包括：

- 一个 `SELECT`，direct column projection/rename；
- direct column 与 typed literal 的 filter，复合 predicate 只允许 `AND`；
- 任意形状的 connected `INNER JOIN ... ON` graph，`ON` 只能是一个或多个 direct column equality；
- direct-column `GROUP BY` 与 `COUNT(*)`/`COUNT(column)`/`SUM`/`MIN`/`MAX`；
- 多层 ordinary View 递归展开；已治理、已填充的 materialized view 作为 Catalog Product terminal，不展开 seed tables。

一个 closure 最多有一道 aggregate barrier。任何消费 aggregate output 的 filter、
join 或 re-aggregation 都拒绝；barrier 上方只可做单输入 direct projection/rename。
View 中的 CTE/subquery、set operation、`DISTINCT`、outer/cross/natural/`USING` Join、
non-equality Join、scalar function、projection cast、window、`HAVING`、`ORDER BY`、
`LIMIT/OFFSET`、relation function、raw/foreign/system relation 和 recursion/cycle 都拒绝。
Literal deparser cast 只有在能证明 exact supported type 时才接受，它不扩大 projection
expression 能力。

编译器同时实施 expanded sources 16、View depth 16、reachable nodes 64、dependency
edges 128 和累计 definition bytes 1 MiB 上限。DAG discovery 可以 memoize shared
child；semantic expansion 仍按 occurrence 保留 bag semantics，重复 governed
product/stable role 因属于 self-join 而拒绝。

View Artifact 还是执行输入，不只用于比对 digest。对一个 semantic root 的
outer single-product QueryPlan，组合器把 public 字段按 `Artifact.Output.FieldID`
替换为已展开的 stable-role fields，保留 View filters/JoinGraph/group/aggregate，
然后调用现有 `CompileRelational`。V4 从各 terminal Product 的 ordinal publication
和 sidecar 编译 companion，不把 root View 当作 opaque V4 Product。

这一内部展开不会扩大签名 Grant：public Grant 仍只包含 root 及获批
outputs/Scope。Gateway 从已绑定 Artifact 每查询派生最小 terminal ProductGrants，
只向它们加入已编译计划/证据需要的字段。Root scope output 也用
`Artifact.Output.FieldID` 映射到 terminal role/column；每个 terminal Catalog 声明的
mandatory scope 都必须被相同任务谓词覆盖。缺失、歧义或物理映射冲突在
reservation 前拒绝，internal grants 不持久化、不回传 Agent。

查询时组合的当前边界比 View definition fragment 更窄：semantic root 必须是
外层唯一 source，不能再与另一 Product Join；root 上的 `ORDER BY`、`LIMIT`、
`OFFSET` 拒绝。这不限制 View 内的 2–16 源任意 connected equi-join 或每 edge
多 equality predicates。若 Artifact 已跨过 aggregate barrier，外层只可纯投影/
重排既有 outputs；filter、Join、group 或 re-aggregation 都返回稳定组合拒绝。

每个 root 的 Catalog contract 分别绑定 exact definition、transitive dependency、typed
canonical plan 和 ordered interface 四个摘要。通过四项检查后，任务才生成规范
`taskgate-task-view-binding-v1` 并写入签名 Manifest/Grant。Alias、registry map order、
Join traversal/order 等不改变 typed algebra 的改写应保持 `canonical_plan_digest`；
filter/join/group/aggregate 语义、dependency 或 public name/type/collation 的变化必须在
对应摘要中可见。五类固定 seed 的 `testing/quick` properties 各运行 64 个
2–5 源 generated cases，检查 determinism、alias invariance、join-order invariance、
direct/nested equivalence 和 semantic/interface/dependency drift。生成图至少包含一条
multi-predicate edge，nested case 包含 1–3 层 transparent wrappers。它们与其他拒绝边界
tests 都不构成对任意 SQL 的形式化等价证明。

lowering 错误是 MCP `isError=true` 的结构化结果。例如：

```json
{
  "trace_id": "...",
  "error": {
    "code": "JOIN_TYPE_UNSUPPORTED",
    "reason": "LEFT_JOIN_UNSUPPORTED",
    "message": "Exposure accounting currently supports connected INNER equijoins only.",
    "location": {"clause": "FROM", "relation": "customer"},
    "supported_alternative": "Use an INNER JOIN or an approved prejoined reporting product.",
    "retryable_after_rewrite": true,
    "sql_profile": "taskgate-reporting-sql-v1"
  }
}
```

稳定 lowering/授权错误包括 `SQL_SYNTAX_ERROR`、`PRODUCT_NOT_APPROVED`、`COLUMN_NOT_APPROVED`、`SQL_NOT_LOWERABLE`、`JOIN_TYPE_UNSUPPORTED`、`JOIN_GRAPH_DISCONNECTED`、`JOIN_KEY_TYPE_MISMATCH`、`COLLATION_MISMATCH`、`SUBQUERY_UNSUPPORTED`、`VIEW_QUERY_UNSUPPORTED` 和 `VIEW_SEMANTIC_CHANGED`。`VIEW_QUERY_UNSUPPORTED` 表示当前 View contract 有效，但请求在 root 上方使用了 graph graft、排序/分页或跨 aggregate barrier 操作，可按返回的 `reason` 改写；`VIEW_SEMANTIC_CHANGED` 则覆盖 View closure 不再匹配 contract、当前定义落到不支持 fragment 或 registry revision 在执行前变化。公共错误不回显物理 View、definition、DSN 或底层 parser 详情。

`VIEW_SEMANTIC_CHANGED` 会把 task 的正交 View 状态从 `ACTIVE` 单向置为
`REQUIRE_REBIND`。新 query/reservation 和新 delegated child 都被拒绝；不能修改旧
Grant 让它指向新摘要。恢复方式是用更新后的 Catalog 创建新 task 并重新人工 OA
审批。检查顺序仍允许旧 task 的相同 `(task_id, request_id, request_digest)` 精确终态
重放；已有 result artifact、query record 和 receipt 不因漂移而删除。

语法、授权或 lowering 失败发生在 Business PostgreSQL 执行和正式 reservation 之前：不扣 queries/rows/DBMS，也不扣 release/dependency/outcome。数据库已开始执行后的 timeout、连接不确定或 provenance 故障继续使用现有 failure-settlement 规则。

## 一律拒绝

| 类别 | 示例 | 稳定策略码 |
|---|---|---|
| 多语句 | `SELECT ...; DELETE ...`，包括注释混淆 | `SQL_MULTIPLE_STATEMENTS` |
| 非 SELECT / 写操作 | `INSERT`、`UPDATE`、`DELETE`、`MERGE`、DDL、`COPY`、`CALL`、`DO`、`SET`，包括写 CTE | `SQL_WRITE_FORBIDDEN` / `SQL_NOT_SELECT` |
| 导出与锁 | `SELECT INTO`、`FOR UPDATE/SHARE` | `SQL_SELECT_INTO_FORBIDDEN` / `SQL_LOCKING_FORBIDDEN` |
| 递归 | `WITH RECURSIVE` | `SQL_RECURSIVE_CTE_FORBIDDEN` |
| 物理或系统对象 | `reporting.expense_detail`、`legacy.*`、`pg_catalog.*`、`information_schema.*`、任何 `pg_*` Relation | `SQL_SYSTEM_OBJECT_FORBIDDEN` |
| 未发布对象 | 任意非 Grant 逻辑产品或非查询内 CTE | `SQL_OBJECT_NOT_ALLOWED` |
| 通配符 | `SELECT *`、`x.*` | `SQL_WILDCARD_FORBIDDEN` |
| 越权字段 | 未列入该 TaskGrant 的列 | `SQL_COLUMN_NOT_ALLOWED` |
| 参数与 Session 值 | `$1`、客户端参数、`current_user`、`current_setting(...)` | `SQL_PARAMETER_FORBIDDEN` / `SQL_FUNCTION_NOT_ALLOWED` |
| 危险或未知函数 | `pg_sleep`、文件/网络/大对象/后台管理函数、执行字符串 SQL 的 `query_to_xml`/`ts_stat`、整表/Schema/数据库 XML 导出族、Catalog 未允许函数 | `SQL_FUNCTION_NOT_ALLOWED` |
| 未允许运算符 | Catalog/引擎白名单外运算符 | `SQL_OPERATOR_NOT_ALLOWED` |
| 扩展执行面 | Set-returning `FROM` 函数、Table Sample、XML/JSON Table 等未允许 AST | `SQL_FEATURE_NOT_ALLOWED` |

策略错误只返回稳定码和安全说明，不返回物理 View、DSN、密码或底层 Parser 诊断。

## Scope 与行预算注入

Agent 永远看不到物理映射。假设 Grant 允许 `expense_detail` 的三个字段、`department=销售部`，剩余行预算为 25，Gateway 生成等价结构：

```sql
WITH "expense_detail" AS (
  SELECT "expense_date", "expense_type", "amount", "department"
  FROM "reporting"."expense_detail"
  WHERE "department" = E'销售部'
)
SELECT *
FROM (
  SELECT expense_date, expense_type, amount
  FROM expense_detail
  ORDER BY expense_date
) AS "__taskbound_result"
LIMIT 25
```

Scope 值不是任意 SQL：它来自显式任务申请并经过 Catalog 允许值或日期边界校验，再由受控运算符和字面量转义器渲染。Catalog 启动校验要求每个产品 Scope 对应一个发布字段；任务申请缺少任一强制 Scope 会失败。

Agent 自己写的内层 `LIMIT` 只能进一步缩小结果。外层限制按剩余累计行预算生成，Connector 还有独立行上限。达到硬上限的合法查询会返回本次允许行，再在同一控制面结算中归档任务。

## 数据库侧防御

即使策略层失误，数据面仍施加：

- 非 owner、`NOSUPERUSER`、`NOBYPASSRLS` 的 `gateway_reader` 只对 `reporting.datasource_attestation` 和两个 `reporting.*` View 有 `SELECT`，对 `legacy.*` 无权限。
- 角色级和连接级 `default_transaction_read_only=on`。
- 每次执行显式 `READ ONLY` 事务；V4 miss 把可见查询和 streamed ordinal companion 放进同一个 `REPEATABLE READ` 事务；verified semantic replay hit 不执行 Business SQL。
- Semantic task 在 reservation 前先重算 requested roots 的 binding；miss 执行时又在上述同一个 `REPEATABLE READ` 事务中重新发现 closure、核对预期 registry revision，然后才发出 visible/companion SQL。两次验证解决 approval/query 间漂移与检查—执行 TOCTOU；无关 roots 不参与该 task 的检查。
- 角色、连接及事务本地 `statement_timeout`，最终取任务剩余 DB 预算、Profile 单次上限、Grant 剩余有效期和 Connector 5 秒上限中的更小值；在途查询不能越过授权截止点继续返回结果。
- `search_path=pg_catalog`、`standard_conforming_strings=on`；QueryPlan 字符串固定使用 PostgreSQL `E'...'` 转义，生成 SQL 对物理 View 使用安全引用标识符。
- Context Deadline 与 PostgreSQL 取消错误统一映射为安全错误码。

## QueryPlan 路径

`execute_plan` 接受声明式 `QueryPlan`：单产品计划支持选择、聚合、过滤、分组、排序、Limit 和 Offset；V2 还接受 2–16 源、任意 connected INNER equi-join graph 形状的 `join_many`，或同产品双分支 `union_distinct`。每条 Join edge 可有多个 column-to-column equality predicate；规范排序后的 graph deterministic binary fold 为现有二元代数。确定性的 Go 编译器校验产品必须已获批、角色来自 Catalog、字段和聚合位于 Catalog/Grant 白名单、Join graph 连通、Join 键类型/排序规则一致、Union 完整去重 schema、别名不重复、Filter 运算符与 literal 类型安全，再把编译结果送入完整 AST 策略。`COUNT(*)` 与 `COUNT(column)` 是不同的规范表达式；只有 `COUNT` 可接受 `*`。

启用 exposure 时，第二个编译阶段接受上述单产品片段和受限 Join/Union，并允许
`COUNT(*)/COUNT(column)/SUM/MIN/MAX`（可带 `GROUP BY`）。它加入 Catalog `entity_key`、Scope、
谓词和聚合输入字段来生成 ordinal provenance companion。未获批的实体键仅在策略
内部扩展 Grant，返回前会被删除；客户端不能借此查询 key。来源结果截断、
列缺失、值不可规范化或两份结果不一致都会阻止结果释放。

Gateway 不包含模型客户端、Prompt 或自然语言翻译器。Gateway 外部的 Agent 负责从自然语言生成 SQL；Gateway 只信任本地 AST lowering 和 QueryPlan 验证后的结构。

## 已知限制

- SQL 是保守子集。合法但 AST 节点未列入白名单的 PostgreSQL 特性会被拒绝；这属于预期的关闭式行为。
- 多输入在线 SQL 片段限于 2–16 源的 connected INNER equi-join graph，在该 operational complexity/DoS ceiling 内不限 graph 形状；每条 edge 是一个或多个 column-to-column equality predicate。Self-join、outer/cross/non-equality join、`NATURAL JOIN`、`JOIN ... USING`、断开的 join graph 和多输入分页关闭式拒绝。高级 QueryPlan 另保留双分支 Union-Distinct，但 SQL profile 不接受 set operation。
- `AVG` 可出现在传统 SQL Catalog allowlist，但不属于 exposure V1 精确计量片段；窗口函数、递归、任意子查询 provenance 和负信息计量也不支持。
- 直接 SQL 不支持客户端占位符。调用方必须提交完整 SQL；Gateway 不提供任意 Session 变量入口。
- Connector 在启动、readiness 和每次新查询前，对没有 `view_contract` 的 legacy/terminal products 执行 source-level Catalog-pinned Schema Attestation；semantic roots 排除在该全局 digest 外，改用 task-scoped transitive binding 和同事务 registry revision 检查。任一路径的相关漂移都会关闭式拒绝，但无关 semantic View 不会全局关闭 readiness。
- 可见结果仍在 Gateway 内存中整体 JSON 编码并加密；provenance companion 已流式消费。Connector 的可见结果仍受全局 10,000 行硬上限约束，Demo Profile 更低。
- `output_format=table` 目前只作为结构化响应元数据返回，Gateway 不负责表格或图表渲染。
- 原始 SQL 派生的 request digest 只证明请求并服务于审计/幂等；它既不是数据库执行计划 Hash，也不是 FactID 或 exposure 命题身份。语义身份由 canonical QueryPlan/`plan_digest` 决定。
- 结果确定时，预算 DB 时间按 Gateway 观测的实际有界用量结算；执行结果无法确定时标记 `INDETERMINATE` 并按完整预留保守计费。它不等同于 PostgreSQL `EXPLAIN ANALYZE` 的精确执行指标。
- Reporting View 与只读角色仍是关键安全边界；SQL AST 策略不能修复一个错误暴露敏感列的 View。
- Audit Hash Chain 只能检测 Gateway 日志的事后修改；它不是 WORM 存储或外部不可篡改账本。
