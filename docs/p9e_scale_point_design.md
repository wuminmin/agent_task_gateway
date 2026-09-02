# P9.E 规模点设计（≥1e7 facts）——草案 v1（2026-09-02）

目标（作者 2026-09-02 强证据裁决）：回应评审"最大规模点仅 1.6M dependency facts"的质疑，
给出 ≥1e7 facts 的实测点。两条可交付分支，先做 A，A 撞资源墙则 B 保底：

- **A 单查询 1e7**：一条查询的 Dependency 足迹 ≥1e7 facts，novel 结算 + 语义重放两臂。
- **B 累计台账 1e7**（批量结算保底）：同 root 多查询把台账累计推过 1e7，测结算延迟随台账规模的曲线。

## 现状（实查出处）

- 最大数据面：exposure_scale 414k 行×4 列（benchmark-v1-generate.sql:32）；result_heavy 100k×16（同文件:46）。
- campaign 最大单查询依赖：S2/SF10 1,200,000（读代码推出自 evidence.tex 宏，待核）；S6/100k-x16 1.6M cells。
- settle 写路径：query_exposure_facts 按 5000/块 INSERT（internal/control/exposure.go:376,406），facts 为内存切片。
- gateway 侧代数：FactSet 全内存（internal/exposure/fact.go）。1e7 facts ≈ 数 GB 堆（读代码推出，未实测）。

## 方案 A：新产品 final_v5_scale_e7

- 数据：generate_series 1..625,000 × 16 列（=1e7 cell facts + 625k row facts），公式镜像 result_heavy
  （闭式可离线重算）。表体积 ~数百 MB（未实测）。
- catalog：新 source publication + 产品 + snapshot + ordinal sidecar。**触发 Dataset Binding 重签（Scale 类）**
  ——落 pilot 定点后进 P9.G 一次签署批（P9-SIGN-HOLD）。
- profile：scale-e7 档，单产品 closure；预算保守先验：D = 11,000,000（1e7 cells + 625k rows + 余量），
  R 按查询形态定，均在设计冻结时给推导式，不拟合实测。
- 查询阶梯：WHERE row_id <= N 全列，N ∈ {62,500(1e6), 156,250(2.5e6), 312,500(5e6), 625,000(1e7)}；
  外加一条 SUM 聚合（输出 1 行、依赖 1e7）。novel + 语义重放两臂。
- 严谨度：pilot 严谨档（3 部署 × 30 样本 × 5 warmup），campaign_class=pilot（不挪用 publication_eligible）。

## 风险与预实测门（先 1e6 阶梯点探路）

1. gateway 堆内存：1e7 FactSet 的常驻内存未实测；1e6 阶梯点先量 RSS 外推。
2. settle 事务时长：2000 块 INSERT 单事务；1e6 点实测事务耗时后线性外推，>10min 则转 B。
3. sidecar 物化体积与部署时长：625k 行 ordinal sidecar 未实测；部署一次量 profile-artifacts 体积（df 前后）。
4. WSL 磁盘：现 69G 空闲；预算 ≤15G（数据+工件+raw），超限先修剪已注册 pilot 工件副本。

## 方案 B：批量结算（保底，不需要新大产品）

现有 result_heavy/exposure_scale 上同 root 连发不重叠分页/分区查询，台账累计 ≥1e7；
报告结算延迟 vs 台账规模曲线（radix/去重成本随台账增长）。catalog 不改——若 A 因资源墙放弃，
B 仍可先行（不等 P9.G）。

## 执行顺序

1. （本设计冻结后）落 SQL + catalog 草案 + closed-form 模型 + 校验器（发射端与校验端同建）。
2. 1e6 预实测门（单部署 pilot）→ 三风险数字落台账 → 裁决 A 续/转 B。
3. A：profile 定点 → pilot-scale-01（阶梯全格）→ 注册 → 宏 → 论文 §Evaluation 规模段重写。
4. catalog 改动汇入 P9-SIGN-HOLD，等 P9.G 一次签署升发表级。
