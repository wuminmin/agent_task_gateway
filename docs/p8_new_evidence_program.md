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

