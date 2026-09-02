# P9.C 优化攻击者研究 — 先验设计冻结（2026-09-02，实现前冻结）

回应 round-10 M1 / round-11 反复强调的"机制从未面对真正试图在预算内最大化提取/重构的对手"。
本设计**在写任何模拟或语料前冻结**（AGENTS.md rule 8：停止规则与阈值用保守先验推导，不用实测拟合；
rule 9：因果断言需对照）。预算取 **owner-derivation（§5.2）**，绝不用被评测轨迹的 union 反推——这正是评审对 RLS 迹的核心批评。

## 目标产品与隐藏量（沿用玩具产品，不改 catalog）

`final_v5_rls_unlimited_expense_detail`，10 行冻结 fixture（见 finalv5rls/corpus-v1.json）。隐藏标量目标 =
销售部最大报销额 = 1910（TR-2026-0009）。对手只能发合法查询（threshold 探针 `amount > c`、页取、聚合），
只观测 accept/refuse 与每次结算返回的 `|Δ_X|` 电荷，不能读被扣下的行。

## Owner-derivation 预算（先验，非轨迹拟合；2026-09-02 修正）

**修正（实现前，记台账）**：初稿的 B_R/B_D 算式用了"行事实+每行全字段"的记账模型，与真实 oracle 事实模型不符——
finalv5rls 的独立 oracle 只计**证据格**（行柄不是独立依赖事实；列表步依赖 = dept+receipt_no 格/行，
equivalent 步再加 amount 格；count 步 = dept+amount 格/行；释放 = 被选 receipt_no 格或每 distinct 阈值一个聚合事实）。
按真实模型重推（owner 授"审阅销售部报销"，与对手行为无关）：
- 期望用途：分页列示销售 6 行（LIMIT 2 × 3 页）+ 两三个阈值核对聚合。
- **B_R = 10**（6 个 receipt_no 释放格 + ~4 个聚合释放事实）；
- **B_D = 18**（授权范围的全部证据格宇宙：6 行 × dept/receipt_no/amount 3 格）；
- **B_O = 12**（3 页复合 + ~3 谓词原子 + ~4 复合，上取整）；**B_Q = 2×B_O = 24**。
- 三档：owner (10/18/12/24)、tightened (10/12/8/16)、loosened (24/18/24/48)。
- **对手的探针数不参与派生；语料校验器钉全表。**

## 两个先验对手策略（各自的期望表在模拟后冻结）

1. **阈值二分恢复**（修正后）：对隐藏销售最大额（fixture 真值 1910）做 `count(amount>=c)` 二分，
   值域 [0,2048)（先验上界）。完整恢复需 11 次探针；每次 distinct 探针charged 2 个 novel outcome
   （1 原子+1 复合）+1 个 novel release（distinct 阈值聚合事实）。**恢复比特 ≤ ⌊B_O/2⌋**：
   owner 档（B_O=12）第 7 探被拒，恢复 6 比特（区间宽 32）；tightened（B_O=8）4 比特；
   loosened（B_O=24）11 探完成、精确恢复 1910。关键结构性发现（先验）：Δ_O=2 与数据无关，
   故 B_O 耗尽后的拒绝**不再泄漏数据比特**——探针梯的数据泄漏被 ⌊B_O/2⌋ 干净上界，
   这把论文 §3 披露的 O(B_Q+B_O·log) 界收紧为可实测的每档常数。
2. **distinct 事实贪心最大化**（修正后）：贪心选每步新增依赖格最多的合法查询。
   owner 档（B_D=18=授权宇宙）：一条 amount>=0 全列示即达 18（=宇宙，B_D 授满范围时如实放行）；
   tightened（B_D=12）：贪心按降序阈值逐行吸收（3 格/行），第 5 行被拒，distinct 依赖恰止于 12=B_D——
   **B_D 精确上界 distinct 事实而与查询序无关**；loosened 同 owner。

两策略各在三个预算档跑（档定义见上：owner 10/18/12/24、tightened 10/12/8/16、loosened 24/18/24/48），
显示提取量随预算单调、且始终被 ⌊B_O/2⌋ 比特与 B_D 个 distinct 事实上界，不被轨迹拟合。

## 头条断言（待实测复核后写入论文，先验期望）

- 预算确实**上界**提取：owner-derived 档对手恢复 ≤ B_O 比特的阈值信息 + ≤ B_Q 拒绝比特，与 §3 披露的界一致；
- 收紧 B_O 按比例削减恢复比特（单调，非拟合）；
- distinct 依赖恢复被 B_D 上界，对手拿不到预算外的行。

## 实现套路（沿用 finalv5counter，发射端+校验端同建 rule 3）

冻结语料包 `evaluation/finalv5adversary/`（corpus.go + corpus-v1.json + corpus_test.go，先验期望表钉死）→
校验器 `evaluation/internal/experiment/adversary_validation.go`（钉整张先验恢复表：每策略每档的 accept/refuse/
恢复比特/终态 distinct）→ 适配器 runner → 五发射面 → workloads 声明 → профиль 生成 → 定点 → 注册。
作为 P9.A 平行扩展发表协议的一员发表级发射（无 binding）。

**先验冻结于此。实现只允许让真系统复现此表；若实测与先验表不符，改的是主张与设计记录，不是证据（改口给根因入台账）。**
