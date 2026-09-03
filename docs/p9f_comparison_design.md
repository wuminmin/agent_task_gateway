# P9.F 对比研究设计（DProvDB 头对头主线）——草案 v1（2026-09-02）

作者 2026-09-02 强证据裁决：menu item 4 取头对头分支；BDG+DFC 组合为降级备选。

## 侦察结论（实查 gh, 2026-09-02）

- 工件：github.com/DProvDB/DProvDB（SIGMOD'24 正式工件, Scala/Java, Chorus 框架子模块, install.sh/run.sh, 2026-05 仍在更新）。
- 栈：JVM + sbt + Chorus + PostgreSQL。WSL 可装（磁盘/网络预算待量, GOPROXY 不覆盖 JVM 生态——走 maven central 直连或镜像）。

## 可比口径（不可比处如实声明）

保证不可通约——DProvDB 计 DP 的 ε 损耗（噪声视图), BDG 计精确声明身份集合（无噪声）。头对头只在三条**机制形状**维度上比, 论文措辞必须把不可比性放在对比表第一行：

1. **每查询开销**：同宿主、同 PostgreSQL、同查询序列（映射到两系统各自可执行的等价形状）下的中位延迟与相对直连开销。回应 M1"无外部成本参照"。
2. **预算耗尽轨迹**：单分析师预算从满到拒的接受/拒绝序列形状（DProvDB ε 递减 vs BDG 三维台账递减）；两侧都用各自保守先验预算, 不拟合。
3. **重复/相似查询语义**：DProvDB 靠 cached noisy view 复用省 ε；BDG 精确重放零计费。同一重复序列下的累计预算消耗曲线——这是 BDG 主张（重放不双计）最有内容的外部对照。

## 执行面（顺序）

1. WSL 装栈 + 复现 DProvDB 默认实验（run.sh 原样跑通=门禁 0）；量安装体积与一次实验时长。
2. 设计共同查询序列：以 DProvDB 论文的 workload 为基（其可执行面窄——聚合查询）, 映射到 BDG 的 expense/result_heavy 等价聚合；映射规则文档化, 两侧 SQL 并排入 supplement。
3. BDG 侧按 pilot 严谨档跑同序列（3 部署×30 样本）；DProvDB 侧样本量对齐。
4. 产物：对比表（三口径）+ 不可比声明段 + 复现脚本。campaign_class=pilot。

## 降级条件与备选

门禁 0 失败（栈装不起/工件跑不通, 时间盒 1 个工作日）→ 降级 BDG+DFC/Passant 组合实验
（本仓已有 Passant 语义比较器集成面, 双执行展示分层互补+叠加开销）, 并在台账记录降级根因。

## 依赖与排期

- 不改 catalog、不触发 binding（比较在各自系统内跑）→ 不等 P9.G。
- 排期在 pilot-benign-06 注册与 P9.E 预实测门之后；JVM 安装等重 IO 不与测时延 run 并行。

## 门禁 0 通过记录与共同序列草案（2026-09-03）

复现链八层（台账 P9F-* 行，全直接证据）：psql 空跑假 RC/半截 conda 环境/Conda ToS/
sbt2-JDK11 矛盾（官方 sbt 1.9.9 二进制解）/chorus 未发布/.m2 root 属主/双 main 交互选择
（无头环境批量入口=runMain edu.Experiments.Experiment）/adult 库缺失。EQW@adult RC=0 44s。

### 共同查询序列（口径 3——重复查询语义，主对比面）

DProvDB 侧：RRQ 任务在 adult.age 上发随机 range count 查询（DP 噪声答案，ε 从预算扣减；
重复/相似查询由 provenance 表复用已付噪声视图省 ε）。
BDG 侧等价：expense_detail/amount 或 result_heavy/quantity 上同形 range count
（count 聚合在片段内 ✓），同 root 重发同语义查询=零计费重放。
序列：N=50 查询、重复率 50%（25 唯一+25 重复），两侧同序列同种子。
读数：DProvDB 累计 ε 消耗曲线（重复省多少）vs BDG 三维台账消耗曲线（重复零增）。
不可比声明置表首：ε 是噪声隐私损耗、BDG 是精确披露计数，比较的是"重复查询的预算折扣机制形状"。

### 口径 1（每查询开销）与口径 2（耗尽轨迹）

口径 1：同宿主同库（容器 PG）上两系统各自执行其可执行形态的 range count，报中位墙钟。
口径 2：DProvDB 预算耗尽（ε→0 拒答）步序 vs BDG 台账触顶拒绝步序（各自保守先验预算）。
