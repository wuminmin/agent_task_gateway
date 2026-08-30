# P8 — 新证据程序（作者 2026-08-30 决定：投入 (a)(b)(c)(d)）

目标：一次新的发表级 campaign（契约 v1.12）覆盖审稿人第 11–17 轮反复要求的全部新证据；期间所有主张随证据改。

## 范围与设计（每项标注：改什么、证据形态、边界）
- **(b) 片段扩展**：HAVING（分组后选择；依赖规则同 Select，谓词原子作用于派生值）、SELECT *（降低时展开为 Product 列表）、精确 AVG（数值/整数→numeric，派生事实表达式 avg，依赖同 SUM/COUNT）、COUNT(DISTINCT col)（依赖=参数列）。同步：`internal/sqllowering`、`internal/queryplan`、`internal/exposure`（V2 算子规则）、`internal/viewcompiler`（如相关）、RQ1 oracle 与生成计划参考求值器、TLC 不变（语义层）。重跑：TPC-H/SSB/代理 SQL 可降低性、RQ1/RQ2、生成计划差分、v5 链。
- **(d1) 派生优化**：先剖析 execute_and_derive 的 Go 侧（序数流消费、见证合并、编码），并行化或算法改进，**不改语义**（差分套件为守门）；在新 campaign 中按发表协议重测。
- **(d2) 三源 join**：新增 Product（orders–lineitem–nonce/或 star）与 Catalog/registry 条目，S7 格：两种拼写、oracle 成员核对、计费。
- **(a) 实跑比较臂**：新 Catalog profile：行数计数器、查询数计数器、去重行集（distinct-row-set）计数器（大 exposure 预算 + 小 max_rows/max_queries），预算由 §5.2 配方**先验**给出（写进语料前）；轨迹：现有 100 步自适应轨迹 + 至少两种顺序（打乱、新颖优先）+ 代理生成轨迹（LLM 助手在 Product 表上写的查询序列，与 SSB/agentworkload 同法）；adapter 新 mode 与验证器；报告每臂释放的 distinct facts 与拒绝点。
- **(c1) ProvSQL 发表级**：ProvSQL 工作负载扩到 S1–S6 形状（投影/分页/两源 join/union/分组），每格叶级展开 + oracle 逐成员核对；进 v1.12 契约。
- **(c2) 时延通道**：adapter 逐步记录客户端时延；新工作负载「被拒足迹阶梯」（10^5 行 Product 上，按足迹递增的被拒聚合）→ 报告被拒 vs 通过时延随足迹的曲线；论文 Prop 8 与 §8.2 按测得改写。
- **(c3) 误拒率**：良性负载语料（多套预算按配方校准）→ 每套预算的误拒数与各维消耗。

## 阶段
1. 代码与语义（本地单测 + WSL 包测试 + pilot 单剖面验证）。
2. 契约 v1.12：registry/coverage/activation、pilot 逐剖面通过。
3. **发表级 campaign 发射前需作者亲手签署 dataset binding / publication approvals**（P45 类，Claude 不代签）；机时约一整天。
4. 生成器与论文更新（改主张适配证据）、独立审稿。

## 状态
- 2026-08-30 22:40 CST：阶段 1 开始，从 (b) 与 (d1) 剖析入手。
