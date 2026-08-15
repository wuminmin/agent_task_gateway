# 带库全量优化 + 9/9 规模实验 合并计划

制定日期 2026-08-16。作者要求把「带库全量只跑一次」的优化方案与「走向 9/9」的规模实验
方案合并成一份计划。本文只写计划与依据，不改任何证据字节，不翻任何 capability。

本文所有事实都标注了出处。凡是推断而非查到的，逐条标记为「推断」。

---

## 一、为什么两件事必须一起规划

计划 B 的每一步都要跑测试；计划 A 决定每一步跑多少。若不先改 A，B 的 118 个 cell
会把「每任务一次带库全量」乘上去，光回归测试就是压倒性的开销。反过来，A 单独做完
没有意义——省下的时间正是要投进 B。

---

## 二、现状（实查，附出处）

### 2.1 带库全量的成本

| 事实 | 出处 |
|---|---|
| `./scripts/db-test-env.sh test -count=1 ./...` 是「带库全量的唯一支持写法」 | `AGENTS.md:63` |
| 每个碰代码的任务，验收清单都含带库全量 | `docs/codex_publication_execution_plan.md:168`、`:470`、`:533` |
| `internal/gateway` 逐包耗时（台账记录，按时间顺序）：2975.095s、2949.087s、3286.496s、3099.580s、3931.505s、4016.427s、4002.790s、4002.790s | 台账各行 |
| 合计 28,263.77s ≈ 7.85 小时，**仅这一个包**（末两条数值相同，可能是同一次运行被两行引用，故实跑 ≥7 次） | 同上 |
| 该包占全量墙钟约 89%（4002.790s / 约 4500s） | v1.10 冻结行的 78 包耗时清单 |
| 该包耗时从 2975s 涨到 4025s，**增长约 35%** | 同上 |
| 慢的原因**不是** sleep：31 个测试文件、150 个 `Test` 函数中只有两处 `time.Sleep(10 * time.Millisecond)` | `internal/gateway/*_test.go` |
| 逐测试耗时**尚未测得** | 本文写作时正在后台跑 `-json` 采集 |

### 2.2 9/9 的真实缺口

先更正一个此前的印象：**契约不是空白，两个家族的 cell 已由作者批准设计并逐格写死**。

| 家族 | cell 数 | 契约状态 | 逐格实现状态 | 出处 |
|---|---|---|---|---|
| baseline | 58 | `AUTHOR_APPROVED_FOR_IMPLEMENTATION` | 58 格全部 `PENDING_IMPLEMENTATION` | `evaluation/final-v5-wsl2/contracts/baseline-v1.json` |
| scale / dependency-e2e | 24 | 同上 | 24 格全部 `PENDING_QUERY_IDENTITY_ADAPTER` | `evaluation/final-v5-wsl2/contracts/scale-v1.json` |
| scale / outcome-merkle | 36 | 同上 | 36 格全部 `PENDING_30_SAMPLE_SCHEDULE_REPLACEMENT` | 同上 |
| scale / extreme | 2 | 在 `scale_extreme_conflict_appendix`，不在 60 格正表内 | — | 同上 |
| artifact | 6 | 已实现 | **2026-08-16 六格全部通过 v3 acceptance** | 台账 `P8-artifact-observerfix` 行 |

**关键结构性事实：scale 家族 60 格只被两个具名阻塞项挡住**——24 格等一个 query identity
adapter，36 格等一次 30-sample schedule 替换。这不是 60 份独立工作。

每个 cell 的契约已经写死了：product/publication 绑定、query 模板与参数、direct 与 bdg
两条入口、`fresh_deployment`、`warmups: 5`、`samples: 30`、期望行列数、oracle manifest 路径、
negative controls、以及**允许与禁止的主张**。全部 118 格统一为 30 样本 + 5 预热。

每个 cell 尚缺的是同一组东西：

- `expected.*_sha256` 与三类 cardinality 均为 `null`，`digest_generation_status: NOT_GENERATED`
- `oracle.manifest_sha256` 为 `null`，`generation_status: NOT_GENERATED`
- `digest_review_status: NOT_APPROVED`、`exact_generated_bytes_freeze: NOT_APPROVED`
  ——**这两项要作者逐字节签署，红线 1，不可代签**

### 2.3 已确认可以缩减的范围

按「每一格必须对应论文里写出来的主张」筛选，44 格转 future work，`main.tex` 与 supplement
一字不改。依据见台账 `AUDIT-self` 行与本文附录。

| 组 | 砍 | 保 | 理由 |
|---|---|---|---|
| baseline S3 | 15 | 0 | S1 的 `SF1`/`SF10` 已提供同类证据（单产品 + direct 对照 + 三种重放收零） |
| scale outcome-merkle | 27 | 9 | 保 `10k/100k/1m` × `x1/x100/x10k` 固定 `o50`，回答摘要点名的 “incrementally updated”；重合度**正确性**由 dependency-e2e 24 格全覆盖 |
| scale extreme | 2 | 0 | 论文未承诺该量级，`main.tex:1398` 现有自限句保留即可 |
| **合计** | **44** | — | — |

**需做 76 格** = baseline 43（S1 10 + S2 10 + S4 3 + S5 8 + S6 12）+ scale 33（dependency-e2e 24 + outcome-merkle 9）。

---

## 三、计划 A：带库全量优化

### A1 — 规则改为三层（立即可做，零风险）

| 层 | 触发时机 | 跑什么 |
|---|---|---|
| 每次代码改动 | 每个任务 | 只跑**受影响的包** + 该改动的聚焦测试，仍用 `-count=1` |
| 门禁 | 契约冻结、翻 capability、正式 campaign、打 tag 之前 | **完整带库全量一次** |
| 明确不做 | — | 不靠 Go 测试缓存省时间（避免「缓存是否造假」争议），而是明确不跑未受影响的包 |

改动落点：`AGENTS.md` 的命令清单、`docs/codex_publication_execution_plan.md` 的任务模板。

风险与缓解：任务 N 的回归要到门禁才暴露且不知是谁引入——受影响包加聚焦测试可拦住绝大多数；
真漏到门禁，对几个提交做 bisect 远比每任务 75 分钟便宜。

### A2 — 测出 `internal/gateway` 的耗时构成（进行中）

一次 `./scripts/db-test-env.sh test -count=1 -json ./internal/gateway/...`，从 `-json`
的逐测试 `Elapsed` 排序，看是长尾还是少数测试吃掉大头。**成本约 67 分钟，只需一次。**

### A3 — 按 A2 的榜单动刀

拿到榜单前不预设结论。可能的方向（**推断，非查到**）：若每个测试都新建 schema 或重跑迁移，
可改为按包一次性 fixture 加事务回滚。

收益量级：把该包从 4000s 降到 1000s，等于**每一次**全量都省约 50 分钟，门禁那次也省，
并止住 35% 的增长趋势。这比减少跑的次数更值钱。

**A3 的验收**：改造后一次完整带库全量 exit 0，且 `internal/gateway` 的 PASS 集合与改造前
逐个测试名一致——只准变快，不准变少。

### A4 — 停止每轮复制两份完整产物（作者 2026-08-16 批准的清理暴露出来的）

`run-artifact-targeted.sh` 每轮把完整产物复制出两份：`snapshot-index-artifacts-full/` 约 4.5 GB
与 `profile-artifacts/` 约 1.9 GB，合计每轮 6.4 GB 落盘；其中 `final-v5-result-heavy-v1.cold.tgord`
（1,859,323,393 bytes）**在同一个运行目录里存了两份**。而每轮真正入库留证的只有 16 个小文件、
合计 845,008 bytes。

2026-08-16 按作者批准清理了 16 个被 ignore 的大产物子目录，回收 51 GB（`raw/` 69 GB → 19 GB）。
清理后复核：两个已入库运行目录各 16 个 tracked 文件全部在位，`git diff HEAD -- raw/` 为空，
v1.10 qualification 的两个活输入 SHA-256 与台账逐字节一致。

**待改**：让 launcher 按需保留，而不是每轮无条件复制两份。改完每轮少写约 6.4 GB。
注意 WSL2 的 `ext4.vhdx` 只涨不缩，`/`、`/var/lib/docker`、`/home` 同在一个 `/dev/sdd` 上，
所以「少写」比「事后删」有效得多。

---

## 四、计划 B：走向 9/9（76 格）

### B0 — 对齐当前事实（近乎零成本）

1. 翻 `artifactRealSystemValidated` 为 true。源码口径写在
   `evaluation/cmd/final-v5-adapter/capability.go:114-121`，2026-08-16 那轮已满足并留证。
2. 更新 `capability.go` 中已过期的路由注释（`7e705a9` 已把 Scale 路由与
   `final-v5-exposure-scale-v1` 预算档加进主 Catalog，注释未同步）。
3. 把 `docs/tkde_revision_status.md` 与源码口径改一致。

**产出**：7/9。**需作者批准**（capability 从 false 翻 true 属必须通知作者的三类之一）。

### B1 — 拆两个具名阻塞项（解锁 scale 全部 33 格）

- **B1a query identity adapter** → 解锁 dependency-e2e 24 格
- **B1b 30-sample schedule 替换** → 解锁 outcome-merkle 9 格（保留切片）

同时**实测确认路由闸门确已解除**：用一个 task 在同一 profile 上连做 novel + semantic_replay。
Catalog 现状显示 `max_queries: 8`、`max_influence_facts: 2500000` 足够，但**尚未实跑验证**。

B1 排在 B2 之前，因为两个阻塞项各挡住一整族，是杠杆最高的两处。

### B2 — baseline 43 格分批实现

按解锁论文哪句话排序：

| 批 | cell | 解锁 | 备注 |
|---|---|---|---|
| B2a | S1 10 + S2 10 | 治理开销（direct vs BDG）+ 三种重放收零 | 审稿人第一个问题 |
| B2b | S4 3 + S5 8 | 摘要点名的 nested View 与 bounded union | **S4 需先把 `final_v5_analytics_depth4` 接进 `config/catalog.yaml`**，目前只在 `config/profiles/registry.json` |
| B2c | S6 12 | result-heavy 路径 2026-08-16 已实测跑通，最省 | — |

### B3 — 生成期望字节与 oracle manifest

76 格的 `expected.*_sha256`、三类 cardinality、oracle manifest 全部从真实运行生成。
**此步产出的是待签署字节，不是已批准证据。**

### B4 — 作者逐字节签署

`digest_review_status` 与 `exact_generated_bytes_freeze` 由 `NOT_APPROVED` 转为批准。
**红线 1：approval 文件写的是作者姓名与决策编号，Claude 代签即伪造证据，此步永不下放。**

### B5 — 正式 campaign 与论文改写

跑 `run-deployment.sh` 九实验 campaign，然后改写论文三处散文：

- `main.tex:1363` “Publication-scale performance remains pending the final frozen WSL2 campaign.”
- `main.tex:1365-1366` “one index build and deployment rather than 140 independent deployments”
- `main.tex:1397-1399` “not million-scale latency…”

**无论最终做到哪一批，这三处都必须重写成与实测一致的范围声明**——按现状投稿，等于主动
告诉审稿人评测未完成。

---

## 五、样本预算（算术，非实测）

契约统一 `samples: 30` + `warmups: 5`，即每格 35 次执行。

| 范围 | cell | 执行次数 |
|---|---|---|
| 全 118 格 | 118 | 4,130 |
| 本计划 76 格 | 76 | 2,660 |
| 已完成 artifact | 6 | 210（当时按 pilot 跑的是 1 样本 0 预热，正式 campaign 需重跑） |

**墙钟无法从现有数据外推**：2026-08-16 那轮是每格 1 样本 0 预热，且小规模格的耗时由
provisioning 与结算主导而非查询本身。B1 完成后应先跑一格完整 35 次执行做标定，再据此
排 campaign 的时间表。

---

## 六、执行顺序与依赖

```
A1 ──────────────────────────────► （立即，解除后续所有步骤的测试税）
A2 ──► A3 ───────────────────────► （与 B 并行，越早做省得越多）
B0 ──► B1 ──► B2 ──► B3 ──► B4 ──► B5
        │
        └─ B1 需先实测确认路由闸门已解除
              B2b 需先把 depth4 产品接进主 Catalog
```

A1 必须最先做：它决定 B1–B3 每一步的回归成本。A2/A3 与 B 并行，不互相阻塞。

---

## 七、需要作者的四个点

1. **范围确认**：76 格（44 格转 future work）认不认。
2. **B0 放行**：`artifactRealSystemValidated` 翻 true。
3. **A1 放行**：验收规则从「每任务一次带库全量」改为三层。此项不影响论文主张与指标，
   按已下放的决策权 Claude 可自行裁决，但因其修改既有任务模板，仍先告知。
4. **B4 逐字节签署**：不可下放，到点必须作者本人操作。

---

## 八、照实登记的未知与风险

- `internal/gateway` 的逐测试耗时构成**尚未测得**，A3 的具体改法在 A2 出结果前不预设。
- 路由闸门是否真正解除**尚未实跑验证**，只核实了 Catalog 现状。
- B1a/B1b 两个具名阻塞项的工作量**无法估计**：Claude 未实现过其中任何一项。唯一可参照的
  摩擦系数是 artifact 那 6 格——在契约早已解析的前提下，从开跑到真跑通，光环境与接线就
  耗了三轮、两次六格全灭（ProfileBinding 假象、observer 拓扑漂移、DSN 字符集各一次）。
- baseline S3 三个档位标签 `1k-5k`/`10k-50k`/`45k-225k` 的含义为**推断**（1:5 比例与决策
  D3「每保留行派生五个 Fact」吻合），代码中无解析器。

---

## 九、性能与规模实验方案的进一步优化（2026-08-16 追加）

作者要求从时间、空间、资源三方面再想。以下逐条附实测与出处；未实测的标注为估算或未知。

### 9.1 先纠正一个直觉：固定开销不是瓶颈

2026-08-16 那轮 targeted 运行的阶段时间由留存文件 mtime 反推：

| 阶段 | 时长 |
|---|---|
| formal Gateway build（`--no-cache`） | 2:09 |
| 部署 + 快照编译 + 产物复制 | 3:55 |
| 三条 live gate | 0:21 |
| **固定开销小计** | **6:25** |
| 测量窗口（6 格 × 1 样本） | 0:58 |
| 总计 | 7:27 |

campaign 的结构是 `run-three-deployments.ps1` 循环 3 次部署，每次部署内跑完九个实验
（`run-deployment.sh:20` 限定 `deployment-01..03`），**不是每格一次部署**。所以固定开销
全程约 19 分钟。**在这里优化收益极小，不要把力气花在这。**

### 9.2 真正的时间瓶颈：scale 的 fresh_root 与 history_prefill

scale 契约 60 格**全部** `fresh_root: true`，且 `history_prefill: BEFORE_MEASUREMENT`
（`contracts/scale-v1.json`）。dependency-e2e 有 8 格落在 `1035000` Fact 量级
（4 个 overlap × novel/semantic_replay），每格 30 样本。

**若每个样本都重新预填 103.5 万 Fact，就是 240 次百万级预填。** 以今天 100k 行走完整路径
17.7 s 作线性外推，单次预填约 3 分钟量级，240 次即约 12 小时——且这是**每次部署**，
乘 3。**这是一个估算，不是实测**，线性外推本身也未验证。

关键在于：这部分**尚未实现**（60 格状态为 `PENDING_QUERY_IDENTITY_ADAPTER` /
`PENDING_30_SAMPLE_SCHEDULE_REPLACEMENT`），所以**预填与 root 复位策略现在还是自由选择**。
它是全局最大的时间杠杆，应当在 B1a 里作为**显式设计决策**定下来，而不是让它从实现里长出来。
`measured.include_prefill: false` 说明预填不计入测量值，但仍占墙钟。

**B1a 的第一件事应是一次预填标定**：单格跑满 35 次执行，实测预填与测量各占多少，
再决定策略。不做标定就实现，等于把最大的成本项交给运气。

### 9.3 可以直接拿的小项

- **formal build 复用**：`run-deployment.sh:261` 明确使用 `--no-cache` 构建。同一
  submission commit 下三次部署产出同一镜像，可建一次后按 digest 复用，省约 4 分钟。
- **不要并行三次部署**：会污染性能测量（这是性能实验，不是功能实验），且主机只有
  23 GB 内存扛不住。此条写进计划，防止以后被当成「优化」。

### 9.4 空间

| 项 | 实测 | 处置 |
|---|---|---|
| 每轮复制两份完整产物 | 6.4 GB/轮 | 见 A4，待改 launcher |
| Docker build cache | 27.44 GB（可回收 25.22） | `--no-cache` 构建仍写缓存，无上限增长；给 runner cleanup 加 `docker builder prune --keep-storage` |
| 每次部署的 `snapshot-index-artifacts` 卷 | 约 4.5 GB | 见 9.5 |
| 被取代的 v1.8 / v1.9 qualification 目录 | 11.7 GB | 作者已批准，2026-08-16 删除其 ignored 大产物子目录，回收 12 GB；四个已入库文件各自保留 |

### 9.5 快照产物跨部署复用——**已由 Claude 收回，不做**

原提议：快照 publication 产物是确定性的，`sidecar_digest`/`dictionary_digest`/`manifest_digest`
已在 `config/catalog.yaml` 逐个 pin，故可用按 digest 校验的共享卷跨部署复用，省编译时间与卷空间。

**收回理由（作者 2026-08-16 追问「改实验协议是否影响论文思想」后实查得出）：**

1. **不影响论文思想**：摘要与五条贡献全是语义与机制，没有一条依赖索引编译了几次。
2. **但影响一个已报数字**：`\RQFourVFourBuildRuns` = `1`（`paper/tkde/generated/evidence.tex:147`），
   直接印在 `main.tex:1397`「Only 1 build and one fixed-host deployment were measured.」，
   `main.tex:1365-1366` 亦写「one index build and deployment rather than 140 independent deployments」。
   **索引构建次数是论文里的已报量。** 跨部署复用等于 V5 campaign 只有 1 次索引构建被 3 次部署共享，
   而不是 3 次独立构建——主动放弃一个本可改善的数字。
3. **收益量级不成比例**：今天实测「部署 + 快照编译 + 产物复制」合计 3:55，复用最多省其中一部分，
   3 次部署合计**分钟级**；而 9.2 的预填是**小时级**估算。差两个数量级。
4. **另有必须隔离处**：RQ5 的 `\RQFiveTotalBuildRuns` = `12` 是**被测对象**
   （`supplement.tex:953`「All 12 measured cycles satisfy the declared five-minute…」），
   任何复用都绝不能触及 RQ5 那条构建路径。

**结论：不做复用，保留每部署独立编译，据实报 3 次独立索引构建。**

**根因登记（CLAUDE.md 第 7 条）：** 提出 9.5 时未先查论文报了哪些数字，也未把收益与 9.2 的
量级作对比。这是本轮第三次同类根因——提建议前没查权威来源。补一条具体做法：
**任何拿工程代价换论文内容的建议，必须先查论文实际报了哪些数、并给出收益量级，再提出。**

### 9.6 资源：一个必须在 B1a 之前验的风险

主机实测：**32 核 / 23 GB 内存**（可用 19 GB）。compose 里 `snapshot-index-result-heavy`、
`snapshot-index-exposure-scale`、`snapshot-sidecar-install` 各 `mem_limit: 16g`（顺序执行、
不并发），Gateway `mem_limit: 12g`。

2026-08-16 那轮 Gateway cgroup 峰值实测 **3,636,482,048 B（3.39 GiB）**，工作量是
100k 行 × 16 列。

**风险原文**（`docs/getting-started.md:213`）：

> 当前实现的 Connector/可见结果到 Parquet 的部分路径仍会**在内存中持有完整结果**……
> 因此 `GATEWAY_CONNECTOR_MAX_ROWS=1200000` 是关闭式行上限，**不是百万行路径已具备有界内存的证明**。

dependency-e2e 的 `1035000` Fact 格正好落在这个「未被证明」的区间里，且 `.env` 实测
`GATEWAY_CONNECTOR_MAX_ROWS=1200000` 只比它高 16%。**Gateway 内存是否随 Fact 数线性增长
尚未测过**；若线性，103.5 万相对今天 10 万即 10 倍，会逼近甚至超过 12g 容器上限，
在 23 GB 主机上没有多少余量。

**结论：B1a 实现之前，应先用一格 `1035000-overlap-0` 做一次内存标定。**
这比实现完再发现 OOM 便宜得多。另：32 核在测量阶段基本闲置（测量本身串行），
多核只对 A3 的测试套件优化有用。

### 9.7 归口

| 项 | 谁定 |
|---|---|
| 9.3 build 复用、不并行、9.4 build cache 上限 | Claude 直接做 |
| 9.2 预填标定、9.6 内存标定 | Claude 直接做，结果上报 |
| 9.5 快照产物跨部署复用 | ~~作者裁决~~ **已收回，不做** |
| 9.4 删 v1.8/v1.9 qualification | 作者 2026-08-16 批准，已执行，回收 12 GB |

### 9.8 内存：主机真实约束与对 9.6 的两处更正（作者 2026-08-16 问「12 GB 提到 18 GB 够吗」后实查）

**主机实测（此前只记了 WSL 内部数字，未查 Windows 侧）：**

| 项 | 实测 |
|---|---|
| Windows 物理内存 | `34,081,603,584` B = 31.7 GiB |
| WSL2 上限 | `/mnt/c/Users/buckw/.wslconfig` 的 `memory=24GB`，`MemTotal` 实测 24,604,380 kB = 23.5 GiB |
| WSL2 swap | `swap=24GB`，`swapfile=D:\\wsl-swap.vhdx` |
| `autoMemoryReclaim` | disabled |
| Gateway 容器 | `mem_limit: 12g`（`compose.yaml:425`） |

**（一）18g 作为上限无害，作为实际用量主机给不起。** 24 GB 的 VM 里还要装 4 个 PostgreSQL
（business/control/direct/provsql）、MinIO、oa-demo，加宿主侧 runner/adapter/observer 进程，
以及 PostgreSQL 强依赖的 page cache（当前 buff/cache 实测 10 GiB）。若 Gateway 真吃到 18 GB，
其余只剩约 5 GB。**上限的作用是 fail-closed，不是配额**；设成 18g 只会让内存耗尽发生得更晚、
并落到别的进程头上。

**（二）swap 是比 OOM 更严重的隐患。** 配了 24 GB swap。**在性能测量期间发生 swap 比 OOM 更糟**
——OOM 会失败并被发现，swap 会产出看似合理、实则被污染的时序数据。建议正式 campaign 期间
把 swap 关掉或调到很小，让内存不足以失败的形式暴露，而不是悄悄污染数字。

**（三）比改容器上限更实在的一步**：`.wslconfig` 的 `memory` 从 24GB 提到 26–28GB
（物理 32 GB，给 Windows 留 4–6 GB）。这同时惠及 PostgreSQL 与 page cache。改完需 `wsl --shutdown`。

**（四）更正 9.6 的两处，均为 Claude 自己讲错：**

1. **「10 倍于今天」讲错了。** `1,035,000` 是 **Fact** 数不是行数；按每保留行派生五个 Fact
   （decision D3，`compose.yaml:153` 亦记 exposure-scale 为 414,000 行），1,035,000 Fact ≈
   **207,000 行**，只有今天 100k 行那格的约 2 倍，不是 10 倍。
2. **把风险机制安错了地方。** dependency-e2e 的两条查询实为聚合：
   `scale-dependency-history-bdg.sql` 是 `SELECT sum(metric)`，candidate 是 `SELECT count(*)`,
   **可见结果各只有一行一列**。而 Gateway 是「缓冲可见查询、流式读取 ordinal companion」
   （`docs/getting-started.md`），所以「在内存中持有完整结果」这条风险主要落在
   **result-heavy 那类 100k 行 × 16 列的 artifact 格**（今天实测峰值 3.39 GiB），
   **不是 dependency-e2e**。dependency-e2e 的内存压力在记账结构侧——重叠度 0 时
   `UnionFacts = 2 × 1,035,000 = 2,070,000` 个 Fact 的 bitmap 与 Merkle-radix 集合。

**结论不变但警戒级别下调**：仍需一次 `1035000-overlap-0` 内存标定，但它测的是记账结构的
内存曲线，不是「百万行结果撑爆内存」。**在标定出数字之前，不建议凭猜把 12g 改成 18g。**

---

## 十、内存共享与 NAS 方案设计（作者 2026-08-16 提出）

作者提出两条：让 WSL 与宿主机共享 32 GB 内存；把 PostgreSQL 迁到 NAS。
下面按这两条设计。**第一条可做且应做；第二条对被测数据库不可行，原因逐条列出，
并给出 NAS 真正该承担的角色。** NAS 侧凭据与地址一律不写入本仓库，只引用
`/home/wmm/auth/nas/` 下的私有文档。

### 10.1 内存：双档 `.wslconfig`

当前实测：Windows 物理 `34,081,603,584` B（31.7 GiB）；`.wslconfig` 为
`memory=24GB`、`swap=24GB`、`swapfile=D:\\wsl-swap.vhdx`、`autoMemoryReclaim=disabled`。

**问题不只是总量，还有 swap。** 24 GB swap 意味着内存吃紧时系统会静默换页而不是失败。
**在性能测量期间，swap 比 OOM 严重得多**：OOM 会失败并被发现，swap 产出的是看似合理、
实则被污染的时序数据，而那正是要写进论文的数字。

设计为两档，按用途切换（改后需 `wsl --shutdown`）：

| 档 | memory | swap | autoMemoryReclaim | 用途 |
|---|---|---|---|---|
| 日常档 | `28GB` | `8GB` | `gradual` | 写代码、跑聚焦测试、构建。空闲内存交还 Windows，真正实现「共享」 |
| 测量档 | `28GB` | `0` | `disabled` | 正式 campaign 与任何要留证的性能测量。宁可 OOM，不要静默换页 |

`28GB` 的取法：物理 31.7 GiB 留约 4 GiB 给 Windows。相对现状净增约 4 GB 可用。
**测量档必须关 reclaim**：回收与再缺页会在测量窗口内引入方差。

配套：`compose.yaml` 的 Gateway `mem_limit` 在 9.6 的标定出数字前不动（今日实测峰值
3.39 GiB，距 12g 仍有约 3.5 倍余量）。上限是 fail-closed，不是配额。

### 10.2 NAS 不能承载被测数据库——五条实查依据

| # | 依据 | 出处 |
|---|---|---|
| 1 | **版本不符且不可绕过。** 契约要求 `postgresql_version_num = 160014`（PostgreSQL 16.14），硬编码在 `observer_accounting_v3.go:84` 的 `RequiredMeasurementEnvironment`，并由 `finalv5publication/validate.go:212`、`generate.go:207` 各自复验；`db-test-env.sh verify` 也断言该值。**NAS 上是 PostgreSQL 12.7**，差四个大版本，而 observer 的记账正建立在 `pg_stat_statements` 的字段与 `track_planning` 等设置上 | `/home/wmm/auth/nas/DATABASE-STATUS.md` |
| 2 | **内存不够且有前科。** DS423+ 总内存 9.6 GB + swap 8.1 GB = 17.7 GB，低于当前 WSL 的 24 GB；而 `compose.yaml` 里三个快照编译器各 `mem_limit: 16g`。NAS 已于 2026-08-10 因内存暴涨发生 OOM、Docker 连锁崩溃 | `nas-config.md`、`OOM-INCIDENT-20260810.md` |
| 3 | **这是性能实验。** 数据库跨网络后，RQ4 的 p50/p95 测到的是局域网往返，不是 BDG 的开销 | 论文 `main.tex:1348` 的 RQ4 行 |
| 4 | **拓扑与存证同时失效。** observer 的封闭服务集要求 `business-postgres` 等作为部署内的 Compose 服务（2026-08-16 刚修的那条）；qualification 还 pin 了 PostgreSQL 的 `image_reference`/`repo_digest`/`local_image_id`/`container_image_id`/`platform`。数据库搬出部署，这两条一起断 | `evaluation/cmd/final-v5-observer/main.go:67-90`、`run-artifact-targeted.sh` 的 identity 校验 |
| 5 | **它是家里的生产库。** NAS 上运行着 15 个 MySQL 业务库与 PostgreSQL `rd_studio`（373 张表）。而实验要求 `fresh_deployment`——每次 campaign 清库重建 35 个 `taskgate_ordinal` relation。在生产 NAS 上做这件事风险不可接受 | `DATABASE-STATUS.md` |

**补充**：即使只把 A3 的 `db-test-env` 挪到 NAS 也无收益——同样卡在 16.14，且把
`internal/gateway` 那 4000 秒的数据库往返搬到网络上只会更慢。

### 10.3 NAS 该承担的角色：证据归档

作者真正的痛点是磁盘，而 NAS 恰好适合解决它，且完全不触碰任何测量或门禁。

实测依据：每轮 targeted 运行落盘 6.4 GB（`snapshot-index-artifacts-full` 4.5 GB +
`profile-artifacts` 1.9 GB），`raw/` 一度到 69 GB；而每轮真正入库留证的只有 16 个小文件、
845,008 bytes。这些大产物**写一次、之后只读**，正是归档负载。

设计：

1. **A4 落地**：让 launcher 默认不复制完整产物；需要时按 catalog 中已 pin 的
   `sidecar_digest`/`dictionary_digest`/`manifest_digest` 校验后再取。
2. **归档而非留在本地**：确需保留的整份产物，运行结束后同步到 NAS 的归档目录，
   本地只留 16 个入库小文件。同步后按 digest 复核再删本地副本。
3. **不放**：`.env`、任何凭据、任何未经作者批准的 approval 字节。
4. **不依赖**：measurement 路径不得从 NAS 读取任何东西——归档是单向的，
   NAS 不可用不影响任何实验。

这条能把 `raw/` 的稳态占用压到 GB 以下，且**不改变任何被测语义**。

### 10.4 若作者坚持在 NAS 上跑被测数据库

唯一不破坏论文的前提是：在 NAS 上另起 **PostgreSQL 16.14** 容器并复现全部
`RequiredMeasurementEnvironment` 设置，且接受 (a) 网络往返进入测量值、
(b) DS423+ 9.6 GB 内存扛四个实例、(c) qualification 的容器 identity 需重新定义。
**Claude 的判断是不值**：第 3 条使测得的数字无法支撑论文的性能主张，
而痛点（磁盘）用 10.3 已能解决。此判断可由作者推翻，但需连带裁决 RQ4 的口径。

---

## 十一、24 小时交付方案（作者 2026-08-16 03:05 定目标）

作者目标：**24 小时内交付一篇带实验结果的论文**。截止约 2026-08-17 03:00。
本节是对全部讨论的收敛，也是唯一的执行清单。

### 11.1 先说清楚：24 小时内做不到什么

**做不到 9/9，做不到 76 格，做不到任何一个未实现的 baseline / scale 格。** 依据：

- 118 格全部处于 `PENDING_IMPLEMENTATION` / `PENDING_QUERY_IDENTITY_ADAPTER` /
  `PENDING_30_SAMPLE_SCHEDULE_REPLACEMENT`；每格除实现执行路径外，还要 dataset binding、
  oracle manifest、task 定义，再加生成期望字节与**作者逐字节签署**（红线 1，不可代签）。
- 摩擦系数的唯一实测参照：artifact 那 6 格在契约早已解析、执行路径早已存在的前提下，
  从开跑到真跑通仍耗了三轮、两次六格全灭（ProfileBinding 假象、observer 拓扑漂移、
  DSN 字符集各一次）。

把这些塞进 24 小时只有两种结果：交不出，或者交出未经验证的东西。两者都不可接受。

### 11.2 24 小时内能交付什么

**一篇带真实、有分布的 Artifact / result-heavy 实验结果的论文**，并把论文里三处
「评测未完成」的散文改写成与实测一致的范围声明。

具体证据形态：**6 个冻结 cell × 3 次独立部署 × 3 样本 = 每格 9 个测量**，每次部署前
5 次预热。这是 targeted launcher 允许的上限（`SAMPLES` 硬校验 1–3）。

可产出的论文数字：

- 每格 `client_full_drain_ms` 的中位数与全距（9 个测量）
- Gateway cgroup 峰值（今日单样本实测 3.39 GiB，多样本可给区间）
- `parquet_bytes` / `encrypted_object_bytes`（今日实测 6,235 → 18,743,852 B）
- v3 acceptance 通过率、`receipt_verified` 通过率、`unexpected` 调用数
- **3 次独立部署**——直接改写 `main.tex:1365-1366` 那句「one index build and deployment」

### 11.3 时间预算（基于实测，非估计）

单次 targeted 运行实测分解：固定开销 6:25（build 2:09 + 部署与快照编译 3:55 + gates 0:21），
测量窗口 6 格 × 1 样本 = 58 s。

| 项 | 计算 | 时长 |
|---|---|---|
| 每次部署固定开销 | 实测 | 6:25 |
| 每次部署测量窗口 | (5 预热 + 3 样本) × 58 s | 7:44 |
| 单次部署合计 | | **≈ 14:10** |
| 三次独立部署 | × 3 | **≈ 43 分钟** |

**43 分钟的实验，24 小时的窗口。** 时间不是瓶颈，失败重试和论文改写才是。

### 11.4 执行清单（按小时）

| 时段 | 动作 | 阻塞 |
|---|---|---|
| **H0–H1** | `.wslconfig` 改 `memory=30GB` / `swap=0` / `autoMemoryReclaim=disabled`；作者执行 `wsl --shutdown`；A1 三层规则落到 `AGENTS.md` 与任务模板；证据归档改指 `/mnt/d/wsl-data/taskgate-evidence` | 需作者执行 shutdown |
| **H1–H2** | B0：翻 `artifactRealSystemValidated`、修 `capability.go` 过期注释、`tkde_revision_status.md` 对齐；跑受影响包验收 | **需作者批准翻 capability** |
| **H2–H3** | 三次独立 targeted 运行（`SAMPLES=3`、`WARMUPS=5`，三个不同 `RUN_ID`），实测约 43 分钟 | 无 |
| **H3–H6** | **失败缓冲**。按摩擦系数，这一段大概率会用掉 | 无 |
| **H6–H10** | 从 9 个样本生成论文宏；改写 `main.tex:1363`、`1365-1366`、`1397-1399`；新增 Artifact 证据行 | 无 |
| **H10–H14** | 容器化 IEEE 构建、复核页数与摘要字数、`git diff --check`、凭据扫描 | 无 |
| **H14–H20** | **作者审阅与签署**（若决定把该轮升为 publication 证据） | **需作者** |
| **H20–H24** | 终稿构建与留证 | 无 |

### 11.5 一个必须作者拍板的口径问题

targeted launcher 产出的样本恒为 `campaign_class=pilot`、`publication_eligible=false`。
两条路，**必须二选一，不能含糊**：

- **(A) 如实报为 pilot**：论文写「Artifact 路径在三次独立部署上完成端到端验证，
  每格 9 个测量」，并明说这是 pilot、不是冻结 campaign。**不需要签署，24 小时内可交付。**
- **(B) 升为 publication 证据**：需要改运行类并由作者逐字节签署生成的期望摘要与 oracle
  manifest（红线 1）。**能不能在 24 小时内完成取决于作者何时签。**

**Claude 建议 (A)。** 理由：论文摘要与五条贡献都不靠性能立论，pilot 级的真实端到端证据
已经能把 `1363` 那句欠条换成有内容的范围声明；而 (B) 的收益是标签，代价是签署往返。

### 11.6 证据归档改到 `/mnt/d/wsl-data`

作者定：不进 git 的存档放 `/mnt/d/wsl-data`，比 NAS 快。**实测支持这个判断**：
`D:\` 剩余 1.5 TB，顺序写 **169 MB/s**（256 MiB / 1.59 s），6.4 GB 的一轮产物约 38 秒。
更关键的是它在 Windows 卷上，**不进 `ext4.vhdx`，所以不会再推高 C 盘占用**。

落地：
1. 归档根目录 `/mnt/d/wsl-data/taskgate-evidence/`（已建）。
2. 每轮运行结束后，把 `snapshot-index-artifacts-full/` 与 `profile-artifacts/` 移到
   归档根下按 `RUN_ID` 分目录，本地只留 16 个入库小文件。
3. **单向**：measurement 路径不得从 `/mnt/d` 读任何东西；归档不可用不影响任何实验。
4. 不放 `.env`、任何凭据、任何未经作者批准的 approval 字节。

注意 `/mnt/d` 是 9p/drvfs，**大量小文件很慢，大文件顺序写很快**——归档正好是后者。

### 11.7 24 小时内明确不做

- A3（`internal/gateway` 测试提速）：收益在未来每次全量，但改造加验证要一次完整跑，
  在冲刺窗口里是风险而非收益。**榜单已留存，改造留到交付之后。**
- A4 的 launcher 改造：用 11.6 的归档脚本先顶住，改代码留到交付之后。
- NAS：作者已定不考虑。
- B1 / B2 / B3 / B4：见 11.1。
