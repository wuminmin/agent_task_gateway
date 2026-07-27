结论：可以投 TKDE，但应把它当成一篇基于 TaskGate 重新立项的数据库论文，而不是把现有 TDSC 稿换模板。当前版本直接投稿属于 No-Go，主要风险不是工程缺陷，而是主贡献仍是授权网关集成，缺少 TKDE 期待的数据语义、算法、复杂度/性质证明和完整实验。

## 推荐定位

建议题目：

> **TaskGate: Task-Bound Positive-Output Dependency Accounting for Autonomous Database Agents**

核心问题改成：

> 现有授权只能约束 Agent“可以查询什么”，却不能约束一个自适应、多查询、多 Agent 任务累计获得了多少不同的数据事实。

定义根任务 \(T\) 已知事实集合 \(K_T\)，查询实际释放的规范化事实集合 \(F(q,D)\)，则增量消耗为：

\[
\Delta_T(q)=\mu_\pi\!\left(F(q,D)\setminus K_T\right)
\]

其中 \(\pi\) 是版本化计量 profile。事实身份至少绑定：

\[
(\text{data product},\text{snapshot},\text{entity key},
\text{field},\text{value version})
\]

不要只给一个任意加权标量。更稳妥的是双账本：

- `release exposure`：Agent 实际收到的原始或派生事实；
- `positive-output dependency`：按声明的闭合代数规则参与已交付正向输出成功
  推导的保守 row/cell fact footprint；内部兼容名可保留 `influence`。

扫描百万行返回一个 `COUNT(*)` 时，release 可能只有一个聚合事实，但 dependency
不能被误记为一个 fact。它不声称是最小 causal provenance 或完整 physical read set。

## 如何避开撞车

| 已有方向 | 已经覆盖 | 你需要强调的差异 |
|---|---|---|
| [Query-Based Data Pricing](https://homes.cs.washington.edu/~magda/papers/koutris-pods12.pdf)、[QIRANA](https://pages.cs.wisc.edu/~paris/papers/qirana.pdf)、[ARIA](https://doi.org/10.1109/ICDE65448.2025.00235) | 查询定价、属性权重、等价查询和防价格套利 | 不是货币定价，而是人类授权任务的状态化显式暴露与执行期守恒 |
| [DProvDB](https://arxiv.org/abs/2309.10240)、[CacheDP](https://www.vldb.org/pvldb/vol16/p574-mazmudar.pdf) | 历史查询、缓存、累计 DP 隐私预算 | 不计算 \(\epsilon\)，而是确定性的 contract-defined disclosure atoms |
| 数据 provenance | Join、聚合、派生结果的来源追踪 | provenance 是计量底座；新贡献是任务账本、增量结算和委托守恒 |
| 细粒度访问控制 | 行过滤、列掩码和单次授权 | 解决跨查询、分页、缓存、重试和子 Agent 的累计消费 |

因此不要声称：

- 首次按字段权重计费；
- 首次让同一信息只收费一次；
- 首次保证等价 SQL 不套利；
- 首次提出非货币数据预算；
- 首次结合 provenance 与 Gateway。

可守住的新颖性应是：

> 以人类授权的根任务为预算主体，为显式数据释放定义可组合的 exposure effect，并在受限关系代数、在线执行和多 Agent 委托图上维持任务级守恒。

## TKDE 版本至少需要四项贡献

1. **Exposure algebra**

   覆盖 SPJA，加一组明确的 `GROUP BY/COUNT/SUM/MIN/MAX`。定义 projection、selection、Join、Union、聚合、数据更新和派生事实的收费语义。不要声称支持任意 SQL 等价。

2. **在线计量算法**

   SQL 编译为带 provenance 的关系计划，执行结果先留在 Gateway 内，计算实际增量暴露，再原子结算并释放：

   `reserve → execute/buffer → derive provenance → settle → release`

   这样超预算结果可以不交付，而物理执行成本仍单独记录。

3. **性质与证明**

   在定义的查询片段和 rewrite 集合内证明：

   - rewrite invariance；
   - Join multiplicity conservation；
   - split/merge 与 pagination conservation；
   - cache/retry idempotence；
   - task-family delegation non-amplification；
   - 并发结算下的预算安全。

4. **可复现的系统证据**

   用独立 oracle、等价改写、反套利用例、有限状态模型，以及真实
   PostgreSQL 全路径开销与并发实验验证前述语义和结算性质。

## 当前仓库的复用程度

系统骨架有价值，但语义核心尚不存在：

- 当前预算仍只有查询数、行数和 DB 时间：[budget.go](/home/wmm/agent-scope/task_gateway/internal/domain/budget.go:14)。
- 论文也明确承认它不是隐私或信息预算：[main.tex](/home/wmm/agent-scope/task_gateway/paper/tdsc/main.tex:1142)。
- AST analyzer 已能传播产品和列依赖，可作为 lineage 起点：[ast.go](/home/wmm/agent-scope/task_gateway/internal/sqlpolicy/ast.go:18)。
- 事务式预留—结算可以直接扩展：[query.go](/home/wmm/agent-scope/task_gateway/internal/gateway/query.go:193)。
- 向量预算 TLA+ 模型可保留验证并发账本：[VectorBudget.tla](/home/wmm/agent-scope/task_gateway/formal/VectorBudget.tla:20)。

大致可复用：控制面 60%–75%，SQL 分析 35%–50%，正文只有约 20%–30%。缺失的关键实现是稳定行/实体 ID、snapshot、逐结果 provenance、根任务知识账本和子 Agent 委托。

## 实验门槛

建议设置四个 RQ：

- RQ1：计量结果与独立的 positive-output dependency ground truth 是否一致？
- RQ2：数百至上千组等价改写的 charge 是否一致？
- RQ3：Join 放大、查询拆分、重叠分页、缓存、重试和子 Agent 是否可套利？
- RQ4：lineage、历史去重和账本分别增加多少时间、空间和吞吐开销？

基线至少包括 query count、returned rows、serialized bytes、weighted cells、无历史去重的 provenance，以及完整方案。建议 PostgreSQL 加第二个引擎，并使用完整 TPC-H/TPC-DS 查询族、等价 SQL 变形集和多 Agent 任务轨迹。现有六条 TPC-derived 查询和未完成的性能结果不足以投稿。

## 论文压缩方案

TKDE 主文建议控制在 12 页：

- Introduction：1 页
- Related Work + Problem Model：1.5 页
- Exposure Algebra：2 页
- Algorithms and Proofs：2.5 页
- System：1 页
- Evaluation：3 页
- Limitations/Conclusion：0.5 页
- References：约 0.5–1 页

人工审批 UI、签名格式、密钥管理、六个分离的 TLA+ 模型和完整故障矩阵移到 supplement。现稿已经约 14 页，不能在原稿上继续加章节。

## 实际投稿方式

截至 2026 年 7 月 23 日，最贴近的 DK-GenAI 专刊已于 2026 年 6 月 30 日截止，因此应投全年滚动的 Regular Track。该方向可对应 TKDE 的 data-management methodology、query processing、database integrity/security 和 performance evaluation。[TKDE 官方范围](https://www.computer.org/digital-library/journals/tk/cfp-ieee-transactions-on-knowledge-data-engineering)

投稿注意：

- 使用 IEEE 期刊双栏模板；摘要 100–200 words，不能含公式或引用。[IEEE CS Author Guidelines](https://www.computer.org/publications/author-resources)
- Regular 按 12 个 formatted pages 准备，摘要和参考文献都计入；超出部分目前为 220 美元/页。[2026 IEEE 费用表](https://journals.ieeeauthorcenter.ieee.org/wp-content/uploads/sites/7/IEEE-Article-Processing-Charges-List.pdf)
- 通过 [IEEE Publishing Portal](https://journals.ieeeauthorcenter.ieee.org/submit-your-article-for-peer-review/ieee-publishing-portal/) 选择 TKDE 和 Regular Paper。
- 默认单匿名；可以申请双匿名，但由 EIC 决定。
- Traditional 模式不收 OA APC；可选 OA 当前为 2,800 美元。[IEEE OA 费用](https://open.ieee.org/for-authors/article-processing-charges/)
- 不得并投；如果与会议稿、预印本或当前 TDSC 版本存在关系，要提交旧版本及逐项差异说明。
- 代码和数据公开不是强制，但 TKDE 明确鼓励 DataPort 和 Code Ocean；这篇论文最好提供查询变形集、Agent traces、原始结果和复现实验脚本。

最终 Go/No-Go 判断：

- **现在直接投：No-Go，desk reject 风险高。**
- **完成 exposure algebra、受限 SQL 证明、在线结算、多 Agent 守恒和完整实验后：符合 TKDE 主航道。**
- 最关键的策略是：把 Agent 当作新型数据库 workload，把 exposure accounting、关系语义和原子结算当作论文主体；不要把“Gateway 产品”或“计费标准”当作主体。
