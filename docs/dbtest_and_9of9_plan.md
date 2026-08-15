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
| 被取代的 v1.8 / v1.9 qualification 目录 | 11.6 GB | 保留中，等作者发话 |

### 9.5 待作者裁决：快照产物能否跨部署按 digest 复用

快照 publication 产物是**确定性**的，其 `sidecar_digest`、`dictionary_digest`、
`manifest_digest` 已在 `config/catalog.yaml` 中逐个 pin，`cmd/snapshot-index` 本身也带
`-allow-existing-identical`。理论上可以用一个**按 digest 校验的共享卷**跨部署复用，
而不是每次部署从 PostgreSQL 重新编译——同时省时间与空间。

**论证**：`fresh_deployment` 约束的应是数据库与账本状态，而 publication 产物是不可变、
digest 已 pin、且 Gateway 启动时会校验的。**但这确实改变了「fresh」的含义，属实验协议改动，
不由 Claude 裁决。** 若作者认可，收益是每次部署省掉快照编译（今天实测部署+编译共 3:55 中的大头）
与每部署 4.5 GB 卷。

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
| 9.5 快照产物跨部署复用 | **作者裁决**（改实验协议含义） |
| 9.4 删 v1.8/v1.9 qualification | **作者裁决** |
