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
- 2026-08-31 01:10 CST：(b) 代码、差分、RQ1、论文文本全部落地（台账 P8-B-FRAGMENT / P8-B-DIFFERENTIAL / P8-B-RQ1 / P8-B-PAPER）。
  (d1) 四刀落地：100k×16 扫描派生 15.67 s → 2.43 s（台账 P8-D1-DERIVATION），门禁全绿（P8-D1-GUARDS）。
  阻塞发现：私有 Dataset Binding 钉住 `config/catalog.yaml` 摘要，(b) 的 avg 批准使其失效——已恢复文件字节，avg 留在校验器能力
  （P8-D1-BINDING）；**阶段 3 前作者需重新签署绑定，届时决定演示 Catalog 是否批准 avg**。
  试点复测进行中：subphase-pilot-09（analytics 两剖面 ×1）、随后 subphase-pilot-08（result-heavy ×3，含 S6 100k×16）。
  尚未开始：(a) 计数器比较臂、(c1) ProvSQL S1–S6 发表级、(c2) 被拒探针时延、(c3) 良性负载误拒率、(d2) ≥3 源连接格。


## (c2)/(c3) 详细设计（2026-08-31，实现前先记下，预算先验写在语料之前）

### (c2) 被拒查询的时延通道
- **证据字段**：`RLSStepEvidence` 与 `AttackStepEvidence` 各加 `client_ms`（adapter 从发出请求到收到响应的墙钟毫秒；已有 `component_ms` 只在网关侧、只对通过的查询）。发射端（`evaluation/cmd/final-v5-adapter/rls.go`、`attack.go`）与校验端（`evaluation/internal/experiment/sample-v3.schema.json`、`rls_validation.go`）同一提交改。
- **新工作负载** `refused-footprint-ladder-v1`（实验 rls，scale `1e5-rows`，modes `bounded` / `unlimited`）：Product `final_v5_rls_{bounded,unlimited}_result_heavy`（10^5 行关系 `final_v5_result_heavy` 的两份不可变 Product，各自 profile：`rls-footprint-bounded` / `rls-footprint-unlimited`，registry/hot-artifacts/attestation/activation-support 全套条目）。阶梯：行数 k ∈ {10^2, 10^3, 10^4, 10^5} × 参数列数 f ∈ {1, 4, 16}，每步 `SELECT sum(c1)[, …, sum(cf)] FROM final_v5_result_heavy WHERE row_id <= k`，依赖足迹 = k·f。每步新根（fresh root），使拒绝只来自单查询计费超过 B_D，不来自累计。
- **先验预算**（写在语料前）：bounded 臂 B_D = 100（= 最小阶 10^2×1 的足迹，只有该阶通过，其余 11 阶被拒）；B_R、B_O、B_Q 按 §5.2 配方给足（B_R = 0 行明细——阶梯只放聚合；B_O = 阶数 + 谓词原子数 = 12+4；B_Q = 4·B_O）。unlimited 臂全部通过，给出同足迹的「通过」时延对照。
- **报告**：每阶被拒（bounded）与通过（unlimited）的 `client_ms` 中位数与 IQR 对足迹曲线；通过侧另给 `component_ms` 分相。论文：§3 「timing channel」句与 Prop. 8 后的讨论改为测得斜率（µs/依赖事实）与截距；§限制段保留「模型不含该通道」，但给出测得量级；补充材料一张表。
- **边界**：该通道由「执行与派生先于结算」决定，(d1) 的派生提速会直接改变斜率，因此 (c2) 必须在 (d1) 之后、与 Phase 3 同一发表级 campaign 内测。

### (c3) 良性负载误拒率
- **语料**：`evaluation/agentworkload` 的 40 条 LLM 所写 SQL 中可降低的 27 条（摘要钉住），按问题顺序作为一条良性轨迹在 expense/orders Product 上执行；每样本新根。
- **预算集（先验，§5.2 配方）**：`recipe`：B_R = 语料里明细类语句合计释放行数 × 列数（由语料静态算出，不跑网关），B_D = 语料中最大聚合足迹（同样静态算出），B_O = 语句数 + 不同谓词原子数，B_Q = 4·B_O；`recipe-x2`、`recipe-x4` 各维乘 2、乘 4。三套预算 = 三个 profile（`benign-recipe` / `benign-x2` / `benign-x4`），同一 Catalog。
- **报告**：每套预算下被拒的良性语句数（误拒）、首个误拒的位置、各维累计消耗/预算；`recipe` 预算按定义应无误拒——若有，则是配方本身漏算（如分页/重复查询的 Outcome 计费），如实报告并回改 §5.2 的配方文字，而不是调预算。
- **边界**：不是「安全性」证据，只回答审稿人「配方给出的预算在良性使用下是否够用」。
- 2026-08-31 02:00 CST：试点复测完成（S6 5.0×，S1 2.8×，S2 不变→第五刀分组路径 1.6× 于剖析子测试）；(d1) 到此收口，剩余 = DB 套件复核、v5 链 #7、证据提交、终检、analytics 试点复核 S2。
- 2026-08-31 04:50 CST：(c2) 第一片(client_ms 证据字段+版本门控)落地全绿(台账 P8-C2-CLIENTMS)。
  勘定：私有 Dataset Binding 只在选中 Scale/Artifact/ProvSQL 格时校验（run-profile-campaign.sh:255），
  ladder/良性/计数器等 RLS 族试点**不需要**重签；live catalog 的新增 Product 会使绑定对 S/A/P 格失效，
  但那次失效已注定由 Phase 3 前的一次作者重签统一覆盖（届时一并批准 avg 与全部新 Product/预算 profile）。
  阶梯实现的冻结面：RLS 校验器的「frozen primary matrix」需按 v1.12 扩展（新 workload id + 预注册步数 + 语料包）；
  新 Product 属作者已于 2026-08-30 批准的新证据程序范围，逐项记台账。

## 契约 v1.12 变更面清单（2026-08-31 勘定；每个新工作负载/Product 都要走全）
1. **语料包**：`evaluation/finalv5<name>`（仿 finalv5rls：embed 冻结 JSON、CorpusID/CorpusSHA256、Steps/预期计费、Load 校验）。
2. **Product/目录**：master `evaluation/final-v5-wsl2/catalog/benchmark-contract-v1.yaml`（被 `contracts/index-v1.json` hash-locked→重钉+
   contract_release 升 v1.12，`supersedes_contract_release` 链上）+ live `config/catalog.yaml`（新 Product/预算 profile；会再次使 Dataset
   Binding 失效——Phase 3 前作者一次重签统一覆盖）。
3. **profile 生成**：`config/profiles/workloads-v1.yaml`（声明 experiment→workload→products）→ `go run ./evaluation/cmd/final-v5-profile`
   重生成 profile catalog/registry.json/hot-artifacts/coverage/attestations（registry `contract_release` 同步 v1.12）。
4. **冻结协议**：`evaluation/final-v5-wsl2/protocol/workloads-v1.yaml`（发表格矩阵）+ `protocol-v1.yaml` replicate contracts；
   两文件 sha 重钉进全部 example 配置（protocol/workload/acceptance/statistics 四摘要）。
5. **adapter**：`evaluation/cmd/final-v5-adapter/`（新 workload 分支 + capability.go 的 publicationRequirements 矩阵）。
6. **校验器**：`evaluation/internal/experiment/`（frozen primary matrix 扩展 + 预注册步数/预期计费 + schema 若有新字段则 v1/v3 同步）。
7. **发链前**：`go vet -tags taskgate_integration ./...`；DB 套件；生成器 13 单测；再 v5 链。
- 2026-08-31 05:20 CST：契约 v1.12 闭环——阶梯 Product 对+预算 profile 落地（P8-C2-V112），激活固定点达成（P8-C2-FIXEDPOINT，
  live pE-v112-fixedpoint-10 全绿 + 离线固定点提交 c126487），全量 evaluation 套件与 vet -tags 全绿。十次发射的失败谱系
  （分支落后/旧版钉死/磁盘满）与流程补丁（预检内置 pull、df 纪律）见台账。
  下一步：DB 套件+链 #8 复核 v1.12 树，然后 (c2) 阶梯的 adapter/校验器休眠切片（slice b），最后 workload 声明激活（slice c）。
- slice b 设计定稿（2026-08-31）：阶梯沿用 experiment `rls`（新 experiment id 会牵动 launcher 映射/example 配置/finalize 注册表，面更大）。
  adapter：`validRLSCell` 增列 `refused-footprint-ladder-v1`/`1e5-rows`/{bounded,unlimited}，Execute 分派到新的 ladder runner
  （fresh root per rung；unlimited 臂 12 阶全过并核对精确标量；bounded 臂仅 rung-01 过，其余核对 EXPOSURE_BUDGET_EXHAUSTED 与
  charged_dependency==语料基数；每步 client_ms）。验证器：`validateRLSVerificationStrict` 在语料绑定检查**之前**按 WorkloadID 分支——
  ladder 样本绑定 finalv5footprint（CorpusID/sha、每阶期望），不撞 finalv5rls 的 CorpusSHA256 钉。capability 矩阵与 frozen 协议
  workloads 在 slice c（激活）时一并动，capability_test 的 cellCount 6→8 同步。
- 2026-08-31 08:00 CST：链 #8c 全绿, v1.12 树四道门禁(DB 套件/全量 evaluation/链/终检)收口(台账 P8-C2-CHAIN8)。
  链 #8 的三次尝试暴露并修复两类流程缺陷：E1 装置无指纹 reuse 硬钉、冻结引导撤销未成对恢复(P8-C2-CHAIN8A/8B)。
  当前进行：slice b(阶梯 adapter runner + 校验器语料分支, 休眠)。
- slice b 设计修订（2026-08-31 08:20，改口给根因：此前判「沿用 rls」时未见 sample-v3 的 experiment 条件块与语料钉死的全貌）：
  阶梯改为**独立 experiment id `footprint`**——RLS 信封的必填字段是策略形状（policies/roles JSON），复用等于伪造策略证据；
  schema 按 experiment 绑定信封，塞 workload 条件比新开 experiment 手术面更大；且契约明文允许 required 九项之外的
  source-controlled 扩展 experiment（types.go:131 default 分支），试点可跑，发表级纳入属作者范围。
  新面：FootprintVerificationEvidence + sample-v3 条件块 + finalv5footprint 语料绑定 + adapter footprint.go + validator + 
  launcher 映射/example 配置/workloads 声明（slice c）。

## 阶梯有界臂拒绝码修订（2026-09-01，pilot-footprint-04 实证）

有界臂的拒绝有两个站点，码按阶先验可导出（`Rung.BoundedRefusalCode()`）：

- 证据行跨度（阶的基行数）≤ Dependency 上限 400：预留期估算越界，拒 `EXPOSURE_BUDGET_EXHAUSTED`（第 2、3 阶，100 行）。
- 跨度 > 400：完整来源证据在该预算下不可能产出，fail-closed 拒 `EXPOSURE_EVIDENCE_REQUIRED`，零计费（第 4–12 阶，≥1000 行；pilot-footprint-04 第 4 阶实录 0/0/0 计费）。

这是既有生产语义（三个产生站点读码核对），不改任何被测路径；改的是实验主张——两码都是暴露契约下的未计费拒绝，
按站点区分并逐阶钉死。第 1 阶按语料精确计费获释放（1 release / 400 dependency / 6 outcome，标量 799819/2）。

## (c3) 设计修订（2026-09-01，动工前；数据事实核查后）

**语料现实**（实查 queries/ 与 config/catalog.yaml、db/init）：27 条可降低语句跨 5 产品 3 数据族，全部闭式可离线建模——
expense 族冻结 10 行（00-schema.sql）、provsql 族 orders 50k/lineitem 250k/nonce 1k（01-fixture 闭式公式）、
result_heavy 100k×16 列闭式（benchmark-v1-generate.sql）。

**先验分类必须分三层**（可降低 ≠ 会释放）：
1. **策略层拒绝**（预算无关，先验可判）：AVG 类（allowed_aggregates 无 avg——avg 批准属作者待办）、LIKE（q34，运算符 ~~ 不在产品面）、
   其余按产品 allowed_operators/aggregates/functions 与 SELECT * 的处理逐条静态判定（用生产 lowering+policy 离线跑，不手判）。
2. **授权但零释放**：谓词与数据不匹配（q03/q07/q37 英文部门值 vs 中文数据；q29 category=\'A\' vs alpha..delta；
   任务 mandatory scope 与谓词交集为空）。释放 0 行，但仍消耗 B_Q 与 Outcome 原子。
3. **授权且释放**：按数据模型静态算 release/dependency/outcome 足迹。

**误拒指标定义**：误拒 = 三层中第 2、3 类（策略层已授权）语句在给定预算下被暴露预算拒绝的数量。
第 1 类如实另列（\"policy-refused, budget-independent\"），不计入误拒也不从语料中删除（语料是未编辑的 agent 输出）。

**配方预算（先验公式，语料静态量导出）**：B_R = Σ(明细类释放行×列)，B_D = max(单语句依赖足迹)，
B_O = 语句数 + 不同谓词原子数，B_Q = 4·B_O；三套 profile：benign-recipe / benign-x2 / benign-x4，同一闭包
（5 产品全集，新 profile 走阶梯同款激活流程）。每样本一根跑完整 27 条轨迹（考累计，不同于阶梯的每阶新根）。

**实施切片**：A. 数据模型+离线分类器+足迹计算（finalv5benign 语料包，冻结 corpus-v1.json）；
B. 证据信封+校验器+schema+adapter（复用 footprint 模式，独立 experiment id \'benign\'）；
C. 声明+profile+激活+launcher 三处白名单+试点。

### (c3) 依赖口径定论（2026-09-01，读代码 internal/queryplan/relational.go）

companion（ProvenanceSQL）只应用 WHERE，不含 HAVING 与 LIMIT：依赖足迹 = WHERE 幸存基行 ×（1+evidence 字段数），
与可见结果是否为空无关。后果：q25（HAVING 全排除，输出 0 行）仍计 250k 行依赖；q30（LIMIT 100）仍计全部 ~5.7 万幸存行；
q28（WHERE 匹配 0 行）companion 0 行 → 依赖 0。evidence 字段集与 join 双源结构一律取自 CompileRelational 的
Sources[].EvidenceFields，不手推。配方 B_D 由此为 max 单语句足迹（预计量级 ~1.1M，q23 连接）。
分类链复用 harness_sql_test 的纯 Go 三链（QueryProductFromCatalog + Lower + Compile(Relational) + sqlpolicy.Authorize，
live catalog 的产品面），良性任务 scope 取全包含集（dept 全三值/partition_key=1/category 全四值），不改变语句语义。
语料 v1 钉：语句字节 sha、分类三层、释放行数与释放事实基数、依赖基数与集合摘要、谓词原子数；聚合标量不钉（RQ1 已覆盖正确性，
(c3) 只测拒绝），如实记录该范围决定。

### (c3) 语料 v2 规则集（2026-09-01，pilot-benign-01/04 实测反推并逐句验证）

**单产品（legacy）依赖**：每贡献行 = 基行事实 + 调用方引用列的格事实（投影∪过滤∪分组∪聚合参数；
含被引用的实体键列，q30/q22 证实；不含 mandatory scope 注入列，q21 证实）。贡献行 = 可见输出组/行的成员：
HAVING 排除组零贡献（q05 D=0）、LIMIT 截断（q30=100×17）——legacy companion 即带 LIMIT 的可见语句。
novel 验证链：q13=6、q38=10、q36=10 全命中。

**释放**：明细 = 行×(1+列)（q30 R=1600=100×16——注意实测无行事实?待覆核: 1600=100×16 纯格）＋跨句集合去重（q35 novel R=16）；
分组 = 输出行×可见列（q01 R=6=3×2, q08 R=24, q13 R=12=3×4）。

**Outcome**：接受句 = 唯一谓词原子数+1 复合（q22 O=2, q35 O=7=6+1, HAVING 句 O=2=1 原子(HAVING 字面量)+复合）。

**join 待标定**：以 CompileRelational 的 Sources[].EvidenceFields 为字段权威，联行发射规则在模拟中对齐两个实测靶：
q23 novel=845,000（前置 q21=100,000, q22=15,000）与 q26 novel=495,000。对齐后冻结。

**重分类**：q05/q12/q15 = released 类、释放 0 行、依赖 0、O=2。预算按 v2 绝对集重推后, 三臂 pilot 定版。

