# SQL 安全规则与已知限制

## 决策链

无论 SQL 来自 `query_sql` 还是 `execute_plan` 的确定性编译，都会经过同一条本地链路：

```text
Agent SQL / 编译后的 QueryPlan SQL
  → 所有权、任务状态、签名 Grant、TTL、Catalog 摘要与 Schema Attestation
  → pg_query_go/v6 PostgreSQL AST 解析
  → 语句/对象/字段/函数/运算符/特性白名单
  → 为每个逻辑产品生成 TaskGrant 约束 CTE
  → 外层 LIMIT = 剩余累计行预算
  → Control PostgreSQL 以 (task_id, request_id) 幂等检查并预留预算
  → 业务 PostgreSQL 显式只读事务 + statement_timeout
  → 结算预算、AES-GCM 保存结果、写 Ed25519 查询回执和审计事件
```

安全判断不使用正则或注释剥离来猜测 SQL。解析失败或出现未识别 AST 节点时关闭式拒绝。

`query_sql` 和 `execute_plan` 都要求客户端提供任务内唯一的 `request_id`。相同 ID/相同请求摘要只观察首次持久化结果或状态；相同 ID/不同摘要返回冲突，绝不再次执行或消费预算。

## 允许的 SQL

- 恰好一条 PostgreSQL `SELECT`。
- 非递归 `WITH ... SELECT`、子查询、Join、`UNION ALL`、分组、排序及策略白名单支持的表达式。
- 只引用本次 Grant 中的未限定逻辑产品名，例如 `expense_summary`。
- 只引用获批字段或查询内确定生成的别名。
- 函数、聚合与运算符必须处于当前获批产品 Catalog 允许列表和引擎安全集合内。
- 白名单标量类型的普通 Cast，例如 `CAST(amount AS numeric)`。
- 显式选择列。普通 `SELECT *` 和 `product.*` 禁止；聚合参数中的 `count(*)` 是特意允许的例外。
- 字符串、布尔、数字和 `NULL` 等普通常量。QueryPlan 的 literal 由 Go 编译器转义，不把客户端值当 SQL 片段。

一个合法直接 SQL 示例：

```sql
SELECT department, expense_type, sum(total_amount) AS amount
FROM expense_summary
GROUP BY department, expense_type
ORDER BY amount DESC
```

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

- 非 owner、`NOSUPERUSER`、`NOBYPASSRLS` 的 `gateway_reader` 只对两个 `reporting.*` View 有 `SELECT`，对 `legacy.*` 无权限。
- 角色级和连接级 `default_transaction_read_only=on`。
- 每次执行显式 `READ ONLY` 事务。
- 角色、连接及事务本地 `statement_timeout`，最终取任务剩余 DB 预算、Profile 单次上限、Grant 剩余有效期和 Connector 5 秒上限中的更小值；在途查询不能越过授权截止点继续返回结果。
- `search_path=pg_catalog`，生成 SQL 对物理 View 使用安全引用标识符。
- Context Deadline 与 PostgreSQL 取消错误统一映射为安全错误码。

## QueryPlan 路径

`execute_plan` 接受单产品声明式 `QueryPlan`：选择字段、聚合、过滤、分组、排序和 Limit。确定性的 Go 编译器校验产品必须已获批、字段和聚合位于 Catalog/Grant 白名单、别名不重复、Filter 运算符与 literal 类型安全、排序引用有效且 Limit 非负，再把编译结果送入完整 AST 策略。

Gateway 不包含模型客户端、Prompt 或自然语言翻译器。需要从自然语言构造 QueryPlan 时，由 Gateway 外部的 Agent 完成；Gateway 只信任经过本地验证后的结构化字段。

## 已知限制

- SQL 是保守子集。合法但 AST 节点未列入白名单的 PostgreSQL 特性会被拒绝；这属于预期的关闭式行为。
- `execute_plan` 的 QueryPlan 只支持一个产品；需要 Join 多个已批准产品时使用 `query_sql`，仍受同一 Grant 和策略约束。
- 直接 SQL 不支持客户端占位符。调用方必须提交完整 SQL；Gateway 不提供任意 Session 变量入口。
- Connector 在启动、readiness 和每次新查询前对 Reporting View 的列顺序与 PostgreSQL 类型执行 Catalog-pinned Schema Attestation；任何漂移都会关闭式拒绝。
- 结果在 Gateway 内存中物化后整体 JSON 编码并加密；没有流式结果，Connector 全局硬上限为 10,000 行，Demo Profile 更低。
- `output_format=table` 目前只作为结构化响应元数据返回，Gateway 不负责表格或图表渲染。
- SQL 指纹是规范化 Agent SQL 的 SHA-256；它证明请求文本，不是数据库执行计划 Hash。
- 结果确定时，预算 DB 时间按 Gateway 观测的实际有界用量结算；执行结果无法确定时标记 `INDETERMINATE` 并按完整预留保守计费。它不等同于 PostgreSQL `EXPLAIN ANALYZE` 的精确执行指标。
- Reporting View 与只读角色仍是关键安全边界；SQL AST 策略不能修复一个错误暴露敏感列的 View。
- Audit Hash Chain 只能检测 Gateway 日志的事后修改；它不是 WORM 存储或外部不可篡改账本。
