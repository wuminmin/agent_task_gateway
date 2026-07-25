# 任务级数据暴露记账

TaskGate 的核心预算主体是人类授权的根任务，而不是单条 SQL、单个 Agent
进程或一次数据库连接。系统在传统查询数、返回行数和数据库时间之外，维护
两个互相独立的事实账本：

- **release exposure**：已经交付给任务族的原始或派生结果事实；
- **source influence**：对这些结果的选择、连接或聚合产生影响的来源事实。

这不是差分隐私预算，也不估算互信息或推断风险。它是一个版本化契约下的
确定性显式披露计量模型。

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

完整 FactID 再经 JSON 编码和 SHA-256，成为控制库的去重键。Snapshot 是
发布契约，不是 PostgreSQL MVCC transaction ID；基础数据发生语义更新时，
Catalog 管理流程必须提升 snapshot。即使 snapshot 未变，字段值变化也会因
`value version` 不同而成为新事实，但这不能替代正确的数据版本管理。

对根任务 `T`，设两类已知事实分别为 `K_release(T)` 和
`K_influence(T)`。查询观察到的集合为 `F_release(q,D)` 与
`F_influence(q,D)`，则 V1 的增量费用是：

```text
delta_release   = |F_release(q,D)   - K_release(T)|
delta_influence = |F_influence(q,D) - K_influence(T)|
```

两维分别与上限比较，不折叠成任意加权标量。`exposure_profile_version`
固定计量规则；不同 Profile 不能混入同一根账本。

## 可执行代数

V2 的规范性定义见 [Exposure Algebra V2：正式语义](exposure-algebra-v2.md)；以下表格只是便于阅读的摘要。

`internal/exposure` 为有限关系实现逐单元来源集合和逐行 lineage：

| 运算 | release | source influence |
|---|---|---|
| Projection | 实际输出字段 | 输出字段来源和保留行存在性 |
| Selection | 通过谓词后的输出 | 输出来源、保留行存在性、谓词字段 |
| Pagination | 当前页实际输出 | 当前页输出对应来源 |
| Join | 连接结果字段 | 两侧行存在性、连接键和输出字段来源；重复连接结果不重复计算同一 FactID |
| Union | 去重后的结果字段 | 所有能产生该结果 tuple 的来源并集 |
| `GROUP BY` | 分组键和派生聚合事实 | 组内行存在性、分组字段及聚合输入字段 |
| `COUNT/SUM/MIN/MAX` | 每个结果单元一个派生事实 | 产生该值的唯一来源事实集合 |

派生聚合 FactID 绑定输出值与有序来源 Hash 集，避免相同数值但不同来源被
错误视为同一披露。代数测试覆盖 projection/selection rewrite、Join
multiplicity、split/merge、pagination、retry、aggregate 和 snapshot update。

这里的 conservation 是在已定义 FactID 和 rewrite 集合内的集合等价，不是
“任意 SQL 等价查询都具有相同信息量”的声明。特别是，V1 只记正向产生结果
的来源，不计量从空结果、排序位置或未命中行推断出的负信息。

## 在线执行

启用 exposure Profile 的任务只能通过受支持的结构化 `execute_plan` 路径
释放数据。Gateway 为一个 QueryPlan 生成可见查询和 provenance companion，
然后执行：

```text
reserve -> execute/buffer -> derive provenance -> settle -> release
```

1. 在 Control PostgreSQL 中同时创建资源预算和 exposure reservation。
2. 在 Business PostgreSQL 的同一个只读 `REPEATABLE READ` 事务中执行可见
   查询及 provenance companion，保证两份结果看到同一数据库快照。非聚合
   计划让两条语句使用相同隐藏投影、实体键排序和行上限，并核对实体键集合；
   聚合 companion 对来源上限多请求一行，以可靠识别截断。
3. 结果留在 Gateway 内存中；稳定实体键只用于计量，不返回给未获批客户端。
4. Gateway 从两份结果构造并规范化 release/influence FactID 集。
5. Control PostgreSQL 锁定根账本，使用不可变事实表的唯一键只插入 novel
   facts，并分别检查两个上限。
6. exposure 结算、资源结算、AES-GCM 结果、终态审计和 Ed25519 V4/V5 回执在
   同一事务提交后，Gateway 才向客户端释放结果。

任一 exposure 维度超限时，整笔控制事务回滚：新事实、账本计数和结果密文
都不落库，缓冲结果也不返回。Business PostgreSQL 已经完成的物理工作仍在
资源遥测中独立处理。来源证据缺失、截断或无法规范化时同样 fail closed。

## 在线支持边界

| 能力 | 可执行代数 V2 | 在线 Gateway V2 |
|---|---:|---:|
| Projection / filter / order / limit | 是 | 单产品 QueryPlan |
| `GROUP BY`, `COUNT`, `SUM`, `MIN`, `MAX` | 是 | 单产品 QueryPlan |
| Join | 是 | 尚未接入在线 compiler |
| Union | 是 | 尚未接入在线 compiler |
| 任意直接 SQL | 否 | exposure grant 下关闭式拒绝 |
| `AVG`、窗口函数、子查询、递归 | 否 | exposure grant 下关闭式拒绝 |
| 多数据源或跨引擎查询 | 否 | 否 |

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

所有后代通过 `root_task_id` 写入同一组 release/influence 事实和同一根账本。
因此父子 Agent 查询相同事实时，第二次增量费用为零；并发子任务由根账本
`FOR UPDATE` 锁串行结算，不能分别花掉同一剩余额度。撤销根任务会阻止新的
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
| `exposure_ledgers` | 每个根任务的 Profile、双上限和双已用计数 |
| `query_exposure_reservations` | 查询的估计、实际、增量费用和观察摘要 |
| `exposure_facts` | 按 root + ledger kind + FactID Hash 唯一且不可变的已知事实 |

查询 V4 回执签名绑定 `root_task_id`、Profile、两类 actual facts、两类 charged
facts 和规范化 observation SHA-256；V2 planner 的 V5 还绑定候选、选择、snapshot
bundle、planner 和联合 Effect。回执不包含原始 FactID 或结果行；审计员
可验证收费证据与终态审计位置，但不能据此恢复敏感值。

## 预算规划

`plan_exposure` 只接受 `taskgate-exposure-v2`；V1 标量规划路径已删除。
客户端提交 required-output 契约和候选 QueryPlan，Gateway 执行候选并生成
精确 Effect 与固定版本 utility。完整定义见 [exposure-v2.md](exposure-v2.md)。
V2 从每个 requirement 至多选一个，并求解：

```text
maximize sum(server-derived required-output utility)
subject to |union(candidate.release) - root_history.release| <= remaining_release
           |union(candidate.influence) - root_history.influence| <= remaining_influence
```

V2 候选成本、FactID 和 utility 标量均由服务端生成。Planner 使用稠密双
bitset、精确集合并、集合包含支配和稳定 tie-break。required-output 契约由调用
请求声明并进入回执证据；它不是对语义答案真值的认证。

## 证据与限制

- `make eval-exposure` 运行 ground truth、等价改写、确定性
  anti-arbitrage cases、计费基线和 planner oracle；V2 单元/性质测试另覆盖 typed NULL、multi-key Join、Union commutativity/idempotence、Group NULL 与嵌套闭包。
- `make verify` 运行真实 PostgreSQL race/integration suite，包括并发结算、
  委托共享账本、重放及超限结果不落库。
- `make formal` 检查 `ExposureLedger.tla` 的双预算安全、exact novel charge、
  settle-before-release、retry idempotence 和 task-family non-amplification。

当前 corpus 是可审计的开发证据，不是论文规模的性能结论。尚未完成的外部
实验包括完整 TPC-H/TPC-DS 查询族、第二数据库引擎、多 Agent 任务正确率、
长期账本空间增长及 lineage/去重吞吐开销。系统也不提供差分隐私、推断控制、
自动 snapshot/CDC、任意 SQL provenance 或跨 Gateway 执行租约。
