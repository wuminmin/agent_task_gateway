# taskgate-exposure-v2

规范性的完整语法、类型规则、逐运算推导、FactID 编码、SQL 值域条件与严格定理见 [Exposure Algebra V2：正式语义](exposure-algebra-v2.md)。本文保留 Profile 与语义概览。

`taskgate-exposure-v2` 是与 V1 不可混用的 Exposure Profile。一个 root task family 的 Control PG ledger 在创建时固定 profile；委托任务不能改变它。

默认 Catalog 只定义 V2 budget profiles；low、medium、high approval routes
全部要求独立的人类审批，不存在自动批准路径。

V2 的执行边界是：客户端每次提交一个确定的 `QueryPlan`，不提交 FactID 或
计量成本。Gateway 在同一个 Business PostgreSQL `REPEATABLE READ` 快照内执行
可见查询及 provenance companion，生成精确的 `(release, influence)` FactSet。
Control PostgreSQL 随后锁定 root ledger，并在同一事务中完成 novel FactSet
结算、资源扣费、结果加密、审计与 V4 receipt。事务提交前不会释放结果。

在线编译器覆盖单产品片段，以及两个 Scan 叶子的受限 INNER equijoin 和
union-distinct。Join companion 保留实际匹配行对；Union 的可见语句使用完整
schema 做 DISTINCT，而 provenance companion 使用 `UNION ALL`、分支标记和
源 entity key，因而同一 distinct class 的所有成员都会进入注释。两种输入都
可以继续 grouping/aggregation，并直接调用下述 `JoinOnV2`、
`UnionDistinctV2`、`AggregateFromResultsV2` 和 `ObserveV2`，没有第二套在线
dependency 规则。

这里的 `influence` 是兼容性的 API、数据库列和 wire identifier；在 V2 中它的
规范含义是 **conservative positive-output dependency footprint**（保守正向输出
依赖足迹），不是“实际因果影响”。对每个实际交付的输出行和单元格，它按本
Profile 的闭合关系代数规则，计入参与成功推导的基础 row/cell FactID。该集合
不要求是最小 causal provenance，也不等于 PostgreSQL 执行计划的完整 physical
read set。

V2 明确不计量 selection 为 FALSE/UNKNOWN 的行、空结果所隐含的负信息、未返
回的 group、page 外行、仅用于确定排序/排名的非输出行或排序信息，以及 timing、
cardinality side channel、背景知识和数据库物理读取但未进入正向输出推导的数据。
因此文档中的 “exact” 只表示相对于下述 contract-defined dependency profile
精确；它不表示完整 source access 或反事实因果关系。

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

索引键为 `SHA256("TASKGATE-FACT-V2\0" || CanonicalEncode(payload))`。`exposure_facts.canonical_payload` 同时保存规范 payload；相同 hash 对应不同 payload 时事务关闭式失败。`source_namespace` 和 self-join role 来自 Catalog，不使用 SQL alias。预计算 summary/generalized product 若要展开到另一个 base namespace，必须提供受信 lineage manifest；未提供时不能声称跨产品 dependency overlap。

支持值域保留 PostgreSQL 类型：整数、exact numeric、浮点、布尔、文本、bytea、date/timestamp、JSON/JSONB 与 UUID。NULL 有独立编码；numeric 用任意精度有理数规范化，不经 `float64`。字符串字段必须由 Catalog 固定确定性 PostgreSQL collation 的精确名称和版本，Connector 在启动和每次查询事务内重新证明；`time with time zone` 关闭式拒绝。

## Annotation and algebra

每行含 row support、每单元格 support，以及保留 multiplicity 的 witness multiset。
兼容字段 `Influence` 使用 support set；Derived FactID 使用 witness commitment。
因此 Join fanout 不重复收取同一 dependency FactID，但会改变聚合派生事实的
witness。

正式支持 scan、selection、projection、多列 conjunctive equijoin、union-distinct、group/`COUNT|SUM|MIN|MAX` 和稳定分页。语义固定为 PostgreSQL bag semantics、SQL NULL/三值逻辑、exact numeric、UTC 与精确证明的 deterministic collation。分页必须由用户排序字段加 Catalog stable entity/group key 构成全序。

| 运算 | release | positive-output dependency |
|---|---|---|
| Scan | 实际交付的 base/derived cells | 每个交付行的 row fact 与交付字段 cell facts |
| Select | 仅 TRUE 且最终交付的行/字段 | 保留行原有依赖，加 predicate dependency fields；FALSE/UNKNOWN 行不进入 |
| Project | 仅 `V` 中字段 | 保留子节点行级依赖，并加入 `V` 中 cell 依赖；不会丢失上游 Group/Join/Union 依赖 |
| Join | 实际匹配并交付的结果 cells | 匹配行对两侧 row 依赖、join 条件字段与交付字段；不含未匹配行 |
| Group | 实际交付 group 的可见 group/aggregate cells | 所有成员 row 依赖和全部 group-key fields，即使 key 不在 `V` |
| `COUNT(*)` | 一个派生 cell | 所有 group member row facts |
| `COUNT(a)/SUM(a)/MIN(a)/MAX(a)` | 一个派生 cell | 所有成员的参数 cell facts，包括 NULL 以及 MIN/MAX 的非极值输入 |
| Union-distinct | 实际交付 distinct class 的 cells | 已证明完整 tuple 无重复的 identical set-valued branch 可先折叠；其余 class 包含全部候选成员 row 依赖及全部 schema equivalence fields，即使字段不在 `V` |
| Page/Order | page 内实际交付 cells | 原样保留 page 内 annotation；不新增 page 外行、排序键、rank 或 absence 依赖 |

NULL 和 MIN/MAX 非极值输入的纳入是有意采用的 conservative operator-input
dependency，而不是这些输入对标量值具有 causal influence 的声明。不同
Union alternative 使用幂等 max composition；同一 candidate member 内的 row
与所有 equivalence fields 使用 same-proof composition，保持 operand exchange，
以及对 set-valued `R` 的 `R UNION DISTINCT R`/duplicate-branch collapse FactSet
不变性。普通 Scan 是 bag；其 tuple 若有重复，仍必须执行 duplicate
elimination，不能被该规则静态折叠。

`taskgate-query-normal-form-v3` 是 typed normal form：除 alias 删除、字段限定、AND/projection/group 归一化、aggregate 名称与类型归一化及稳定分页 tie-break 外，还静态检查 Scan schema、Join predicate、Union schema 和 collation profile。它不声称解决任意 SQL 等价。
