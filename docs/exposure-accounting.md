# 任务级数据暴露记账

TaskGate 的核心预算主体是人类授权的根任务，而不是单条 SQL、单个 Agent
进程或一次数据库连接。系统在传统查询数、返回行数和数据库时间之外，维护
三个互相独立的事实账本：

- **release exposure**：已经交付给任务族的原始或派生结果事实；
- **positive-output dependency**：按 V2 闭合代数规则参与已交付正向输出成功推导
  的基础 row/cell facts；API/数据库仍以 `influence` 作为兼容标签。
- **query outcome**：一个成功查询对应一个 FactID，绑定服务端规范化 QueryPlan、
  发布 FactSet 摘要和可见行数；不同命题即使都返回空集或 `0` 也不会合并。

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
page、NULL/bag、collation、UTC 和 exact numeric mode。支持的 alias、大小写和
遍历顺序改写会归一到同一个 digest；不同阈值或关系上下文会产生不同 digest。
因此同命题同结果重放为零增量，不同命题同结果各增加一个 outcome fact。

## 在线执行

启用 V4 exposure Profile 的任务只能通过受支持的结构化 `execute_plan` 路径
释放数据。Gateway 为一个 QueryPlan 生成可见查询和 provenance companion，
然后执行：

```text
reserve -> semantic replay lookup / execute+stream -> derive bitmap -> settle -> release
```

1. 在 Control PostgreSQL 中同时创建资源预算和 exposure reservation。
2. 先用绑定 task/grant/scope、typed normal form、Catalog/schema/publication/dictionary 和编译版本的 key 查询 committed semantic replay。命中只复用已提交 observation/result，不执行 Business/provenance SQL。
3. miss 时，在 Business PostgreSQL 的同一个只读 `REPEATABLE READ` 事务中缓冲可见结果并流式读取 ordinal companion；Gateway 同时只保留当前 canonical group 的 support bitmap、稀疏 witness multiplicity 和查询级 dependency bitmap。
4. Gateway 生成 base release/dependency bitmap 及动态 derived release/outcome。未知 handle、越界 ordinal、manifest 不匹配、非规范 bitmap、overflow 或截断都 fail closed。
5. Control PostgreSQL 对三个维度分别执行精确 `ANDNOT + popcount` 和 `OR`，并通过一次 root-head epoch CAS 原子发布。CAS 冲突重读 head 并重算全部 R/I/O。
6. exposure/资源结算、AES-GCM 结果、semantic materialization、终态审计和 Ed25519 V6 回执在同一事务提交后，Gateway 才向客户端释放结果。semantic replay 命中仍重新授权、扣普通资源、写审计/新回执并用新 query AAD 重加密。

任一 exposure 维度超限时，整笔控制事务回滚：新事实、账本计数和结果密文
都不落库，缓冲结果也不返回。Business PostgreSQL 已经完成的物理工作仍在
资源遥测中独立处理。dependency 证据缺失、截断或无法规范化时同样 fail closed。

## 在线支持边界

| 能力 | 可执行代数 V2 | 在线 Gateway V2 |
|---|---:|---:|
| Projection / filter / order / limit | 是 | 单产品 QueryPlan；多输入暂不分页 |
| `GROUP BY`, `COUNT`, `SUM`, `MIN`, `MAX` | 是 | 单产品、Join 或 Union-Distinct 输入 |
| Join | 是 | 两个不同 Catalog 稳定角色的 INNER equijoin |
| Union | 是 | 同产品两个过滤分支；显式完整 DISTINCT schema |
| 任意直接 SQL | 否 | exposure grant 下关闭式拒绝 |
| `AVG`、窗口函数、子查询、递归 | 否 | exposure grant 下关闭式拒绝 |
| 嵌套 Join/Union、自连接、多数据源或跨引擎查询 | 代数可嵌套（同快照条件） | 否 |

Catalog 中的 SQL allowlist 仍可包含更宽的传统资源控制片段，但 exposure
Profile 额外收窄到上表。默认 Demo Profile 已启用 exposure，因此
`query_sql` 不会绕开 provenance 路径；仅迁移前或明确禁用 exposure 的
resource-only grant 保留直接 SQL 兼容行为。

## 根任务与委托

根任务创建自己的 `exposure_ledgers` 行。子任务的签名 Manifest 和 Grant
同时绑定 `root_task_id` 与 `parent_task_id`，并必须满足：

- 委托发起者拥有且仍可使用父任务；
- 目标是已启用的 query Principal；
- 产品、字段、Scope、敏感级别、期限和全部预算只能收缩；
- Catalog、Datasource、Schema 证据及 exposure Profile 保持一致；
- 每次执行都重新验证完整祖先链仍为 ACTIVE。

所有后代通过 `root_task_id` 写入同一个三维 root head。
因此父子 Agent 查询相同事实时，第二次增量费用为零；并发子任务由根账本
epoch CAS 原子结算，不能分别花掉同一剩余额度。撤销根任务会阻止新的
后代查询。已终态的相同 `request_id` 重放只读取首次结果，不再次执行或收费。
若子 Grant 进一步缩小 exposure 上限，子任务只能在该签名的绝对任务族上限内
增加新事实；已有事实的零增量读取仍可重放。

默认 Compose 只配置 Alice 这个 query Principal；跨主体委托需要部署者再
注册一个启用的 query Principal。委托机制不是自主创建新身份的接口。

## 存储与凭证

核心控制表如下：

| 表 | 作用 |
|---|---|
| `tasks` | 保存 root/parent lineage |
| dictionary manifest/chunks | 冻结 base FactID↔ordinal 双射、segment bounds 和审计用 cold payload |
| dynamic dictionaries | 少量 derived release/outcome 的精确 hash/payload 身份 |
| bitmap containers/set manifests | `(dictionary, segment, ordinal>>16)` 的不可变内容寻址容器及集合根 |
| root exposure head | 每个根任务的 Profile、三维 set manifest、已用计数和单一 epoch |
| committed observations/materializations | observation digest、O(1) query reference 和 semantic replay 密文 |

查询 V6 回执签名绑定 `root_task_id`、Profile、dictionary set、三类 effect digest、actual/charged counts、root epoch、result digest 和 observation SHA-256。回执不包含原始 FactID 或结果行；有权审计方可从 cold dictionary + bitmap 无损恢复 canonical FactID。旧回执 verifier 保留，但不复用 V6 digest domain。

## 证据与限制

- `make eval-exposure` 运行 ground truth、等价改写、确定性
  anti-arbitrage cases 和计费基线；V2 单元/性质测试另覆盖 typed NULL、multi-key Join、Union commutativity/idempotence、Group NULL 与嵌套闭包。
- `make verify` 运行真实 PostgreSQL race/integration suite，包括并发结算、
  委托共享账本、重放及超限结果不落库。
- `make formal` 检查 `ExposureLedger.tla` 的三预算安全、exact novel charge、
  settle-before-release、retry idempotence 和 task-family non-amplification。

现有 169.3 s novel、154.1 s replay 和 9.8 GiB Gateway 峰值属于 legacy 全量 FactSet/逐事实存储诊断，不是 V4 结果。V4 的 ≤4 s novel P95、≤150 ms semantic replay P95、≤512 MiB Gateway peak 等数字目前是硬验收门槛；完成固定环境重跑前不能写成已实现 SLO。系统也不提供差分隐私、推断控制、可变源/任意 SQL provenance 或跨 Gateway 执行租约。
