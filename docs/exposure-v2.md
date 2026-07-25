# taskgate-exposure-v2

规范性的完整语法、类型规则、逐运算推导、FactID 编码、SQL 值域条件与严格定理见 [Exposure Algebra V2：正式语义](exposure-algebra-v2.md)。本文保留系统集成与 planner 概览。

`taskgate-exposure-v2` 是与 V1 不可混用的 Exposure Profile。一个 root task family 的 Control PG ledger 在创建时固定 profile；委托任务不能改变它。

默认 Catalog 只定义 V2 budget profiles；low、medium、high approval routes
全部要求独立的人类审批，不存在自动批准路径。

V2 的执行边界是：客户端提交候选 `QueryPlan` 与 utility evidence，不提交 FactID 或成本。Gateway 在同一个 Business PostgreSQL `REPEATABLE READ` 快照内执行所有候选及 provenance companion，生成每个候选的精确 `(release, influence)` FactSet。Control PostgreSQL 随后锁定 root ledger，扣除最新历史集合，用双 bitset exact planner 选择候选，并在同一事务中完成联合 FactSet 结算、资源扣费、选中结果加密、representation plan、审计与 V5 receipt。事务提交前不会释放任何候选结果。

## Fact identity

V2 有三种语义 payload：

```text
BaseRow(profile, source_namespace, snapshot, entity_key)
BaseCell(profile, source_namespace, snapshot, entity_key,
         field_id, sql_type, canonical_value)
Derived(profile, snapshot_bundle, output_row_key,
        normalized_expression, sql_type, canonical_value,
        witness_multiset_commitment)
```

索引键为 `SHA256("TASKGATE-FACT-V2\0" || CanonicalEncode(payload))`。`exposure_facts.canonical_payload` 同时保存规范 payload；相同 hash 对应不同 payload 时事务关闭式失败。`source_namespace` 和 self-join role 来自 Catalog，不使用 SQL alias。预计算 summary/generalized product 若要展开到另一个 base namespace，必须提供受信 lineage manifest；未提供时不能声称跨产品 influence overlap。

支持值域保留 PostgreSQL 类型：整数、exact numeric、浮点、布尔、文本、bytea、date/timestamp、JSON/JSONB 与 UUID。NULL 有独立编码；numeric 用任意精度有理数规范化，不经 `float64`。字符串字段必须由 Catalog 固定确定性 PostgreSQL collation 的精确名称和版本，Connector 在启动和每次查询事务内重新证明；`time with time zone` 关闭式拒绝。

## Annotation and algebra

每行含 row support、每单元格 support，以及保留 multiplicity 的 witness multiset。Influence 使用 support set；Derived FactID 使用 witness commitment。因此 Join fanout 不重复收取 influence，但会改变聚合派生事实的 witness。

正式支持 scan、selection、projection、多列 conjunctive equijoin、union-distinct、group/`COUNT|SUM|MIN|MAX` 和稳定分页。语义固定为 PostgreSQL bag semantics、SQL NULL/三值逻辑、exact numeric、UTC 与精确证明的 deterministic collation。分页必须由用户排序字段加 Catalog stable entity/group key 构成全序。

`taskgate-query-normal-form-v3` 是 typed normal form：除 alias 删除、字段限定、AND/projection/group 归一化、aggregate 名称与类型归一化及稳定分页 tie-break 外，还静态检查 Scan schema、Join predicate、Union schema 和 collation profile。它不声称解决任意 SQL 等价。

## Exact planner

对候选 Effect `E_c` 和 root history `K_T`，成本是：

```text
R(S) = |union(E_c.release for c in S) - K_T.release|
I(S) = |union(E_c.influence for c in S) - K_T.influence|
```

实现先批量扣除历史 hash，再分配稠密整数并构造 release/influence bitset。每个 requirement 枚举 skip 或选择一个候选，扩展使用 OR 与 popcount。唯一安全的 frontier 支配条件是两个集合均为子集且 utility 不低；仅比较集合大小不安全。

V2 默认限制每次最多 16 个候选。所有候选的可见查询与 provenance 数据库时间都计入普通 DB 时间预算；只有选中结果被持久化和释放。

## API example

```json
{
  "task_id": "task_...",
  "request_id": "monthly-representations-1",
  "candidates": [
    {
      "id": "monthly-summary",
      "requirement": "trend",
      "plan": {
        "product": "expense_summary",
        "columns": ["month"],
        "aggregates": [{"function":"sum","column":"total_amount","alias":"total"}],
        "group_by": ["month"],
        "order_by": [{"column":"month","direction":"asc"}]
      },
      "utility_evidence": {"answer_completeness": 0.9, "query_coverage": 1.0}
    }
  ]
}
```

响应只公开选中候选、cost cardinality、effect digest 和选中结果，不公开 FactID payload。V5 receipt 绑定 snapshot bundle、全部候选摘要、选中摘要、planner version 与联合 Effect digest。
