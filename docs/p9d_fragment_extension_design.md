# P9.D 片段扩展 — 派生格/typed-arithmetic 依赖规则 先验设计（2026-09-02）

回应 round-11 M1（强分支）：实现补充表 XXXIV 勾勒的 **arithmetic in projections/aggregates** 规则
（最常见阻断，如 revenue = price × discount），带守恒论证，再重跑 TPC-H/SSB/agent 可接纳率。
先验设计先于代码（rule 8）；发射端与校验端同改（rule 3）。

## 规则：派生格（derived Release cell）

一个确定性 typed 算术表达式 `x = f(c_1,…,c_k)`（+,-,*,/、整/数值/时间域、无 volatile）在射影或聚合参数位产生一个
**派生 Release 格**，其身份复用现有派生事实机制 f_d：
- FactID = f_d(⟨B, k_o, N_arith(x), τ_x, C_τ(v_x), W⟩)，其中 N_arith 是算术表达式的规范正规形（交换律排序、类型化字面量、
  别名擦除——复用 §3 的 N(x) 框架，仅新增算术算子节点）；
- **Dependency footprint** = 表达式读的全部 base 列格 ∪ 行事实：`facts_D(x) = {row fact} ∪ {cell(c_i) : c_i ∈ args(f)}`。
  即"派生格的依赖=其算术表达式的自变量格"，与现有"谓词读的列即依赖"同源。

## 守恒论证（复用现有归纳，仅加算术节点）

- **确定性**：typed 算术正规形对语义等价输入注入（同 Theorem「Fact identity well-definedness」，扩到算术表达式：
  排除 float NaN 位型，按 PostgreSQL 语义值等价）。⇒ 同表达式同行产生同派生身份。
- **拆分/重放守恒**：把 `x=price*qty` 拆成两查询或重放，派生身份不变（正规形确定）⇒ 累计只收一次
  （现有 base-ledger split/page 守恒命题的算术实例）。
- **足迹正确**：facts_D(x) 恰为归纳规则选出的自变量格，不含被拒行/非输出（O2 的算术实例）。
- 除法/溢出/非结合浮点：`SUM(a*b)` 的浮点折叠已被拒（bag 不定折叠序）；整/数值算术确定，时间算术按类型规则。
  非确定算术表达式一律 fail-closed（保守，不进片段）。

## 发射端 + 校验端同改（rule 3）

- 发射端: `internal/gateway` 的 lowerer（接受算术射影/聚合参数节点，现 PROJECTION_EXPRESSION_UNSUPPORTED/
  AGGREGATE_UNSUPPORTED 拒）+ 效果抽取（派生格 facts_R/facts_D）+ 正规形 N_arith。
- 校验端: RQ1/oracle 独立实现镜像同规则; generated-plan 差分评估器加算术节点; 守恒/拆分测试覆盖算术格。
  **两端同改否则最贵一轮才炸。**

## 重跑可接纳率（证据）

规则落地后重跑三套 lowerability：TPC-H（现 0/22）、SSB（现 2/13）、agent-written（现 27/40）。
先验期望（保守，不拟合）：算术规则解锁 agent 集里的 projection-arithmetic 阻断（Q09/Q20/Q32，3 条）与部分聚合算术，
SSB 的 derived measure（lo_revenue-lo_supplycost）解锁若干；TPC-H 仍多数卡在子查询/其他。
**实测数字待复核后写论文；先验只记方向不记具体分子。**

## 主张边界（诚实）

只实现算术派生格这一条规则；OR/外连接/子查询/非字面比较仍 fail-closed（补充表 XXXIV 其余行留作未来）。
论文相应从"覆盖有限"更新为"算术派生测度已纳入片段，覆盖提升到 X"（X 实测填）。catalog 改动：可能需派生测度产品做评测，
并入 P9.G 一次签署前的 catalog 批次。

## 顺序

先在 tkde-review-revisions 落代码+两端测试（门禁绿），再作为扩展实验/或直接在现有 lowerability 语料上重跑（离线，无需 binding），
可接纳率数字进论文。catalog 若需新派生测度产品，并入 P9.G 批次。相关: [[docs/p9_new_evidence_program.md]]。
