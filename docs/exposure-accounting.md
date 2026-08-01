# 任务级数据暴露记账

TaskGate 的核心预算主体是人类授权的根任务，而不是单条 SQL、单个 Agent
进程或一次数据库连接。系统在传统查询数、返回行数和数据库时间之外，维护
三个互相独立的事实账本：

- **accounted result exposure**（API/数据库兼容标签 `release`）：查询成功结算时，
  为准备逻辑发布而计费的原始或派生结果事实；它不证明 artifact 已可用或已被观察；
- **positive-output dependency**：按 V2 闭合代数规则参与已交付正向输出成功推导
  的基础 row/cell facts；API/数据库仍以 `influence` 作为兼容标签。
- **query outcome**：一个成功查询对应一个 FactID，绑定服务端规范化 QueryPlan、
  发布 FactSet 摘要和可见行数；不同命题即使都返回空集或 `0` 也不会合并。

令 `F_release_candidate(q)` 为 withheld execution 内派生的候选结果事实，
`F_release_accounted(q)` 为 settlement 已计费事实，`F_release_available(q)`
为 canonical artifact 达到 `AVAILABLE` 后可逻辑发布的事实。成功结算时前两者相等，且
`F_release_available(q) ⊆ F_release_accounted(q)`。promotion 永久失败时，已计费事实
可以从未变成可用事实。系统不跟踪 preview、下载或 Agent/人的实际观察；这些行为既不
增加也不撤销 release/dependency/outcome 费用。

这不是差分隐私预算，也不估算互信息或推断风险。它是一个版本化契约下的
确定性显式披露与命题计量模型。dependency 是保守 operator-input footprint：既不
要求是最小 causal provenance，也不声称等于数据库执行期间的完整 physical
read set。

V4 不改变下面任何 FactID 或收费语义，只改变物理实现：冻结 snapshot 中的 base facts 在发布时映射到 immutable ordinals，在线用精确压缩 bitmap 结算；少量 derived release/outcome 使用动态字典。V2/V3 通用代数继续作为解码 oracle，而不再是百万事实生产热路径。详见 [TaskGate V4](exposure-v4.md)。

## 事实身份

V1 Profile 中，一个事实由下列五元组标识：

```text
(data product, snapshot, entity key, field, value version)
```

其中：

- `data product` 是 Catalog 中的逻辑产品名；
- `snapshot` 是 Catalog 管理者发布的数据版本标签；
- `entity key` 是产品 `entity_key` 字段值的类型化 JSON SHA-256；
- `field` 是基础字段、`__row_exists__`，或规范化派生字段名；
- `value version` 是类型化 JSON 值的 SHA-256。

完整 FactID 再经 canonical 编码和 SHA-256，成为稳定语义身份。V4 的 Snapshot Compiler 按完整 hash 排序并在独立 segment 内分配 `uint32` ordinal；dictionary 保留可逆映射，bitmap 不是 probabilistic summary。Snapshot 是
发布契约，不是 PostgreSQL MVCC transaction ID；基础数据发生语义更新时，
Catalog 管理流程必须提升 snapshot。即使 snapshot 未变，字段值变化也会因
`value version` 不同而成为新事实，但这不能替代正确的数据版本管理。

对根任务 `T`，设三类已计量事实为 `K_release(T)`、`K_influence(T)` 和
`K_outcome(T)`。V3 查询观察到对应的三个集合，则增量费用是：

```text
delta_release   = |F_release(q,D)   - K_release(T)|
delta_influence = |F_influence(q,D) - K_influence(T)|
delta_outcome   = |F_outcome(q,D)   - K_outcome(T)|
```

三维分别与上限比较，不折叠成任意加权标量。`exposure_profile_version`
固定计量规则；不同 Profile 不能混入同一根账本。

## 可执行代数

V2 的规范性定义见 [Exposure Algebra V2：正式语义](exposure-algebra-v2.md)；以下表格只是便于阅读的摘要。

`internal/exposure` 为有限关系实现逐单元 dependency support 和逐行 annotation：

| 运算 | release | positive-output dependency |
|---|---|---|
| Projection | 实际输出字段 | 输出字段来源和保留行存在性 |
| Selection | 通过谓词后的输出 | 输出来源、保留行存在性、谓词字段 |
| Pagination | 当前页实际输出 | 当前页输出对应来源 |
| Join | 连接结果字段 | 两侧行存在性、连接键和输出字段来源；重复连接结果不重复计算同一 FactID |
| Union | 去重后的结果字段 | distinct class 全部候选 row dependency 与全部 schema 去重字段，包括隐藏字段 |
| `GROUP BY` | 实际可见的分组键和派生聚合事实 | 组内 row dependency 与全部 group key，包括隐藏 key |
| `COUNT/SUM/MIN/MAX` | 每个结果单元一个派生事实 | 完整逻辑参数输入；包含 NULL 与 MIN/MAX 非极值 |

派生聚合 FactID 绑定输出值与规范 witness commitment，避免相同数值但不同
dependency witness 被错误视为同一披露。代数测试覆盖
projection/selection rewrite、Join
multiplicity、split/merge、pagination、retry、aggregate 和 snapshot update。

NULL 与非极值的纳入是保守 operator-input dependency，不是 causal influence
声明。这里的 conservation 是在已定义 FactID 和 rewrite 集合内的集合等价，不是
“任意 SQL 等价查询都具有相同信息量”的声明。特别是，V2 base algebra 只记正向产生结果
的来源，不把 selection 失败行、未返回 group、page 外行、排序位置
或未命中行所隐含的负信息。PostgreSQL 物理计划读取但未进入正向输出推导的
数据也不属于该 footprint。V3 另对成功的规范化命题收费一单位，所以空/零
答案不再完全免费；但它不估算该命题揭示了多少 bit，也不是推断控制或差分隐私。

## V3 OutcomeFact

V3 保留全部 V2 release/dependency FactID，不改变其去重语义。Gateway 在完成
V2 observation 后增加一个 `FactOutcome`，其规范 payload 包含：

```text
(taskgate-exposure-v3,
 query normal-form version,
 SHA256(normalized QueryPlan),
 SHA256(visible row count + normalized release FactSet),
 visible row count)
```

normal form 绑定 Catalog namespace、snapshot/lineage、typed predicate、group/order/
page、NULL/bag、collation、UTC 和 exact numeric mode。对在线 INNER equijoin，SQL alias 先解析为 Catalog 稳定角色，JoinGraph 的 nodes、edges、edge 内 predicates 及 equality 两端规范排序后 deterministic binary fold 为现有二元代数。因此支持的 alias、大小写、Join 交换/括号/遍历顺序和 edge/predicate 顺序改写会归一到同一 digest；不同阈值或关系上下文会产生不同 digest。
因此同命题同结果重放为零增量，不同命题同结果各增加一个 outcome fact。

## Nested View 与授权语义

Phase B 不为 exposure ledger 新增一种关系运算。它把 Catalog semantic root 的
ordinary View dependency DAG 在授权时递归展开，并且只有 closure 能 flatten 为现有
typed SPJG/`join_many` QueryPlan 时才接受。Governed materialized View 是 opaque
terminal Product；不会追入 seed tables。于是 View 层级最终仍落到本页已经定义的
Projection、Selection、INNER equi-Join、Group 和 `COUNT/SUM/MIN/MAX` 规则，而不是
另建一套 provenance 语义。

View definition 允许 direct projection/rename、column-to-literal `AND` filters、
connected INNER equality JoinGraph（每 edge 可有多个 predicates）和上述聚合。一个
closure 最多一道 aggregate barrier，barrier 上方只能有纯单输入 projection/rename。
Expanded governed sources 最多 16；View depth 16、reachable nodes 64、dependency
edges 128、reachable definition bytes 1 MiB。递归/cycle、raw/foreign/system relation、
self/outer/cross/non-equality Join、CTE/subquery/set operation、scalar expression、window、
`HAVING`、aggregate-output filter/join/re-aggregation 等都 fail closed。
在 depth 16 的 operational ceiling 内，这是任意层数的 ordinary-View 递归展开，
不是固定的两层 rewrite。

已编译 Artifact 在查询时真正取代 semantic root scan：外层 public outputs 按
`Artifact.Output.FieldID` 合成到 expanded Scan/`join_many` 计划，然后进入现有
`CompileRelational` 和 exposure derivation。V4 将每个 terminal Product 绑到其真实
publication ordinals/sidecar，所以解码后的 base release/dependency 仍使用 terminal
FactID namespace/snapshot，而不是一个虚构的 root FactID。

签名 Grant 仍只批准 root Product；Gateway 每查询从已绑定 Artifact 派生
最小 terminal internal grants，并不修改持久授权。Root Scope 经同一 Output
FieldID 映射到 terminal，且必须覆盖每个 terminal mandatory scope；映射不全时
不执行、不预留、不计费。当前仅合成一个外层 semantic root，拒绝它与
其他 query-time Product 的 Join 以及 root 上的 order/page。已聚合 View 之上仅纯投影；
这些组合限制不改变 View 内部任意 connected equi-join/多 predicate edge 的支持。

Catalog 为每个 root 分开绑定 exact definition、transitive dependency、typed canonical
plan 和 ordered interface 四摘要。任务申请校验四者后，把每个 product 的
plan/dependency/interface 摘要组成规范 `taskgate-task-view-binding-v1`；该 digest 进入
签名 Manifest/Grant，并通过 Grant digest 传递式进入 semantic replay/receipt 的授权
上下文。它不会代替 `plan_digest`：前者回答“当前批准的是哪个 View closure”，后者
回答“本次查询命题是什么”。因此同一个 Agent QueryPlan 在不同已批准 closure 上不能
错误命中同一 semantic observation。

Legacy source-level `schema_digest` 只覆盖没有 `view_contract` 的 products，semantic
roots 使用 task-scoped closure；这避免无关 View 变更全局失效。相关漂移会产生
`VIEW_SEMANTIC_CHANGED`，并使 task 的正交 binding 状态不可逆地进入
`REQUIRE_REBIND`。旧 Grant 不能原地换语义；必须用更新 Catalog 创建新 task 并重新
OA 审批。旧 task 的 exact request 终态重放、既有 result artifact 和 receipt 仍按原
保留规则存在，因此 drift detection 不重写历史 exposure ledger。

## 在线执行

启用 V4 exposure Profile 的任务默认通过 `query_sql` 提交
`taskgate-reporting-sql-v1`。Gateway 先把受支持 SQL 无损 lowering 为 canonical
QueryPlan，再为该计划重新生成可见查询和 provenance companion；原始
SQL 不进入数据库执行。SDK、内部测试和确定性工作流可继续直接调用不在
普通 `tools/list` 中列出的 `execute_plan`，但两个入口共用同一 QueryPlan
编译、provenance、FactID 和结算链路：

```text
exact replay / View-binding check -> reserve
        -> semantic replay lookup / execute+stream -> derive bitmap
        -> stage encrypted Parquet -> settle + PENDING metadata/receipt
        -> canonical promote -> consumed/AVAILABLE -> result_id + summary
```

1. Gateway 先处理相同 `(task_id, request_id, request_digest)` 的 exact terminal replay。对新的 semantic request，它读取持久 binding status、重发现本任务 roots 并比对签名 binding；只有 `ACTIVE` 且摘要一致才在 Control PostgreSQL 中同时创建资源预算和 exposure reservation。
2. 先用绑定 task/grant/scope、typed normal form、Catalog/schema/View binding/publication/dictionary 和编译版本的 key 查询 committed semantic replay。命中只复用已提交 observation 和 `AVAILABLE` artifact，不执行 Business/provenance SQL；`PENDING`、过期、已清理或 key 非 `ACTIVE` 的 source 不可复用。
3. miss 时，Connector 在 Business PostgreSQL 的同一个只读 `REPEATABLE READ` 事务中先重新发现已绑定 View closure、核对预期 registry revision，再缓冲可见结果并流式读取 ordinal companion；Gateway 同时只保留当前 canonical group 的 support bitmap、稀疏 witness multiplicity 和查询级 dependency bitmap。检查与查询共享同一数据库快照，封闭 View replacement TOCTOU。最终可见投影编码为 Parquet，并在上传对象存储前用 `chunked-aes-gcm-v1` 客户端侧加密为 private staging 对象。
4. Gateway 生成 base release/dependency bitmap 及动态 derived release/outcome。未知 handle、越界 ordinal、manifest 不匹配、非规范 bitmap、overflow 或截断都 fail closed。
5. Go control path 从 Control PostgreSQL 读取已提交的三维 set，分别执行精确 `ANDNOT + popcount` 和 `OR`；同一 Control PostgreSQL 事务持久化 set 内容，并通过一次 root-head epoch CAS 原子发布。CAS 冲突重读 head 并重算全部 R/I/O。
6. 同一 Control PostgreSQL 事务提交 exposure/资源结算、`PENDING` artifact 元数据、semantic materialization、终态审计和 Ed25519 V6 回执；Control PG 不保存 Parquet bytes 或结果行。commit 后 Gateway 把 staging 对象幂等提升到确定性的 canonical key。canonical 对象创建成功即 `consumed`，随后 artifact 标记为 `AVAILABLE`，普通 query/get 只返回 `result_id`、行列数、期限和摘要。semantic replay 命中仍重新授权、扣普通资源、写审计/新回执，并为新 query 创建自己的 artifact。

promotion 在 Control commit 后中断时，持久的 `PENDING` intent 由启动和后台恢复继续；只有 staging 与已提交哈希/ETag 证据一致时才能幂等 promotion。恢复不重执行 SQL、不退款、不创建第二个 Receipt；staging 丢失或 canonical 证据冲突时 readiness fail closed，需先恢复正确对象证据或受审修复，当前不自动放弃/退款。`preview_result` 默认 20 行且单次最多 100 行，默认对大于 64 MiB 的 artifact 关闭；`deliver_result` 返回默认 5 分钟且不超过 artifact TTL 的 Gateway capability URL。两者以及实际下载都不是新的计费事件。

SQL 语法、授权或 lowering 失败在第 1 步的正式 reservation 和 Business
PostgreSQL 执行之前返回结构化、可修复错误，不扣 queries/rows/DBMS，也不扣
release/dependency/outcome。数据库已开始执行后的 timeout 或故障继续遵守
现有 failure-settlement 规则。

View drift 若在 reservation 前发现，同样不产生 reservation；若同事务二次检查才
发现，Gateway 以 `VIEW_SEMANTIC_CHANGED` 释放这笔资源/exposure reservation、把 task
binding 单向标记为 `REQUIRE_REBIND`，且不执行 visible/provenance SQL、不创建 artifact
或消费 exposure；该已预留请求仍以 released error record、审计和终态 Receipt 留证。
已经存在的 exact terminal request replay 不走新执行路径，因此仍返回首次持久化终态。

任一 exposure 维度超限时，整笔控制事务回滚：新事实、账本计数、artifact 元数据和 Receipt
都不落库，private staging 被清理，不创建 canonical 对象，也不向 Agent 返回 `result_id`、preview 或 delivery 能力。Business PostgreSQL 已经完成的物理工作仍在
资源遥测中独立处理。dependency 证据缺失、截断或无法规范化时同样 fail closed。

## 在线支持边界

| 能力 | 可执行代数 V2 | 在线 Gateway V2 |
|---|---:|---:|
| Projection / filter / order / limit | 是 | 单产品 SQL/QueryPlan；多输入不分页 |
| `GROUP BY`, `COUNT`, `SUM`, `MIN`, `MAX` | 是 | 单产品、Join 或 Union-Distinct 输入 |
| Join | 是 | SQL 及 QueryPlan `join_many`：2–16 个不同 Catalog 稳定角色的 connected INNER equi-join graph，operational complexity/DoS ceiling 内不限 graph 形状；每 edge 一个或多个 column-to-column equality；支持 10 表 Join，仍受 1 MiB 请求体、AST 和资源边界 |
| Nested View DAG | 不新增运算 | task-scoped ordinary View recursive expansion；governed materialized Product terminal；仅接受可 flatten 到本表 Projection/Selection/Join/Group/Aggregate 的 closure，最多一道 aggregate barrier，受 16/16/64/128/1 MiB sources/depth/nodes/edges/definition ceilings；展开计划实际进入 relational/V4 terminal ordinal 路径，query-time root Join/order/page 暂拒绝 |
| Union | 是 | 仅高级 QueryPlan：同产品两个过滤分支；显式完整 DISTINCT schema |
| 任意直接 SQL | 否 | 只有能无损 lowering 的 `taskgate-reporting-sql-v1` 可进入 exposure 链路 |
| `AVG`、窗口函数、子查询、CTE、set operation、递归 | 否 | exposure SQL 下关闭式拒绝 |
| self/outer/cross/non-equality Join、断开的 Join graph、多数据源或跨引擎查询 | 代数可嵌套（同快照条件） | 否 |

Catalog 中的 SQL allowlist 仍可包含更宽的传统资源控制片段，但 exposure
Profile 额外收窄到上表。默认 Demo Profile 已启用 exposure，因此
`query_sql` 必须先 lowering，不会绕开 provenance 路径；仅迁移前或明确禁用 exposure 的
resource-only grant 保留直接 SQL 兼容行为。原始 SQL 派生的 request digest 只用于
审计和幂等；FactID、OutcomeFact 和 semantic replay 始终绑定 canonical
QueryPlan/`plan_digest`，不绑定 SQL 文本哈希。

## 根任务与委托

根任务创建自己的 `exposure_ledgers` 行。子任务的签名 Manifest 和 Grant
同时绑定 `root_task_id` 与 `parent_task_id`，并必须满足：

- 委托发起者拥有且仍可使用父任务；
- 目标是已启用的 query Principal；
- 产品、字段、Scope、敏感级别、期限和全部预算只能收缩；
- Catalog、Datasource、Schema、task View binding 证据及 exposure Profile 保持一致；
- 每次执行都重新验证完整祖先链仍为 ACTIVE。

所有后代通过 `root_task_id` 写入同一个三维 root head。
因此父子 Agent 查询相同事实时，第二次增量费用为零；并发子任务由根账本
epoch CAS 原子结算，不能分别花掉同一剩余额度。撤销根任务会阻止新的
后代查询。已终态的相同 `request_id` 重放只返回首次的 `result_id` 和元数据，不再次执行、不创建第二个对象或收费。
若子 Grant 进一步缩小 exposure 上限，子任务只能在该签名的绝对任务族上限内
增加新事实；已有事实的零增量读取仍可重放。

默认 Compose 只配置 Alice 这个 query Principal；跨主体委托需要部署者再
注册一个启用的 query Principal。委托机制不是自主创建新身份的接口。

## 存储与凭证

核心控制表如下：

| 表 | 作用 |
|---|---|
| `tasks` | 保存 root/parent lineage |
| `view_binding_sets` | 内容寻址保存 task-scoped canonical product contracts；exact canonical JSON 不由 JSONB 重写 |
| `task_view_dependencies` | immutable task→reachable dependency 反向索引证据 |
| `task_view_binding_status` | 与 task lifecycle 正交的 `ACTIVE -> REQUIRE_REBIND` 单向状态和首次 observed digest |
| dictionary manifest/chunks | 冻结 base FactID↔ordinal 双射、segment bounds 和审计用 cold payload |
| dynamic dictionaries | 少量 derived release/outcome 的精确 hash/payload 身份 |
| bitmap containers/set manifests | `(dictionary, segment, ordinal>>16)` 的不可变内容寻址容器及集合根 |
| root exposure head | 每个根任务的 Profile、三维 set manifest、已用计数和单一 epoch |
| committed observations/materializations | observation digest、O(1) query reference 和 semantic replay artifact 引用；复用只接受 `AVAILABLE` source |
| `result_artifacts` | `result_id`、对象位置、schema/ACL、行列数、TTL、key ID、明文 Parquet hash、密文 object hash 和生命周期；不含 Parquet bytes |

客户端侧加密 Parquet 对象保存在 TaskGate 管辖的 S3/MinIO 私有 bucket；Control PG 只保存上述元数据。bucket 必须禁用 versioning，以保证 TTL/purge 删除实际 bytes；非 loopback 的交付基址必须为 HTTPS，日志/APM 必须脱敏 capability URL 的 `token`。查询 V6 回执签名绑定 `root_task_id`、Profile、dictionary set、三类 effect digest、actual/charged counts、root epoch、明文 Parquet result digest 和 observation SHA-256；密文 object SHA-256 作为独立的存储完整性元数据。回执不包含原始 FactID 或结果行；有权审计方可从 cold dictionary + bitmap 无损恢复 canonical FactID。旧回执 verifier 与 legacy PostgreSQL ciphertext 读取路径仅为迁移兼容，生产 main 对新查询强制使用 artifact，普通 query/get 不返回 legacy `rows`。

到达 artifact `expires_at` 后，metadata lookup、preview 和 delivery 均关闭。TTL 或管理员 purge 先删除 canonical 对象，再把 Control 元数据 tombstone；active legal hold 阻止对象进入 retention 清理，但不延长 `expires_at`，也不自动阻止独立的 key-erasure 流程。查询记录、Receipt 和审计证据继续保留。

## 证据与限制

- `make eval-exposure` 运行 ground truth、等价改写、确定性
  anti-arbitrage cases 和计费基线；V2 单元/性质测试另覆盖 typed NULL、multi-key Join、五表 tree-shape 归一、十表链、star/cycle graph、alias/edge/predicate/equality-direction 置换、JoinGraph digest、Union commutativity/idempotence、Group NULL 与嵌套闭包。
- `make verify` 运行真实 PostgreSQL race/integration suite，包括并发结算、
  委托共享账本、重放及超限不产生 canonical artifact；View suite 另覆盖
  五类固定 seed、各 64-case `testing/quick` properties：determinism、alias invariance、
  join-order invariance、nested/direct equivalence 和 semantic/interface/dependency drift；以及
  cycle/limit/aggregate barrier、materialized terminal 和 transitive
  `CREATE OR REPLACE VIEW` detection。
- `make formal` 检查 `ExposureLedger.tla` 的三预算安全、exact novel charge、
  settle-before-release、retry idempotence 和 task-family non-amplification；这里的 release 对应 settle 后的 canonical promotion，不对应下载。

现有 169.3 s novel、154.1 s replay 和 9.8 GiB Gateway 峰值属于 legacy 全量 FactSet/逐事实存储诊断，不是 V4 结果。V4 的 ≤4 s novel P95、≤150 ms semantic replay P95、≤512 MiB Gateway peak 等数字目前是硬验收门槛；完成固定环境重跑前不能写成已实现 SLO。View property tests 也是 bounded grammar 的实现证据，不是任意 SQL 的形式化语义证明或企业规模性能实验。系统也不提供差分隐私、推断控制、可变源/任意 SQL provenance 或跨 Gateway 执行租约。
