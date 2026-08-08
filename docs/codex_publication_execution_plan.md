# TaskGate 发表执行计划（Codex 长期执行）

> 目标：**原型可信 → 实验可复现 → TKDE 投稿并发表**。
> 本文件是可执行的任务队列，不是愿景文档。Codex 每完成一项就更新
> 「§10 进度台账」里的状态，并在提交信息里引用任务 ID（例如 `P2.2`）。
>
> 基线：分支 `tkde-artifact-rerun`，工作树 `/home/wmm/worktrees/taskgate-artifact-rerun`，
> HEAD `38b48bd`（2026-08-08）。主工作树 `/home/wmm/agent-scope/task_gateway` 停在
> `main`，永不触碰。

---

## 1. 三条验收线

| 线 | 完成的定义 |
|---|---|
| **原型（Prototype）** | v3 finalizer 是三条 TaskGate 路径（Artifact/Scale/ProvSQL）唯一的验收路径；v1.4 accounting 在仓库中零活引用；`go test -count=1 ./...` 在**接了真实 PostgreSQL** 的环境下全绿；`validate.sh` 无 SKIP 退出 0 |
| **实验（Experiment）** | 契约 v1.5 冻结；9/9 能力为 true；正式九实验 campaign 跑完并有 Campaign ID；`paper/tkde/generated/evidence.tex` 由真实证据重新生成且 `make paper-final-check` 通过 |
| **论文（Paper）** | 12 页正文 + supplement 编译通过；RQ 表与 Reproducibility Boundary 全部由实测数字支撑；作者逐字节批准；cover letter + 关系披露就绪；ScholarOne 提交 |

三条线是**严格串行**的：没有原型线就没有可信实验，没有实验线论文里的
"pending the final frozen WSL2 campaign" 会直接导致 desk reject。

---

## 2. 红线（任何情况下不得越界）

这些不是风格偏好，是这个仓库的证据完整性契约。违反其中任何一条，之前所有工作作废。

1. **不得伪造、复制、回填任何证据字节。** 不得把 fixture 摘要写进 manifest，不得把
   旧证据文件重新打上新 contract release 标签，不得用合成占位符满足 oracle。
   拿不到就报"拿不到"，写进台账。
2. **不得在 v1.4 活引用集合非空前**做下列任何一件：formal build、N4 重认证、
   100×4 canary、v1.5 冻结、任何能力从 false 翻 true。
3. **不得移动 tag。** `final-v5-contracts-v1 … v1.4` 由作者掌握；Codex 只推分支
   （`git push -u origin tkde-artifact-rerun`），永不推 tag。
4. **不得给 v3 失败加 v1.4 回退。** v3 验收失败 ⇒ 该 sample 是 `fail`，不是降级。
5. **不得在被测方包内实现 finalizer 资源。** 五个 resolver 的真实实现落在
   `evaluation/internal/experiment`，绝不落在 `evaluation/cmd/final-v5-adapter`。
6. **不得安装宿主机 PostgreSQL。** 只用 `scripts/db-test-env.sh` 拉起的
   digest-pinned PostgreSQL 16.14 容器对。
7. **不得改 `run-deployment.sh` 去承载 Artifact 定向跑。** 它服务 MASTER
   `config/catalog.yaml`；Artifact 用 `run-artifact-targeted.sh`。
8. **不得重开已判定的设计决策**（见 §3 的"已封存决策"）。有新证据要推翻时，
   先在台账里写明证据，交作者裁决，不要自行改。
9. **skip 不等于 pass。** DB 测试 skip、`contract SQL executability: SKIPPED`
   在冻结时一律记为未通过。

---

## 3. 当前状态基线（写给接手者，避免重新推导）

**已完成（不要重做）**

- Receipt 版本阶梯已塌缩为**唯一版本**，形状从签名内容读出；`COMPLETED` ⟺ 存在
  `QueryExecutionBindingV2`。用 `RequireCurrentVersion` / `DescribesAnExecution`，
  **永不做版本大小比较**。
- Gateway 只经 `internal/physicalquery.Prepare` / `PrepareSemanticView` 准备语句，
  自有推导已删除；不存在第二份推导。
- finalizer 自己复现执行：`ReproduceExecutionV3`（Prepare → `RequireInputs` →
  `RequirePreparedSame` → `Derive` → 三个行上限必须等于签名值）。
- `RuntimeFinalizerV3` 是唯一生产入口；构造函数与五个 collaborator 全部包私有，
  Adapter 只能提交证据，无法选择自己被对照的档案。
- 30 条 v3 集成门全部 PASS；分类器 manifest v2（路径感知闭世界）已定案。
- Artifact（`result-heavy`）路径代码级已切到 v3；`run-artifact-targeted.sh` 存在。
- 2026-08-08 全新活体激活：7 个 live-route profile 已 activation-supported /
  smoke-passed / targeted-run eligible；54 探针外产品矩阵全拒绝，540 条负断言通过。
- 当前 HEAD 的 Attestation footprint 可移植重认证（i2b-01/02）一致。

**未完成（这就是本计划的全部内容）**

- Scale（`executeDependencyE2E`）、ProvSQL（`executeProvSQLTaskGate`）仍建 v1.4
  accounting；`retiredV14ActiveReferences` 非空（5 个文件）。
- v1.4 路径**根本无法执行**：`captureBoundObserver` 用 `DisallowUnknownFields`
  解 v1 快照，而 observer 只发 `ObserverSnapshotV2` 且必须收
  `--observer-window-id` 与 `--classifier-manifest-sha256`。任何 I1-B 之前的绿色
  Artifact/Scale/ProvSQL 结果**测的是已不存在的路径**——包括当前记为 true 的
  ProvSQL 能力（见 P2.6）。
- Baseline S5 的六个 BDG 臂用了 branch-filtered UNION DISTINCT，在 exposure-v5 下
  准备阶段 `POLICY_DENIED` 失败（`queryplan.PredicateBindings` 按 product 名键控，
  分支限定列解析不到）。**Baseline 能力因此拿不到。**
- 在线 inner equijoin 未实现（需要 qualified-column 解析 + 双源 provenance 重建），
  论文当前把 Join 限定为 algebra/oracle-only。
- 5 个 `internal/gateway` 测试在接真库时失败（`provsql-orders-v1` publication 无法
  持久化），历史遗留，非本次引入。
- 契约 release 停在 v1.4；能力 6/9；`artifactRealSystemValidated=false`；
  11 个 profile 全部 `targeted_validation_passed=false`；`.NOT_READY` 在位；
  无 Campaign ID；论文数字未更新。

**已封存决策（不要重开）**

- Adapter 不能自行推导分类器 manifest ⇒ `OpenObserverWindowV3` 返回整个
  `PreRegisteredObservationV3` 由 Adapter 带回。这**不是**第二次推导，不得如此引用。
- `validateArtifactVerification` 只检查 sample 是被验收的那一个，不再从 sample
  重推验收。
- 只有一个 lowering product 构造：`physicalquery.QueryProductFromCatalog`。
- `profileMaterialResolverV3` 与 `contractResolverV3` 保持分离（论文要陈述两条正交轴）。
- 不给五个 resolver 写 stub；每个真实实现随第一个需要它的 cutover 落地。
- 幂等重放的 manifest 是 `Entries = []` 的真文档，这是最强契约不是放松。
- `PreparedOperationBindingV1` 不再扩展成员（会作废当前 release 的全部激活）。

---

## 4. 关键路径与依赖

```
P0 基线复核
  │
  ├─► P1a 限定列重构 ──► P1b 在线 Join ──┐
  │       (Baseline 能力前置)             │
  │                                       │
  └─► P2  v3 切换收尾 ───────────────────┤
          (Scale/ProvSQL/observer/ratchet)
                                          │
                                          ▼
                                     P3 形式化 + N4 + 100×4 canary
                                          │
                                          ▼
                                     P4 契约 v1.5 冻结  ◄── 作者批准
                                          │
                                          ▼
                                     P5 正式实测 → 9/9  ◄── 私有 dataset binding
                                          │
                                          ▼
                                     P6 证据合并 + 论文数字
                                          │
                                          ▼
                                     P7 论文文本 + 审批 + 投稿

PX  支线（文档/pin/清理，随时可做）
```

P1a → P1b 串行（Join 需要限定列解析）；P1a/P1b 这条线与 P2 可并行（不同文件）。

**作者决策 17 之后，P1b 进入关键路径，不再是可并行的加分项。** 理由：它改的是
运行时语义，而 P3 的 canary、P4 的冻结、P5 的 campaign 都会把当时的运行时行为
封进证据。Join 若在 campaign 之后落地，全部实测证据作废重跑。所以顺序只能是
**先改完运行时，再冻结，再测量**。

---

## 5. 阶段任务

每个任务格式：`ID · 名称`｜前置｜目标｜步骤｜验收｜产出。
"验收"里的命令必须真的跑过并贴输出，不得推断。

### P0 — 基线复核（预计 0.5 天）

**P0.1 环境自检**
- 前置：无
- 步骤：`docker version`（Docker 反复上下线，必须每次实测）；
  `go version`（宿主 go1.25.12，`export GOFLAGS=-buildvcs=false`）；
  `./scripts/db-test-env.sh up` → `eval "$(./scripts/db-test-env.sh env)"` →
  `./scripts/db-test-env.sh verify`（期望 `server_version_num=160014`，
  31 个 `taskgate_ordinal` sidecar relation）。
- 验收：三条命令输出贴进台账。若 Docker 不可用，**停止一切 P3–P6 任务**，只做
  P1a/P2 的纯代码部分与 PX。

**P0.2 带库全量回归（这是第一次真正的基线）**
- 前置：P0.1
- 步骤：`gofmt -l $(git ls-files '*.go')`；`go build ./...`；`go vet ./...`；
  在导出 DSN 的 shell 里 `go test -count=1 ./...`。
- 验收：记录**所有**失败用例。已知失败：5 个 `internal/gateway` 用例（见 P2.6）。
  出现其他失败一律先查清再往下走。
- 产出：台账 `P0.2` 行 + 失败清单。

**P0.3 门禁基线**
- 步骤：`./evaluation/final-v5-wsl2/scripts/validate.sh`（记录每一条 SKIP）；
  `go test ./evaluation/internal/experiment/ -run 'V14|Cutover|ProductionCallers' -v`。
- 验收：确认 `TestV3CutoverIsBlockedByTheUnwiredFinalizer` 仍存在且通过（它此刻
  **应该**通过——它断言切换未完成）。

---

### P1a — Baseline S5 关系片段解锁（关键路径，预计 3–8 天）

**这是 Baseline 能力唯一的技术阻塞。** 现状：`sql/contracts/S5-bdg-plan.json` 是
`union_distinct` over `provsql_orders`，两个分支各带 `partition_key`/`orderkey` 过滤；
`provsql_orders` 走 `final-v5-provsql-low-v1`（= `taskgate-exposure-v5`），
V5 要建 predicate footprint，而 `queryplan.PredicateBindings` 按 product 名键控，
`left_branch.expense_type` 解析为空 ⇒ 准备阶段 `POLICY_DENIED` 失败关闭。

**P1a.0 决策门 — 已裁决（2026-08-08，作者决策 16）**
- 结论：**限定列重构**。重述 S5 被否决（与作者决策 8 冲突：S5 BDG 固定为两分支
  structured union，不接受 plain-SQL 替代）；排除 S5 臂被否决（机制本可支持，
  排除是无谓削 claim）。
- 记录位置：`docs/final_v5_author_decisions.md` 第 16 条。**不要重开此项。**

**P1a.1 实施：限定列重构**
- 改 `queryplan.PredicateBindings` 的键控为 (branch role, product)，
  `Compile` 发出分支限定的列引用（`"l"."col"` / `"r"."col"`）。
- 保持 `internal/physicalquery` 为**唯一**实现；**不得**在 `internal/gateway`
  长出第二份推导（v14 ratchet 与 blocker 测试会拒绝，但更重要的是它会漂移，
  而漂移会被当成测量结果读）。
- 落地后 v1.5 契约可声明支持"**带分支局部谓词的两分支 UNION DISTINCT**"，
  措辞不得超过实测覆盖。

**P1a.2 收口测试**
- `TestUnionBranchFilteredPredicatesFailClosedUnderV5`：选项 1 下它必须**失败**，
  改成正面断言；选项 2/3 下保持。
- `TestTheBranchFilteredUnionDependencySetIsExactlyBaselineS5`：依赖集清空后按其
  自述指示删除该测试，并把 parity case 从"无过滤 UNION"升回"带过滤 UNION"。
- 验收：`go test -count=1 ./internal/... ./evaluation/...` 全绿（带库）。

---

### P1b — 在线 inner equijoin（已决定要做，作者决策 17）

原先"Join 仅 algebra/oracle-only"的收缩表述**已被作者撤回**。Join 要真正在线执行。
前置是 P1a.1——限定列解析正是 join key 跨源冲突所需要的那一半。

- **P1b.0** 先把 visible == provenance 行数不变式**显式写成测试**，再动它。
  目的：它的替代品是一条被陈述的契约，而不是被悄悄放松的假设。
- **P1b.1** 双源 provenance 重建：`deriveObservationV2` 从 joined provenance 行
  重建左右基关系 → `JoinOnV2` → observe。
- **P1b.2** **硬约束**：RQ4 测量的 settlement 路径不得回归。任何触及 `Derive`
  的改动，报告前必须跑完整 `evaluation/exposure` 全套并贴输出。
- **P1b.3** 落地后同步改论文 §Exposure Model 片段声明、§Evaluation、§Limitations
  的 Join 表述（P7.2），并把 oracle-only 的限定语删掉。
- **卡住时的处置**：若在不扰动 settlement 的前提下做不下来，**上报作者重新裁决**，
  **不得**自行退回 algebra-only 表述。写进台账并停在那里。

---

### P2 — v3 运行时切换收尾（关键路径，预计 5–10 天）

**P2.1 observer 捕获全面 V2 化**
- 前置：P0
- 目标：删除 `captureBoundObserver`（v1 解码路径），所有调用点走
  `captureBoundObserverV2`，并传 `--observer-window-id` 与
  `--classifier-manifest-sha256`。
- 验收：`grep -rn "captureBoundObserver\b" evaluation/` 无生产命中；
  observer 相关测试绿。

**P2.2 Scale 切换（`executeDependencyE2E`，仅 TaskGate 臂）**
- 前置：P2.1
- 步骤：调用 `RuntimeFinalizerV3.OpenObserverWindowV3` → 执行 → 提交
  `CarriedEvidenceV3` + `FrozenContractSelectorV3`；删除该路径的
  `NewGatewayControlPlan` / `applyObserverDelta`；
  该 cutover 需要的 resolver 真实实现落在 `evaluation/internal/experiment`。
- 验收：`retiredV14ActiveReferences` 中 `scale.go` 与
  `finalize_scale_artifact.go` 两项可删；v14 ratchet 测试绿。

**P2.3 ProvSQL 切换（`executeProvSQLTaskGate`，仅 `taskgate` 臂）**
- 同 P2.2 形式。`direct` 臂是裸 PostgreSQL，不进 Gateway，不动。
- 验收：`provsql.go` 从 ratchet 移除。

**P2.4 清空 ratchet 与遗留**
- 前置：P2.2 + P2.3
- 步骤：`adapter_bindings.go`、`types.go` 的 v1.4 符号清除；
  把 v1.4 schema/decoder 移进 `evaluation/internal/legacyv14`（该包目前**不存在**）
  并加 import guard，使其无法产出或接受 v1.5 sample；
  `retiredV14ActiveReferences` 置空。
- 验收：ratchet 测试在空集合下绿；`go build ./...` 绿。

**P2.5 翻转守卫**
- 前置：P2.4
- 步骤：`TestFinalizeObservationV3HasProductionCallers` 的 `t.Logf` 改 `t.Fatalf`
  并更新它要求的调用方文件列表（旧列表点名三个 Adapter 文件，而 Adapter 不能
  命名 trusted 类型——列表必须指向 finalizer 侧入口）；
  删除 `TestV3CutoverIsBlockedByTheUnwiredFinalizer`。
- 验收：两条守卫的 mutation 检查（临时把调用点注释掉，测试必须红）。

**P2.6 修复 5 个带库失败的 `internal/gateway` 测试**
- 前置：P0.2
- 现象：全部卡在持久化 `provsql-orders-v1` publication。
  `liveCompilerTestSnapshotIndex` 这个替身的 manifest 缺 cold-payload 与
  hot-index 摘要，`DictionaryManifest.Validate` 要求五个全在。
- **不得**用输入的 `expected_digests` 回填（会撞 `dictionary has no segments`），
  **不得**跳过 store 写入（故障会挪进生产）。正确出路：编译一个真实的
  `provsql-orders-v1` fixture，或让 harness 像 `snapshot-sidecar-install` 那样装
  publication。
- 顺带修：`internal/ordinal/dictionary.go:68` 遍历 map 报第一个非法摘要，导致同一
  失败随机报 "cold payload" 或 "hot index"——改成确定性顺序。
- 验收：带库 `go test -count=1 ./internal/gateway/...` 全绿，无 skip。

**P2.7 已 true 能力的 v3 复核（诚实性检查）**
- 现状记录 6/9 为 true，其中 ProvSQL 的绿色结果可能取自 I1-B 之前那条**已不存在**
  的 v1.4 路径。
- 步骤：对每一项 true 能力，查出它的证据是哪次运行、在哪个 commit、走的是哪条
  accounting 路径。凡是 I1-B 之前的，标记为"需在 v3 下重测"并纳入 P5。
- 产出：`docs/` 下一张能力—证据溯源表。这张表本身就是论文 §Three-Layer Assurance
  的材料。

---

### P3 — 形式化、N4、活体 canary（前置：P1a + P2 全绿）

**P3.1 formal build**：`make formal` / `Dockerfile.formal`，从集成后的 commit 构建。
**P3.2 N4 重认证**：树变了，Attestation footprint 必须在新树上重跑
（`qualify-attestation-footprint.sh`），并与 i2b 的可移植摘要对照。
记住 N1 的结论：footprint 是**按 schema 限定的 per-entry 多重性**，
一个 ExpectedSchema 的认证对另一个无效。
**P3.3 Result-heavy 100×4 v3 canary**（活体）：这是 v3 路径第一次真跑量。
失败即 `fail`，不降级。

验收：三项各自的证据文件入库（`raw/` 是 `.gitignore *`，值得留的用 `git add -f`，
入库前必须查凭据）。

---

### P4 — 契约 v1.5 冻结（前置：P3；需作者批准）

1. **删除** `config/profiles/activation-support-v1.json` 并重新生成 registry。
   release 一 bump，所有既有 activation smoke 全部作废——这是设计好的路径，
   不是逃生口。**绝不给旧证据重打新 release 标签。**
2. 同步 `evaluation/finalv5contracts/verifier.go` 的 `contractReleaseVxx` 常量与
   `chain` map、`bridge.go` 的 accept-list、`bridge_test.go` 的断言。
3. 重跑而非手改带摘要的记录：`sql-executability-v1.json` 的
   `contract_index_sha256` 用 `run-sql-executability-gate.sh` /
   `go run ./evaluation/cmd/final-v5-contract-sql-check` 重新导出
   （需要一个以 *template* 库命名 DSN 的一次性 PG16，初始化脚本要**去掉**
   `20-final-v5-benchmark-dataset.sh`）。
4. 重跑 7 个 live-route profile 的激活 smoke（v1.5 下）。
5. `validate.sh` 必须**零 SKIP** 退出 0。
6. 契约文本只能声明实际覆盖的关系片段（依 P1a 裁决措辞）。
7. tag 由作者打，Codex 只准备好并报告。

---

### P5 — 正式实测（前置：P4；含操作员输入）

| 任务 | 内容 | 阻塞输入 |
|---|---|---|
| **P5.1** | Artifact 六格 `result-heavy` 定向跑：`run-artifact-targeted.sh`，需 `ATTESTATION_QUALIFICATION`、`POSTGRESQL_IDENTITY`、`TASKGATE_DATASET_BINDINGS`、`RUN_ID`（四者无默认值，故意的）。HEAD 必须干净且已推到同名 origin 分支 | **私有六格 Dataset Binding 文件（必须作者提供）** |
| **P5.2** | Scale：`dependency-e2e` + `outcome-merkle` 全格，不得缩格或改标签 | 数据集绑定 |
| **P5.3** | Baseline：S1–S6，真实不可变数据集 + 真实 Task/OA 执行 + 独立 Oracle | 数据集绑定 + P1a 完成 |
| **P5.4** | 正式九实验 campaign：`run-deployment.sh`（服务 MASTER catalog），`campaign-finalize` 出 Campaign ID | 作者批准 Campaign ID |
| **P5.5** | 目标状态：9/9、`artifactRealSystemValidated=true`、相关 profile `targeted_validation_passed=true`、移除 `.NOT_READY` | 作者确认 |

失败处置：任何一格失败 ⇒ 保留失败目录作为证据（现在 `raw/` 下已有三个这样的目录
是正确做法），分析根因，**不得**换更小规模的数据集顶替一个失败的请求规模。

---

### P6 — 证据合并与论文数字（前置：P5）

**P6.1** PG 支撑的活动重跑：`./evaluation/run-exposure.sh`、
`run-exposure-performance.sh`、`run-exposure-scale.sh`、RQ5 的 daily-publication。
注意 TDSC 那轮修的性能管线要求每个 worker×query 有 ≥30 样本，`generate.py`
的矩阵校验会 fail-closed。
**P6.2** `evaluation/cmd/merge-evidence` 把进程内证据拼进 PG 支撑的
`evaluation/exposure/results.json`（保住 rq2 SQL / rq3 integration / rq4 perf）。
**P6.3** `python3 paper/tkde/generate_evidence.py --evidence-mode final`，
再 `make paper-final-check`（它要求**干净工作树** + `evidence.tex` 与 HEAD 无 diff）。
**P6.4** 安全/模糊测试证据：若 TKDE 稿引用，则 `make eval-full` +
24h fuzz 发表门槛（多日算力，需操作员排期）；若不引用，在台账里明确写"TKDE 不依赖"，
并把它留给 TDSC 稿那条独立线。

---

### P7 — 论文与投稿（前置：P6）

**P7.1 数字与表**：`tab:current-evidence` 的每个宏由实测填充；删除
"Publication-scale performance remains pending the final frozen WSL2 campaign"
这句——它在有 campaign 之后才可以删，之前不许删。
**P7.2 范围表述**：按 P1a/P1b 的实际结果改 §Exposure Model 的片段声明、
§Evaluation、§Limitations 的 Join/Union 措辞。宁可窄，不可宽。
**P7.3 可复现性边界**：写清 warm/single-host/TPC-H-derived、一次 index build、
一个部署；写清 Adapter 可以开多个窗口只交一个（这是机制外、由实验协议约束的
残余自由度，论文必须自曝）。
**P7.4 相关工作与新颖性**：守住的表述是"以人类授权的根任务为预算主体，为显式数据
释放定义可组合的 exposure effect，并在受限关系代数、在线执行与多 Agent 委托图上
维持任务级守恒"。**不得**声称首次按字段计费、首次同一信息只收一次、首次等价 SQL
不套利、首次非货币数据预算、首次 provenance+Gateway。
**P7.5 格式**：IEEE 双栏；摘要 100–200 词且**无公式无引用**；正文 12 页
（参考文献与作者简介计入）；supplement 单独。提交前在 ScholarOne/Author Portal
复核所选类别的页数口径与 supplement 规则。
**P7.6 作者逐字节审批**：更新 `docs/final_v5_author_review_manifest.md`——
`main.tex` / `supplement.tex` / `protocol-v1.yaml` / `workloads-v1.yaml` /
`config/catalog.yaml` / claim matrix 的 SHA-256 全部重算，作者逐项从
`NOT_CONFIRMED` 改为确认。**Codex 不得代作者确认。**
**P7.7 披露与合规**：cover letter 说明与 SessionBound（arXiv 未投稿工作稿）的
关系与实质差异；不得表述为已接受论文或独立外部工作；不得并投。
**P7.8 复现包**：查询变形集、Agent traces、原始结果、复现脚本；TKDE 鼓励
DataPort / Code Ocean（非强制，但对这篇是加分）。
**P7.9 提交**：IEEE Publishing Portal → TKDE → Regular Paper（DK-GenAI 专刊已于
2026-06-30 截止，走全年滚动 Regular Track）。默认单匿名。

---

### PX — 支线（随时可做，不阻塞）

- **PX.1** `result-object-store` / `-init` 还在用 MinIO `RELEASE.*` tag，改成 digest pin。
- **PX.2** `docs/tkde_revision_status.md` 已经落后于工作树（还在写 V8 receipt、
  6/9 等），随每个阶段同步更新；它自称是"当前投稿状态快照"，就必须真的是。
- **PX.3** 把 30 条 v3 集成门的完整需求文本补进 `docs/final_v5_v3_runtime_integration_gates.md`
  （历史上 18/19/21/22/25 曾缺失，现已补齐 —— 确认没有再退化）。
- **PX.4** README 按 `codex_taskgate_tkde_revision_plan.md` §Phase 10 的顺序整理。

---

## 6. 需要你（作者/操作员）提供的输入 — 阻塞清单

Codex 无法自行解决，请按需要的时间点提供：

| # | 输入 | 阻塞的任务 | 何时需要 |
|---|---|---|---|
| ~~1~~ | ~~P1a 三选一裁决~~ | — | **已裁决**：限定列重构（决策 16） |
| ~~2~~ | ~~是否投入在线 Join~~ | — | **已裁决**：要做（决策 17） |
| 3 | **私有六格 `TASKGATE_DATASET_BINDINGS` 文件** | P5.1/P5.2/P5.3 | P4 完成时 |
| 4 | v1.5 冻结批准 + tag 打点 | P4 | P3 全绿后 |
| 5 | Campaign ID 批准 | P5.4 | P5 中 |
| 6 | 24h fuzz 算力排期 / 或确认 TKDE 不依赖 | P6.4 | P6 前 |
| 7 | `final_v5_author_review_manifest.md` 逐字节确认 | P7.6 | 投稿前 |

---

## 7. 降级预案（B 计划）

如果 P5 的完整九实验 campaign 在可接受时间内拿不下来（最可能的原因：算力、
数据集绑定、或某一格反复失败）：

1. **不要**为了凑数缩小规模再按原规模叙述。
2. 走"诚实收缩"路线：把论文的定量主张收缩到实际跑完的能力集合，
   在 §Evaluation 显式列出未完成的能力与原因，并把 Reproducibility Boundary 写实。
3. 收缩的代价是 RQ4 的规模主张变弱——此时必须靠 RQ1/RQ2/RQ3（oracle 一致性、
   改写不变性、守恒性）扛住新颖性论证，这三项目前是仓库里最扎实的部分。
4. 收缩决策是作者决策，Codex 只准备两套数字与两套措辞，不自行选择。

---

## 8. Codex 会话协议

**每次开工**
1. `git status --short && git log --oneline -5`，确认在 `tkde-artifact-rerun`，
   工作树干净，与 origin 的关系明确。
2. `docker version` 实测（不要假设）。
3. 读本文件 §10 台账最后三行，接着做，不要重开已完成任务。

**每次收工**
1. 一个任务一次提交，提交信息首行 `<type>(<scope>): <祈使句>`，正文说明
   **改了什么行为**、**为什么**、**验证了什么**，末行注明任务 ID。
2. 更新 §10 台账：状态、证据（命令 + 关键输出摘要）、遗留。
3. 完成一轮验证后推分支：`git push -u origin tkde-artifact-rerun`。**永不推 tag。**

**报告纪律**
- 测试失败就贴失败输出；跳过的步骤明说跳过；只有真正跑过并通过才写"通过"。
- 遇到与本文件冲突的既有文档，以**证据**为准，并在台账记录冲突，不要静默改文档。
- 发现红线级问题（例如某处证据无法追溯到真实运行）立即停下并上报，不要绕过。

---

## 9. 常用命令速查

```bash
export GOFLAGS=-buildvcs=false

# 带库测试环境（唯一支持的方式）
./scripts/db-test-env.sh up
eval "$(./scripts/db-test-env.sh env)"
./scripts/db-test-env.sh verify

# 全量门禁
gofmt -l $(git ls-files '*.go'); go build ./...; go vet ./...; go test -count=1 ./...

# Final-V5 离线校验链
./evaluation/final-v5-wsl2/scripts/validate.sh
go run ./evaluation/cmd/final-v5-profile -verify
go run ./evaluation/cmd/final-v5-activation-support -verify
go run ./evaluation/cmd/final-v5-contract-sql-check      # 需 SQLCHECK_ADMIN_DSN

# 活体
./evaluation/final-v5-wsl2/scripts/run-artifact-targeted.sh   # Artifact 定向
./evaluation/final-v5-wsl2/scripts/run-deployment.sh          # 九实验 campaign
make eval-v5-final-campaign-finalize CAMPAIGN_ROOT=...

# 论文
make paper-evidence          # generate_evidence.py
make paper-tkde              # 容器内 LaTeX 构建
make paper-final-check       # 干净树 + final 模式 + evidence.tex 无 diff
```

注意：宿主机缺 `texlive-publishers`（`IEEEtran.bst`），论文只能用容器构建。

---

## 10. 进度台账

> Codex 追加行，不改历史行。状态取值：`TODO` / `DOING` / `DONE` / `BLOCKED` / `DROPPED`。

| 任务 | 状态 | 日期 | 证据 / 备注 |
|---|---|---|---|
| P0.1 环境自检 | TODO | | |
| P0.2 带库全量回归 | TODO | | |
| P0.3 门禁基线 | TODO | | |
| P1a.0 S5 裁决 | DONE | 2026-08-08 | 限定列重构；作者决策 16 |
| P1a.1 限定列重构实施 | TODO | | 关键路径起点 |
| P1a.2 收口测试 | TODO | | 三处测试需按决策 16 反转/删除/升级 |
| P1b.0 行数不变式显式化 | TODO | | 必须先于 P1b.1 |
| P1b.1 双源 provenance | TODO | | 作者决策 17；前置 P1a.1 |
| P1b.2 settlement 无回归验证 | TODO | | 触及 `Derive` 即需全套 exposure |
| P2.1 observer V2 化 | TODO | | |
| P2.2 Scale 切换 | TODO | | |
| P2.3 ProvSQL 切换 | TODO | | |
| P2.4 清空 ratchet | TODO | | |
| P2.5 翻转守卫 | TODO | | |
| P2.6 修 5 个带库测试 | TODO | | |
| P2.7 能力证据溯源 | TODO | | |
| P3.1 formal build | TODO | | 前置未满足 |
| P3.2 N4 重认证 | TODO | | 前置未满足 |
| P3.3 100×4 canary | TODO | | 前置未满足 |
| P4 v1.5 冻结 | TODO | | 需作者批准 |
| P5.1 Artifact 六格 | TODO | | 需私有 dataset binding |
| P5.2 Scale 全格 | TODO | | |
| P5.3 Baseline S1–S6 | TODO | | |
| P5.4 九实验 campaign | TODO | | 需 Campaign ID |
| P5.5 9/9 状态翻转 | TODO | | |
| P6.1–P6.3 证据与数字 | TODO | | |
| P6.4 安全/fuzz | TODO | | 先确认 TKDE 是否依赖 |
| P7.1–P7.9 论文与投稿 | TODO | | |
| PX.1–PX.4 支线 | TODO | | |
| P0.1 环境自检 | DONE | 2026-08-09 | `export GOFLAGS=-buildvcs=false`; `docker version`: Client/Server 29.1.3, Server Docker Desktop 4.55.0; `go version`: `go1.25.12 linux/amd64`; `./scripts/db-test-env.sh up` 达到 `Running 12/12`; `eval "$(./scripts/db-test-env.sh env)"`; `./scripts/db-test-env.sh verify`: control/business `server_version_num=160014  OK`, `taskgate_ordinal relations=31  OK` |
| P0.2 带库全量回归 | DONE | 2026-08-09 | 基线采集完成，**原始全量未通过**：在已 `eval "$(./scripts/db-test-env.sh env)"` 的同一 shell 中，`gofmt -l $(git ls-files '*.go')` 非空：`internal/control/execution_binding.go`（起始 HEAD 已存在，引入于 `9f59b0cf`）；`go build ./...` 与 `go vet ./...` 均 exit 0；`go test -count=1 ./...` exit 1：`TestBenchmarkProbeRenameIsSemanticsPreserving` / `TestReservedKeywordCTEIsRejectedByPostgreSQL` 均报 `schema "final_v5_benchmark" already exists`，`internal/gateway` 在默认 10m 超时，当时正运行 `TestLiveV10PreStateChangedBetweenPreparationAndReservation` 的 `snapshotbundle.Compile` / `VerifyColdDictionary`。原因已查清：`db-test-env.sh env` 导出的 SQL-check DSN 指向已含 benchmark schema 的模板库，而脚本 `test` 分支明确不导出它；同分支也明确为真库 gateway 套件加 60m timeout。诊断复跑 `go test -timeout=60m -count=1 ./internal/gateway/...` exit 0：`ok taskbound.local/agent-data-gateway/internal/gateway 2442.799s`。计划 §3/P2.6 所称当前 5 个 publication 失败与当前 HEAD 冲突：祖先提交 `81bab97` 已修复该 5 项，本次 gateway 全包复跑也无失败。遗留：gofmt 非空与 P0.2 配方的 SQL-check DSN/default-timeout 冲突；未伪装为通过。 |
