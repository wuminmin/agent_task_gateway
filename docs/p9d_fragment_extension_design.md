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

## 施工分片（2026-09-02 细化，承 Explore 地图；每片全绿提交）

- **D1 queryplan 层**: QueryPlan 加派生射影槽(DerivedColumn{Expr *DerivedExpr, Alias})、Aggregate 加派生参数槽; 法向式版本策略——含算术的查询用新常量 taskgate-query-normal-form-v5, 无算术查询字节不变(V3/V4 哈希与重放身份零扰动); Compile 的 SQL 发射; 单测。
- **D2 lower.go 接线**: lowerSelect 第四分支(A_Expr {+,-,*,/} over ColumnRef/A_Const/嵌套→DerivedExpr, lower.go:176 前), lowerAggregate 接受派生单参(:476 前); float/未知算子/深度 fail-closed 原码不变; 单测含全部拒绝路径。
- **D3 exposure+gateway**: algebra_v2 加 MapV2(镜像 :749-758 聚合格循环, per-row: Value=PG 返回值, Expression=N_arith, ReleaseFact=nil, Support/Witness=参数格并集); deriveObservationV2 + deriveRelationalObservationV2 接线; physicalquery/preparation 视需传派生列; 单测。
- **D4 三镜像**: generatedalgebra reference.go 派生分支 + campaign.go 生成 draw + production.go MapV2 映射; make eval-generated-algebra 全跑 0 mismatch。
- **D5 门禁**: DB suite + go test ./evaluation/... + vet。
- **D6 可接纳率重跑**: make eval-tpch/ssb/agent-workload-lowerability; finalv5benign build.go 冻结报告漂移检查与 corpus_test 27 钉连锁更新(若 agent 集接纳数变)——benign 语料重冻需同轮记台账; 宏(\TPCH*/\SSB*/\AgentWorkload*)经链重跑。
- **D7 论文**: §9/§1/摘要覆盖率措辞按新实测更新(对价裁减守 12 页)。

版本策略依据: NormalFormVersion 注释"Existing V4 tasks must never have their normal-form or outcome hashes reinterpreted"——新算子节点只能进新版本常量, 且只对含算术的查询启用。

## D3 设计更正（2026-09-02，实施前）：admitted 算子收窄到 {+,-,*}

除法退出第一版规则：PostgreSQL numeric 除法的结果 scale 选择语义复杂且商可为非终止小数，
离线精确复刻（effect 推导需在 companion 侧确定性重算派生值以交叉验证 PG 可见值）不可行；
而 +,-,* 在精确域封闭（int64 带溢出检查 fail-closed；numeric 用 big.Rat 精确、结果必有限）。
评测目标（agent 集 Q09/Q20/Q32、SSB derived measure）全部只用 * 与 -，除法不损失任何目标覆盖。
除法保持 fail-closed 原拒绝码，表 XXXIV 注记更新。派生值记账采用**重算路线**：
MapV2 由参数格值确定性重算派生值（回避 PG 行对齐），可见结果哈希继续由现有结果校验面覆盖。
聚合派生参走"MapV2 先行 + 聚合引用派生字段"的组合，不给聚合层加第二条派生路径。

## D4 前语义修正（2026-09-02）：派生算术 NULL 传播

初版 MapV2 把 NULL 参数 fail-closed——错：PostgreSQL 对 (a*b) 的 NULL 参数返回 NULL，
含 NULL 行的合法查询会被错误拒绝（差分域即有 three-valued 行）。修正为 NULL 传播：
任一参数 NULL ⇒ 派生值 NULL（规范 "null"），依赖仍并集参数格（读了才知道是 NULL），
派生 Release fact 值=null 与 base NULL cell 先例一致；交叉验证两侧 canonical "null" 相等。

## Ordinal(V4/V5) 路径接线蓝图（2026-09-02 精读产出）

通道已存在：ordinal_derivation.go 非分组 visible 输出 cell 无 base fact 时走
NewDerivedFactV2(bundle, member.key, spec.CanonicalExpression, SQLType, 可见值, witness 承诺) 进 dynamic derived 集
（commitUngrouped）；聚合输出即此先例。ordinal 语义派生值=PG 可见值（与 V2 路径重算不同——V2 侧的重算交叉验证
保留为独立防线；ordinal 侧可后续附加重算校验为增强）。接线三件：
1. queryplan.OrdinalVisibleSpec 加派生参数(Args []OrdinalFieldUse)+singleVisibleSpecs 的 Kind:"derived" 分支
   （插 Columns 后 Aggregates 前, CanonicalExpression=N_arith, Args=参数列 evidence bindings 的 fieldUse）。
2. ordinalAggregateSpecs 的 DerivedArg 分支（Args 同上, CanonicalExpression=fn(N_arith), SQLType=AggregateOutputType(fn, exprType)）。
3. gateway ordinal deriver 的 visible cell 组装处按 Args 并集参数格 witness（找 member.cells 填充点）；
   normalizeOrdinalProgram 校验面同步。

## D6 域放宽（2026-09-02，实测驱动）：精确域内类型提升

同型闭合把 q32（quantity bigint × unit_price numeric）拒了，而 PG 的 int×numeric→numeric 是精确运算。
放宽为：全部操作数属精确域 {smallint,integer,bigint,numeric}；输出型 = numeric 若任一操作数为 numeric，
否则 bigint（整数提升；重算侧 int64 溢出仍 fail-closed）。字面量继承输出型。q09/q20 仍拒（子查询、
聚合间除法——超范围属正当界定）。多源（join 上的）派生算术留作后续版本，本版单产品。
