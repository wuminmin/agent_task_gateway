# P9 新证据程序（作者 2026-09-02 "agree all" + "choose strong evidence choice"）

作者对六项决策清单全批，二选一项一律取强证据分支。本文件排序整个程序，核心约束是**把作者亲手签署压到一次**。

## 硬约束（不可违反）

- **封存的 172 格 campaign 永不修改**（`evaluation/final-v5-wsl2/raw/` 与其证据）。因此所有升发表级/新实验一律作为**独立的新发表级 campaign**，不折进 172 分母；论文以"扩展实验的追加发表级 campaign"引用，172 头条数字不变。这同时化解了 round-10/11 记的"分母耦合"。
- **binding 签署永不下放**（代签即伪造）。当前 binding 钉旧 catalog（ac2dc5cf @ 02618c1），现 catalog 为 5cd0ad44；语义增量仅为足迹梯 2 产品+路由+2 预算档（avg 改动已回退，bfb6a5b）。
- **pilot 类不需 binding**（实证：pilot-counter-05 在 binding 失效下 EXIT=0）。发表级的 Scale/Artifact/ProvSQL 格才触发 binding（待 Explore 确认 counter/footprint/benign 发表级是否也触发）。

## 批量签署策略

会改 catalog 的实验：ProvSQL 重启用（binding 失效因足迹产品加入）、片段扩展（派生格算术产品）、≥1e7 规模（大产品）、DProvDB 对比（可能新产品）。**全部先加进 catalog 并落到 pilot 定点，再让作者一次性签最终 catalog**，避免二次签署。不改 catalog、不需 binding 的项先做。

## 阶段

- **P9.0 机制确认**（Explore 进行中）：发表级计划能否只选 counter 四档；协议哈希绑定；注册表结构；发表级 binding 触发面。
- **P9.1 counter/footprint/benign 升发表级**（若不需 binding）：独立发表级 campaign，3 部署，注册到扩展 registry。直接把 round-11 表4 从 pilot 转发表级（解 R11-M6 强分支）。
- **P9.2 优化攻击者研究**（pilot，无 binding，无 catalog 改动）：同 expense_detail 玩具产品，冻结先验语料（owner-derivation 预算，非 trace 拟合），量化每 B_Q 单位恢复比特数 / 拒绝前可达 distinct 事实——量化论文已披露的信道。套用 counter 研究的语料+校验器+五发射面+定点套路。
- **P9.3 片段扩展**（强分支）：派生格/typed-arithmetic 依赖规则 + 守恒证明 + 重跑 TPC-H/SSB/agent 可接纳率。发射端与校验端同改（rule 3）。加派生格产品进 catalog。
- **P9.4 规模点**（强分支）：≥1e7 facts 单产品或批量结算实验，加大产品进 catalog；核 WSL 盘预算。
- **P9.5 对比实验**（强分支）：DProvDB 头对头（多分析师累计预算工作负载）或 BDG+DFC 组合演示。
- **P9.6 catalog 冻结 + 一次签署**：P9.3/P9.4/P9.5 的 catalog 改动全部落定后，生成最终 catalog，作者亲手重签 binding。
- **P9.7 发表级发射**：ProvSQL/规模/片段发表级 campaign（binding 就绪后），注册，整合进论文，重开评审轮。
- **P9.8 行政项**：artifact DOI、自引匿名化核查、avg 批准（作者）、以及 P9.6 的 binding 重签。

## 顺序理由

P9.1/P9.2 无 binding 依赖，先并行推进出证据；P9.3–P9.5 的工程量大且都改 catalog，做完再一次签署（P9.6）解锁其发表级；最后整合重评审。作者只需在 P9.6 签一次、P9.8 批 avg/DOI/匿名化。

## 关键架构发现（Explore 2026-09-02，改变排序）

**封存协议内无法做真发表级扩展实验。** `evaluation/final-v5-wsl2/protocol/{protocol-v1.yaml,workloads-v1.yaml}` 哈希锁定
（protocol_sha256 a7cd2cd4…、workload_manifest_sha256 b698b036…，钉在 contracts/index-v1.json 与 finalv5contracts/verifier.go:33），
封存的 formal-v111-publication-03 的每条配置都记录了这些哈希。给协议加 counter/footprint/benign 会改这两个哈希，破坏封存证据。
`final-v5-campaign-plan` 的 publication 分支硬断言 172 格（main.go:83），`ValidateSplitPublicationPlan` 硬断言 11 部署/125 格。
**评审 R11-M6 以为"升发表级很便宜（语料已冻结）"是错的——代价按实际量级说：这是一条平行协议的工程量，不是改几行。**

**解耦（好消息）：** 私有 Dataset Binding 只被 Scale/Artifact/ProvSQL 格触发（run-profile-campaign.sh:236-260），counter/footprint/benign
及新的优化攻击者都不需要它。因此**扩展实验的发表级证据无需作者签字**——只需一条平行的扩展发表协议。binding 重签仅为 ProvSQL-发表级与规模-发表级批量保留到最后一次。

## 修订阶段（两条轴解耦：证据类 vs 私有 binding）

- **P9.A 平行扩展发表协议**（无作者、无 binding）：新增 protocol-ext + workloads-ext + index-ext + campaign-plan 第三类分支
  + 平行 split 校验器 + 新注册清单 schema + paper 侧校验器。给扩展实验真发表级（3 部署×30 样本×warmup、publication_eligible=true、封存 manifest），
  绝不碰封存文件。发射端与校验端同建（rule 3）。
- **P9.B counter/footprint/benign 升发表级**（用 P9.A，无 binding）：解 R11-M6 强分支; 替换论文里模拟的 counter 臂为实测; 良性第二迹强化 R11-M2。
- **P9.C 优化攻击者**（扩展实验, 用 P9.A）：同 expense_detail 玩具产品, 冻结先验语料(owner-derivation 预算), 量化每 B_Q 恢复比特/拒绝前 distinct。
- **P9.D 片段扩展**（强分支, 改 catalog）: 派生格 typed-arithmetic 依赖规则+守恒证明+重跑可接纳率。
- **P9.E 规模点**（强分支, 改 catalog 大产品）: >=1e7 facts 或批量结算。
- **P9.F 对比**: DProvDB 头对头 或 BDG+DFC 组合。
- **P9.G 一次 binding 重签**（作者, D/E 落定后）→ 解锁 ProvSQL/规模发表级。
- **P9.H 发表级发射 + 整合 + 重评审; 行政项(DOI/匿名化/avg)。**

顺序: P9.A→B→C 无作者依赖先出扩展发表级证据; D/E/F 改 catalog 并行推进落 pilot 定点; 全部落定后 P9.G 一次签署; P9.H 收口。
