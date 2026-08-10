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
- 31 条 v3 集成门全部 PASS；分类器 manifest v2（路径感知闭世界）已定案。
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
- ~~5 个 `internal/gateway` 测试在接真库时失败~~ —— **已被 P0.2 推翻**：祖先提交
  `81bab97` 已修复该 5 项，带库 gateway 全包复跑无失败（需 `-timeout=60m`，
  默认 10m 会超时）。P2.6 相应作废，见台账 P0.2 行。
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
                                     P3 形式化 + N4 + Artifact-targeted 100×4 canary
                                          │
                                          ▼
                                     P4.0 publication oracle closure
                                          │
                                          ├─► 自动生成、作者审阅完整 12/6/105 binding
                                          ▼
                                     P4 契约 v1.5 冻结  ◄── 作者批准
                                          │
                                          ▼
                                     P5 正式实测 → 9/9
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

**P1.9 闭合 finalizer 重放身份（P2.2 的新前置）**
- 前置：P0；与已 `DROPPED` 的 P2.1 无依赖。
- 目标：保留 `ReproduceExecutionV3` 产出的 visible/companion
  `StatementIdentity`，并在 `paired_novel`、`single_query` 和
  `semantic_replay` 上闭合 signed↔reproduced 的 exact SQL、strict AST、
  row limit、policy fingerprint 与 companion presence。比较由签名实际存在的
  target 驱动，不按路径执行计数跳过 semantic replay 的已授权未执行 target。
  本任务只补 S–R 边；不扩展 Adapter carried evidence，不改生产推导，不碰
  P2.2/P2.3 cutover。
- 验收：`go build ./...` 与 `go test ./evaluation/internal/experiment/...` 通过。
  临时删除 S–R 调用点时，Gate 31 的八个独占 mutation 格全部失败，而 gate 1–30
  保持通过；恢复后 Gate 31 通过。Gate 31 八格为 paired visible/companion 的
  policy fingerprint，以及 semantic replay visible/companion 各自的 exact、
  strict、policy fingerprint。row-limit receipt mutation 由既有 binding、pre-state
  与 authorizer limit 不变量提前拒绝，不冒充 Gate 31；生产 comparator 的 visible /
  companion × exact / strict / row limit / policy fingerprint 分支由直接单元测试覆盖，
  并以独立 no-op mutation 证实。v1.4 ratchet 条目不得增删。

**P2.2 Scale 切换（`executeDependencyE2E`，仅 TaskGate 臂）**
- 前置：P1.9
- 步骤：调用 `RuntimeFinalizerV3.OpenObserverWindowV3` → 执行 → 提交
  `CarriedEvidenceV3` + `FrozenContractSelectorV3`；删除该路径的
  `NewGatewayControlPlan` / `applyObserverDelta`；
  该 cutover 需要的 resolver 真实实现落在 `evaluation/internal/experiment`。
  本任务只落 dormant、fail-closed 的 wiring。`exposure-scale`
  （`config/profiles/registry.json`）保持当前不可路由状态，不宣称 live pass，
  不翻任何 capability，不改 registry。
- 验收：`retiredV14ActiveReferences` 中仅 `scale.go` 一项可删；
  `finalize_scale_artifact.go` 一项保留至 P2.3；v14 ratchet 测试绿。
  原因：`finalize_scale_artifact.go:554` 的 `validateObserverTransition`
  仍被 `finalize.go:973` 的 ProvSQL 分支调用，该 allowance 在 P2.2 阶段
  无法诚实删除；不得通过移动文件位置来满足 grep 验收。

**P2.3 ProvSQL 切换（`executeProvSQLTaskGate`，仅 `taskgate` 臂）**
- 同 P2.2 形式。`direct` 臂是裸 PostgreSQL，不进 Gateway，不动。
- 验收：`provsql.go` 从 ratchet 移除；同时删除
  `finalize_scale_artifact.go` 的遗留 allowance 与共用 helper。

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

**P2.6a 封闭 grouped-join 展示 profile（旧 P2.6 已 DROPPED，不复用）**
- 前置：P2.5；这是 P2.3 台账追加的 shared production lowering / pre-registration
  硬前置。规则只按 SQL 形状定义，不得按 experiment/workload ID 开例外：仅 connected
  INNER equijoin 的 grouped result 可按完整、直接投影的 `GROUP BY` key 排序，每个 key
  恰好一次且方向仅 `ASC`/`DESC`；visible delivery order 与 ordinal provenance companion
  的 canonical order 相互独立；joined `LIMIT/OFFSET` 仍关闭式拒绝。
- projection cast 不是通用表达式开口：可证明的 `bigint`/`int8`、`numeric`、`text`
  identity cast 在 canonical plan 中擦除；唯一非 identity presentation 是上述完整排序
  grouped join 中 PostgreSQL 自然结果为 exact `numeric` 的 `SUM(...)::text`，以
  `postgresql-numeric-text-v1` 同时绑定 QueryPlan、semantic identity 与 ordinal
  program，而记账自然类型仍为 `numeric`。ungrouped/Union encoding、其它 cast、
  aggregate/partial/unselected/positional/NULLS/USING order 一律拒绝。
- 决策 10 不重开：一把 normalizer 与 S2/S4/S5 canonical typed-row lexical order 原样
  封存，只修权威 prose 中“这些冻结查询不依赖总结果顺序”的理由。S2/S4/S5 query、
  baseline binding 和已发布 normalizer contract 字节零 diff，并由历史 SHA 正向守卫。
- 冻结 ProvSQL SQL、public/private matrix 与 105 个 nonce variant 不改；每个 variant 必须
  经唯一 `sqllowering.Lower` → `queryplan` → `physicalquery.Prepare` 生产链成为 executable
  candidate。digest-pinned PostgreSQL 16.14 至少真实执行一个 exact visible/companion pair，
  校验三行 oracle、顺序及 `bigint/text/bigint/bigint` schema；synthetic binding 只能作为
  source/preparation slice，不得表述为 targeted/live/deployment/publication pass。
- **编译器 identity 移动必须单列上报：** `CompilerVersion` 从 v6 升为 v7，并记录旧/新
  SHA；保留 ordinal program v1。Result encoding 不带完整 relational `OrderBy` 时必须拒绝；
  强 gate 必须证明无 `OrderBy` 的 single、join、union、grouped 计划，除 compiler identity
  外的 SQL、semantic、ordinal、plan 与 preparation-plan 字节均与 v6 一致。当前
  release/activation 不因此继承支持；v7 与 contract release bump 的重认证统一排入 P4。
- 本任务不得改 registry、capability、activation support、frozen contract/evidence 或 tag，
  不得运行 formal/N4/100x4/campaign/freeze；完成本项仍不得进入 P2.7，须先完成 P2.6b。

**P2.6b 按 P1b.2 完整重跑 exposure**
- 前置：P2.6a 已作为单独 commit 推送，工作树 clean。必须从该准确 HEAD 执行
  digest-pinned PostgreSQL 16.14 的完整 `make eval-exposure`，由真实 runner 的事务发布
  路径刷新且仅刷新其合法 evidence；失败不得污染 canonical evidence，也不得复制、回填
  或沿用旧 run 字节。
- 验收沿用 P1b.2 全标准：build-stage、RQ1/RQ2、exposure rewrite、RQ3 的五个 named live
  tests/两个 package terminal PASS、RQ4 三曲线、raw→artifact→results 两级 SHA、credential
  scan 与容器/网络清理全部实际通过；任何 SKIP/未运行不算通过。P2.7 前置改为 P2.6b。

**P2.7 已 true 能力的 v3 复核（诚实性检查）**
- 前置：P2.6b。
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
**P3.3-prep 100×4-only runner 准备**：只准备可恢复、可审计的运行入口，不产生
canary evidence。`run-artifact-targeted.sh` 接受显式 cell 白名单；白名单只能是经严格
校验的冻结六格子集（可为完整集合），未设置时仍默认六格，且只改变本轮请求格和
`expected`。终局 `.taskgate_acceptance_v3 != null` 断言不得放宽。
在 readiness 之后、第一格 measurement 之前，runner 必须导出本轮 Compose project 并
fail-closed 真跑 `TestFormalDeploymentRunsTheApprovedHealthcheckLive`、
`TestPeriodicLivenessProbesAddNoBusinessStatements`、
`TestExplicitReadinessOutsideTheWindowStillAttests`；任一 FAIL、SKIP、缺失或重复 terminal
都不得开跑，10 秒 liveness 检查本身位于所有 measurement window 之外。

**P3.3-prep2 Artifact-targeted deployment binding**：为非发表 Artifact canary 新增
credential-free 的 `taskgate-final-v5-artifact-targeted-deployment-binding-v1`。生成器必须
从 embedded `finalv5contracts.LoadRuntime` 加载并重验 Contract Index，要求 Artifact 恰为
冻结六格，并逐格通过既有 Bridge 的 `ArtifactCell`、`QueryContract` 与 `OracleManifest`
核对 SQL SHA、NxC、oracle manifest SHA 和 canonical result SHA。它还必须绑定当前
Result-heavy Profile Registry 的 profile/closure/Catalog/Publication-set identity、
activation/targeted-run clearance、所选冻结 scale 子集、fresh Business PostgreSQL 的
pre-measurement dataset probe SHA，以及 Attestation qualification 与 PostgreSQL identity
文件的 SHA-256。输出必须 create-exclusive、普通非 symlink、mode `0600`、拒绝 unknown
fields，且不得含 credential、DSN、token、原始 SQL 结果或对象密钥。

Artifact selector 在 `TASKGATE_DATASET_BINDINGS`、
`TASKGATE_FINAL_V5_BINDING_FILE_SHA256`、
`TASKGATE_FINAL_V5_BINDING_SECTION_SHA256` 均未设置时仍须解析 Artifact candidate；只有
可能命中 Scale 或 ProvSQL 的 selector 才能加载完整 private material，并在缺失时
fail closed。runner 不得读取完整 publication binding、调用 Adapter `--validate-binding`
或设置上述三个变量。fresh Business PostgreSQL ready 后、任何 measurement 前生成 targeted
binding，把其 exact file SHA-256 设为 `TASKGATE_FINAL_V5_DATASET_BINDING_SHA256` 并交给
`final-v5-profile-binding`。`TARGETED-NOT-FOR-PUBLICATION` marker 必须记录 targeted binding
路径/摘要、`claim_scope=artifact_path_and_v3_observer_acceptance_only` 和
`publication_factset_oracle_ready=false`。P3.3-prep 的三项 formal-window live gate、严格
`SCALES` 白名单、动态 `expected` 及逐 sample 终局断言全部保持原样。

**P3.3 Result-heavy 100×4 v3 canary**（活体）：从完成 P3.3-prep2 后 clean、已推送的
新 HEAD 重建 formal Gateway image，显式选择 `SCALES=100x4`。它不依赖、也不得读取完整
12/6/105 publication binding；只使用 fresh deployment 现场生成的 Artifact-targeted binding。
Attestation 输入必须显式使用
P3.2 的 `diagnosis-attestation-footprint-qualification-p32-01-20260809T130103Z-f5fc9811eef8`
目录内 `attestation-footprint-v2.json` 与同目录 `postgresql-identity.json`，不得跨轮拼接。
这是 v3 路径第一次真跑量；失败即 `fail`，不降级。通过只能声明 Artifact path 与 V3
observer acceptance，不得声明 independent FactSet oracle closure、publication readiness
或任何 capability/registry 状态翻转；每个 sample 仍必须是
`publication_eligible=false`。

验收：P3.1、P3.2、P3.3 三项各自的证据文件入库（`raw/` 是 `.gitignore *`，值得留的
用 `git add -f`，入库前必须查凭据）；P3.3-prep/P3.3-prep2 不产生 canary evidence。

---

### P4 — Publication oracle closure、完整 binding 与契约 v1.5 冻结（前置：P3；需作者批准）

**P4.0 publication-wide oracle closure and binding generation**：P3.3 不依赖本任务；
它是任何 P5 publication measurement 的硬前置，且必须按顺序完成：

1. 裁决 Scale contract 的 `existing=N` 与当前 validator 的 `history=K` /
   zero-overlap 无 history 冲突，不得在未裁决时任选一边生成摘要。
2. 建立独立 Scale FactSet oracle；expected rows/FactSets 不得复制生产输出。
3. 建立 ProvSQL 105 格 independent FactSet oracle。
4. 裁决 Artifact private dependency section 是删除，还是实现真正 independent oracle；
   P3.3 targeted binding 不替代该裁决。
5. 上述 closure 完成后，由程序从已闭合的 contract/oracle material 自动生成完整
   12/6/105 publication binding `REVIEW_CANDIDATE`，再由作者逐字节审阅。该文件不是让
   作者手工填写 123 个 SHA 的普通输入；不得用 placeholder、测试摘要或运行后反填生成。

完整 publication binding 的最早需求时间是 P4.0 oracle closure 完成后、任何 P5 正式
实测之前。

**P4 契约 v1.5 冻结**：

1. **删除** `config/profiles/activation-support-v1.json` 并重新生成 registry。
   release 一 bump，所有既有 activation smoke 全部作废——这是设计好的路径，
   不是逃生口。Compiler v7 的 identity 移动与本次 contract release bump 一并重认证；
   activation support 不跨 release 继承，7 个 live-route profile 必须全部重跑，并在新
   evidence 中记录 v7 version/SHA。**绝不给旧证据重打新 release 标签。**
2. 同步 `evaluation/finalv5contracts/verifier.go` 的 `contractReleaseVxx` 常量与
   `chain` map、`bridge.go` 的 accept-list、`bridge_test.go` 的断言。
3. 重跑而非手改带摘要的记录：`sql-executability-v1.json` 的
   `contract_index_sha256` 用 `run-sql-executability-gate.sh` /
   `go run ./evaluation/cmd/final-v5-contract-sql-check` 重新导出
   （需要一个以 *template* 库命名 DSN 的一次性 PG16，初始化脚本要**去掉**
   `20-final-v5-benchmark-dataset.sh`）。
4. 重跑 7 个 live-route profile 的激活 smoke（v1.5 下）。
5. `validate.sh` 必须**零 SKIP** 退出 0。
6. 契约文本只能声明实际覆盖的关系片段（依 P1a 裁决措辞）；v1.5 生成时把冻结
   baseline metadata 中决策 10 的旧 grammar 理由同步为“查询不依赖总结果顺序”，但
   S2/S4/S5 query 与 normalizer 字节仍不得改变。v1.4 contract/evidence 不原地改写。
7. tag 由作者打，Codex 只准备好并报告。

---

### P5 — 正式实测（前置：P4；P5.1 为非发表前置）

| 任务 | 内容 | 阻塞输入 |
|---|---|---|
| **P5.1** | Artifact 六格 `result-heavy` 非发表定向验收：`SCALES` 未设置，`run-artifact-targeted.sh` 按默认完整六格执行；需 `ATTESTATION_QUALIFICATION`、`POSTGRESQL_IDENTITY`、`RUN_ID`，HEAD 必须干净且已推到同名 origin 分支。runner 现场生成 Artifact-targeted binding；所有 sample 仍为 `publication_eligible=false`，不能作为 publication evidence | 同轮 Attestation/PostgreSQL identity 配对 + RUN_ID；**不需要完整 publication binding** |
| **P5.2** | Scale：`dependency-e2e` + `outcome-merkle` 全格，不得缩格或改标签 | P4.0 自动生成且作者审阅的完整 12/6/105 publication binding |
| **P5.3** | Baseline：S1–S6，真实不可变数据集 + 真实 Task/OA 执行 + 独立 Oracle | 完整 publication binding + P1a 完成 |
| **P5.4** | 正式九实验 campaign：`run-deployment.sh`（服务 MASTER catalog），其中包含 Artifact 的正式 publication samples；`campaign-finalize` 出 Campaign ID | 完整 publication binding + 作者批准 Campaign ID |
| **P5.5** | 目标状态：9/9、`artifactRealSystemValidated=true`、相关 profile `targeted_validation_passed=true`、移除 `.NOT_READY` | 作者确认 |

`run-deployment.sh` 对完整 12/6/105 publication binding 的校验不得放宽。该 binding 只在
P4.0 oracle closure 后自动生成并提交作者审阅，不是 P3.3/P5.1 runner 从操作者读取的手填
123-SHA 文件。为保持既有任务编号，P5.1 留在本节，但它只是非发表 targeted acceptance
前置，不属于 P5 publication measurement，也不能满足任何 P5 正式证据要求；Artifact 的
正式 publication 样本只来自 P5.4 campaign。

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
- **PX.3** 把 31 条 v3 集成门的完整需求文本补进 `docs/final_v5_v3_runtime_integration_gates.md`
  （历史上 18/19/21/22/25 曾缺失，现已补齐 —— 确认没有再退化）。
- **PX.4** README 按 `codex_taskgate_tkde_revision_plan.md` §Phase 10 的顺序整理。

**PX.5 修复 skip 审计器的陈旧 allowlist（优先做——它让门禁 exit 1，挡住下游一切）**

- **裁决（2026-08-09，作者）：这不是红线违规，不是绕过，也不是范围蔓延。**
  `evaluation/cmd/final-v5-dbtest-report` 的 allowlist 是**设计成可扩展的**：每条
  allowance 必须带 `Category`、`ReasonSubstring`、`Why`、`Scope` 和 `DeferredUntil`。
  构成绕过的是**无期限、无理由**的条目；带到期里程碑的条目是"被排期的债"，
  正是该审计器要的东西。红线 9 的措辞是"**冻结时**"skip 不记为通过——现在是 P1a，
  不是 P4。
- **根因**：`017e73a`（记录 v1.4 全新活体激活）写入了
  `config/profiles/activation-support-v1.json`，**翻转了这一对测试能跑的那一半**，
  却没同步 allowlist。于是：
  - `TestRegistryClaimsNoSupportWithoutAManifest` 现在 skip 且**无 allowance** ⇒ 审计 exit 1；
  - 反过来，四条兄弟 allowance（`TestCommittedManifestSupportsExactly…`、
    `TestCommittedRegistryMatchesTheManifest`、
    `TestResultHeavyCarriesTheCurrentReleaseActivationEvidence`、
    `TestCommittedManifestCarriesNoSecretsOrBusinessData`）现在**匹配不到任何 skip**，
    其 `Why` 写着"manifest 产生于本 HEAD 之后的 release"——这句话现在是**假的**，
    manifest 就产生于本 release。审计器抓不到这一半。
- **三部分修复，缺一不可**：
  1. **补 allowance**：给 `TestRegistryClaimsNoSupportWithoutAManifest` 加条目，
     `DeferredUntil: DeferredContractsV15Freeze`——P4 删除 manifest 后它必然运行且
     必须通过，这是真实到期日不是永久豁免。`SkipEvidenceNotYetProduced` 这个类别
     **词不达意**（它跳过恰恰因为证据*存在*），需要新类别，例如
     `SkipPremiseExcludedByState`：树当前不处于该测试断言的那个状态。
  2. **清理四条陈旧 allowance**：那四个测试现在真的在跑并通过，条目应删除而非改写。
  3. **补审计器自身的洞**：报告"匹配不到任何 skip 的 allowance"。目前只有
     `ReasonSubstring` 变化会被发现，allowance 整条空转不会。补上之后，
     未来任何一次状态翻转都会当场暴露，而不是留下一条静默豁免。
- **更强的做法（推荐一并做）**：该文件已有 `t.TempDir()` + `writeFixtureDocument`
  的 fixture 模式。用它加一个**不依赖仓库状态**的测试，直接证明机制
  （无 manifest ⇒ registry 不得声称任何 support），于是这条不变式**现在**就有覆盖，
  而仓库态那条测试的 skip 退化为一个有期限的义务。这把"覆盖空洞"变成"覆盖 + 排期"。
- **验收**：审计器 `accepted: true` 退出 0；把新旧两份报告 SHA-256 都写进台账。
- **单独提交**，不要混进 P1a.2。

---

## 6. 需要你（作者/操作员）提供的输入 — 阻塞清单

Codex 无法自行解决，请按需要的时间点提供：

| # | 输入 | 阻塞的任务 | 何时需要 |
|---|---|---|---|
| ~~1~~ | ~~P1a 三选一裁决~~ | — | **已裁决**：限定列重构（决策 16） |
| ~~2~~ | ~~是否投入在线 Join~~ | — | **已裁决**：要做（决策 17） |
| 3 | P4.0 两项作者裁决：Scale `existing=N` 与 `history=K`/zero-history；Artifact private dependency section 删除或真正 independent oracle | P4.0 | P3.3 后、相应 oracle 实施前 |
| 4 | v1.5 冻结批准 + tag 打点 | P4 | P3 全绿后 |
| 5 | 自动生成的完整 12/6/105 publication binding 逐字节审阅 | P5.2/P5.3/P5.4 | P4.0 oracle closure 与自动生成后、任何 P5 publication measurement 前 |
| 6 | Campaign ID 批准 | P5.4 | P5 中 |
| 7 | 24h fuzz 算力排期 / 或确认 TKDE 不依赖 | P6.4 | P6 前 |
| 8 | `final_v5_author_review_manifest.md` 逐字节确认 | P7.6 | 投稿前 |

---

## 7. 降级预案（B 计划）

如果 P5 的完整九实验 campaign 在可接受时间内拿不下来（最可能的原因：算力、
P4.0 oracle closure / 完整 binding 审阅、或某一格反复失败）：

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
./evaluation/final-v5-wsl2/scripts/run-artifact-targeted.sh   # Artifact 非发表定向
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
| PX.5 skip 审计器陈旧 allowlist | TODO | 2026-08-09 提出 | Codex 按红线停在此处，**裁决为非违规**；根因是 `017e73a` 翻转状态未同步 allowlist。三部分修复见 §5 PX.5，单独提交 |
| P2.6 修 5 个带库测试 | DROPPED | 2026-08-09 | 前提不成立：`81bab97` 已修复，P0.2 复跑无失败 |
| P0.1 环境自检 | DONE | 2026-08-09 | `export GOFLAGS=-buildvcs=false`; `docker version`: Client/Server 29.1.3, Server Docker Desktop 4.55.0; `go version`: `go1.25.12 linux/amd64`; `./scripts/db-test-env.sh up` 达到 `Running 12/12`; `eval "$(./scripts/db-test-env.sh env)"`; `./scripts/db-test-env.sh verify`: control/business `server_version_num=160014  OK`, `taskgate_ordinal relations=31  OK` |
| P0.2 带库全量回归 | DONE | 2026-08-09 | 基线采集完成，**原始全量未通过**：在已 `eval "$(./scripts/db-test-env.sh env)"` 的同一 shell 中，`gofmt -l $(git ls-files '*.go')` 非空：`internal/control/execution_binding.go`（起始 HEAD 已存在，引入于 `9f59b0cf`）；`go build ./...` 与 `go vet ./...` 均 exit 0；`go test -count=1 ./...` exit 1：`TestBenchmarkProbeRenameIsSemanticsPreserving` / `TestReservedKeywordCTEIsRejectedByPostgreSQL` 均报 `schema "final_v5_benchmark" already exists`，`internal/gateway` 在默认 10m 超时，当时正运行 `TestLiveV10PreStateChangedBetweenPreparationAndReservation` 的 `snapshotbundle.Compile` / `VerifyColdDictionary`。原因已查清：`db-test-env.sh env` 导出的 SQL-check DSN 指向已含 benchmark schema 的模板库，而脚本 `test` 分支明确不导出它；同分支也明确为真库 gateway 套件加 60m timeout。诊断复跑 `go test -timeout=60m -count=1 ./internal/gateway/...` exit 0：`ok taskbound.local/agent-data-gateway/internal/gateway 2442.799s`。计划 §3/P2.6 所称当前 5 个 publication 失败与当前 HEAD 冲突：祖先提交 `81bab97` 已修复该 5 项，本次 gateway 全包复跑也无失败。遗留：gofmt 非空与 P0.2 配方的 SQL-check DSN/default-timeout 冲突；未伪装为通过。 |
| P0.3 门禁基线 | DONE | 2026-08-09 | 干净 shell（`GOFLAGS=-buildvcs=false`，未设 SQL-check DSN）中 `./evaluation/final-v5-wsl2/scripts/validate.sh` exit 0：protocol/config/schema、profile regeneration、11-profile activation consistency 和 Pilot harness 均报 `pass`；**`contract SQL executability: SKIPPED (TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN is not set)`，明确不记为通过**。`go test ./evaluation/internal/experiment/ -run 'V14|Cutover|ProductionCallers' -v` exit 0，5 个顶层测试全部实际 PASS、无 SKIP；`TestV3CutoverIsBlockedByTheUnwiredFinalizer` 存在且 PASS。`TestFinalizeObservationV3HasProductionCallers` 日志仍指出 Scale/ProvSQL 无 v3 production caller，因此 blocker PASS 表示切换未完成，不表示 cutover 完成。 |
| P1a.1 限定列重构实施 | DONE | 2026-08-09 | Predicate footprint 的产品绑定改为精确 `(role, product)` 复合键，并由 `CompileRelational(...).Sources` 经唯一 helper 供普通 relational、semantic View 与 compiler identity probe 共用；同一 product 的左右 UNION 分支保持独立 role atom，outer UNION 保持既有 product identity，缺失/错配 binding 继续 fail closed。既有单点 SQL lowering 保持在 `internal/queryplan`，UNION output/左右 branch role 统一经安全标识符及 63-byte 上限校验；未在 gateway 增加第二份推导。compiler identity 升至 v6，pin 为 `13fd7f3bf8c21209354d04b82c5006c5f29a5b0dd568b820fc2ef43a81f641ed`。最终差异上 `go test -count=1 ./internal/queryplan ./internal/physicalquery`、RLS oracle、真实 S5 render/compile、v3 cutover blocker 均 exit 0/PASS；`go build ./...`、`go vet ./...` exit 0，`go test -run '^$' ./internal/... ./evaluation/...` exit 0（仅编译，未记为测试通过）。全仓 `gofmt -l` 仍只报 P0.2 已有的 `internal/control/execution_binding.go`，本任务文件无输出；`git diff --check` 通过。首次 `./scripts/db-test-env.sh verify` exit 1，输出 `taskgate_ordinal relations=0  MISSING`；sidecar installer 完成后最终重跑 exit 0，control/business 均为 `server_version_num=160014  OK`、`taskgate_ordinal relations=31  OK`。最终差异上的两条真库旧负断言按新能力明确 FAIL（不是 SKIP）：`preparation_differential_test.go:242: the extraction prepared a branch-filtered V5 union`；`preparation_parity_test.go:897: a V5 union with branch-qualified predicates prepared ... (footprint atoms=2)`；它们是 P1a.2 待反转的陈旧断言，因此本任务不声称带库全量绿色。`validate.sh` 其余门禁报 pass，但 **contract SQL executability 为 `SKIPPED (TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN is not set)`，不记为通过**。未修改 P1a.2 parity/differential/coverage，未翻 capability。 |
| P1a.2 收口测试 | DONE | 2026-08-09 | 提交 `c667970`：V5 `relational_union` parity 恢复为带左右分支局部谓词的形状，正面测试精确要求 `left_branch`/`right_branch` 各一个 V5 predicate atom；该形状同时进入 production-vs-`Prepare` parity 与 reproducibility loops，故删除重复的 extraction refusal 负测试；按作者决策 16 删除整份已失效的 unsupported-dependency scanner，S5 的两分支 filtered template 仍由 `TestBenchmarkS5StructuredPlanRendersAndCompiles` 守卫。带库定向测试、13 个 named shapes、reproducibility 与 S5 render/compile 均实际 PASS；独立 PostgreSQL 16.14 SQL gate 为 28 artifacts / 71 rendered cells / 0 failed。第一次完整 DSN 运行本身为 95 packages、3321 tests、0 package/test failure，但 v1 skip 审计器因 `TestRegistryClaimsNoSupportWithoutAManifest` 的 undeclared SKIP exit 1 / `accepted:false`，未伪装为全绿；作者裁决该失败与 P1a.2 无关并归档 PX.5。未翻 capability。 |
| PX.5 skip 审计器陈旧 allowlist | DONE | 2026-08-09 | 根因 `017e73a` 写入 v1.4 activation-support manifest 后未同步互补测试 allowlist。三部分均完成：新增 `premise_excluded_by_state` 类别及 `DeferredContractsV15Freeze` 的 `TestRegistryClaimsNoSupportWithoutAManifest` allowance；删除四条现已实际运行并 PASS 的正向测试 allowance 及其失真 `Why`；报告 v2 新增 `unmatched_allowances` 反向闭合检查，任何 allowance 未匹配同一 `(package,test)` 且命中 `ReasonSubstring` 的 SKIP 都使 `accepted:false` / exit 1，输出稳定排序。另用 `t.TempDir()` fixture 经完整 `run(..., verify=true)` 路径证明无 manifest 时全 false registry 通过，而 `activation_supported`、`activation_smoke_passed`、`targeted_run_eligible`、`routable` 任一 claim 均失败关闭。聚焦两个包实际 PASS；仓库态负测试的 SKIP 明确保留为有期限义务，四条正向兄弟测试实际 PASS。`./scripts/db-test-env.sh verify` 再证 control/business `160014`、31 relations；独立 SQL gate 再证 28 artifacts / 71 cells / 0 failed。完整命令 `env -u TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN TASKGATE_DB_TEST_TIMEOUT=60m ./scripts/db-test-env.sh test -count=1 -json ./internal/... ./evaluation/... | ... final-v5-dbtest-report ...` exit 0：v2 报告 95 packages、3329 tests、0 failure、9 declared SKIP、0 undeclared SKIP、0 unmatched allowance、`accepted:true`；9 个 SKIP 不记为测试通过，其中 SQL-check 两项由同工作树独立 gate 补证，其余按报告里程碑保留义务。旧失败报告内嵌 raw JSONL `report_sha256=c00e07444db30d1ea8d39ada169b18c4073e7ab537b982d840d41f132c191804`；新 accepted 报告内嵌 raw JSONL `report_sha256=254e69d9c7f63d5a97102bff55c858841109eb8f40b7bb1ed29c5f2450407169`，新 summary 文件 SHA-256 `9fd82a15bb7aaaef69146e99b0dd785aded792c2ee589969efda0ef9a0d15420`（`/tmp/taskgate-px5-dbtest-report.GHV3Iz.json`）。`go build ./...`、`go vet ./...`、`git diff --check` exit 0；全仓 `gofmt -l` 仍只报 P0.2 已有的 `internal/control/execution_binding.go`。历史 v1 报告字节未改。 |
| P1b.0 行数不变式显式化 | DONE | 2026-08-09 | 新增 `TestDeriveObservationV2RequiresEqualUngroupedRowCounts`，直接钉住 single-product、non-relational、ungrouped V2 observation 的当前契约：2/2 的合法 visible/provenance 行集实际成功，2/1 与 1/2 均精确拒绝为 `visible and provenance row sets differ`；同时设置 `Result.RowCount` 与 `Rows` 一致，证明测的是实际行集不是元数据偶合。本任务只增测试，未改 `deriveObservationV2`、`physicalquery.Derive` 或 settlement，因此未触发 P1b.2 的完整 exposure 硬门。聚焦测试及相关 Join normal-form 纯单元集实际 PASS，无 SKIP；`go test -run '^$' ./internal/gateway` 仅编译通过（未记为测试通过），`go vet ./internal/gateway`、`gofmt -l internal/gateway/exposure_test.go`、`git diff --check` 均 exit 0/无输出。 |
| P1b.0 台账证据措辞校正 | DONE | 2026-08-09 | 上一行称 `RowCount` 与 `Rows` 一致能“证明”实现检查实际行集，该措辞过强：二者在 fixture 中始终同步，只能避免 fixture 歧义，不能区分实现读了哪一个。实现确实在 `deriveObservationV2` 中比较 `len(visible.Rows)` / `len(provenance.Rows)`；2/2 成功与 2/1、1/2 精确失败的测试结论不变。本行是 append-only 证据校正，不回写历史。 |
| P1b.1 双源 provenance | DONE | 2026-08-09 | 计划的“尚未实现”前提被仓库证据推翻：`e49d346` 在 2026-07-27 已上线 joined-provenance 的逐源去重基关系重建 → `JoinOnV2` → actual-origin-pair 收缩 → `ObserveV2`，并接入公开在线路径与双库 settlement；`git merge-base --is-ancestor e49d346 73da8ae` exit 0，证明作者决策 17/计划写入时该实现已在其祖先。当前生产链仍为 sealed `physicalquery.PreparedOperation` 恢复 relational compilation，`deriveObservation` 分派到专用 `deriveRelationalObservationV2`；不为迎合计划的函数名重写或复制实现。当前 HEAD 实际运行 `TestExposureV2OnlineJoinOperandSwapUsesSameLedgerFacts`、`TestRelationalOnlinePathAgainstPostgreSQL`、`TestRelationalGatewayEndToEndAgainstPostgreSQL` 及 P1b.0 测试，fake/Business PG/双库 settlement 四条全部 PASS、无 SKIP。本任务无 production 改动；P1b.2 的 current-HEAD 完整 `evaluation/run-exposure.sh` 仍需作为下一独立任务，本行不拿定向测试冒充。 |
| PX.6 exposure build-context 门禁可移植性 | DONE | 2026-08-09 | P1b.2 首次在认证 PostgreSQL 16.14 digest 上执行 `make eval-exposure` 时 exit 1，且在 Docker build-stage/进入 PostgreSQL 与 RQ3 前失败：容器 root 使 `chmod 0500` 无法注入删除失败，`.dockerignore` 排除 `.git` 使测试不能直接调 `git ls-files`，build image 缺 `jq` 使 publication Compose override 在冻结拓扑守卫之前静默退出。首次失败日志 `/tmp/p1b2-eval-exposure.log` SHA-256 `74cc8512f7d88b1966549cfe7a15c4af9f7e8cf181d2d08886ee59e44953b49a`；跟踪的 `results.json` / `rq3-integration.json` / raw RQ3 JSONL 仍分别为 `1eb54a902bc0b47499a2e9bd8cf72dc770bc2024e81df237b6f7162f38e70920` / `37c9a869260b3e2f16f101740d476f7b6d306fbafdebcc4d64b91aa6e877653e` / `72b0adc0e2ae3785974346b685b998f88ad1699e06f9a2ada094cf6e19188b61`，未被该失败运行刷新。修复不加 skip/绕过：用返回成功但不删除的假 `rm` 精确验证 cleanup argv 与“目标仍存在则失败”后置条件；保留 launcher 三次完整 `git ls-files` 静态守卫，并在实际 build context 检查每个 required source 为普通非链接文件、用合成 digest listing 逐项验证任一遗漏都 fail closed；将 publication 冻结 Compose 等值守卫移到 `jq`/Git/Docker 探测之前，minimal `PATH` 仍精确拒绝 override。三条聚焦测试实际 PASS，`docker build --file evaluation/Dockerfile --target build ...` exit 0，其中的 `gofmt` 守卫、`go test ./evaluation/...` 及 evaluation binaries build 全部完成。本任务不生成 exposure 证据；P1b.2 仍为 TODO，须从本提交的干净新 HEAD 完整重跑。 |
| PX.7 exposure oracle 认证生命周期 | DONE | 2026-08-09 | P1b.2 第二次完整命令在 Docker build-stage 通过后仍 exit 2，exposure runner 报 `lookup taskgate-exposure-oracle-1354296 ... no such host`；日志 `/tmp/p1b2-eval-exposure-92f1efc.log` SHA-256 `bc2bdec131459fa54523e1023bd4fcc2bd62240d0eb9bd8f90511efe9d8735f4`。Docker events 证明 oracle 先 exit 1、两个 consumer 后续 exit 1，trap 只在最后清理，非 cleanup 抢跑或容器名冲突。可丢弃容器的同配方复现捕到真正 init 错误：`db/init/20-final-v5-benchmark-dataset.sh` 读取 `/opt/taskgate/final-v5-sql/benchmark-v1-generate.sql` 时该路径未挂载。根因为 `5e12765` 新增 init 20 时同步修了根 Compose 的 dataset 只读挂载与 final-server TCP readiness，却漏改 `run-exposure.sh`；无 host 的 socket `pg_isready` 误把 entrypoint 临时 init server 当成 ready，DNS 失败只是 oracle 退出后的下游症状。修复将默认镜像锁定到认证 PG16.14 digest 并在任何 Docker 调用前拒绝其他 override，补齐与 Compose 同一 dataset `:ro` 挂载，用同一 Docker network 上的 DNS/TCP `pg_isready` 和 60×3s 窗口等待最终 server，并在 RQ2/RQ3 前精确要求 `server_version_num=160014`；oracle 早退立即失败并在删除前输出 state/logs。Hermetic fake-Docker 测试对可变 override、两个只读挂载、网络 probe/retry 顺序、版本错配、早退诊断/不触碰旧 evidence 全部 PASS；Docker build-stage 中的 `gofmt` 守卫、`go test ./evaluation/...` 与全部 evaluation binaries build 亦 exit 0；真实一次性 oracle 实测通过同网络 probe，返回 `160014` 且 `final_v5_benchmark` 有 2 张 base table，随后精确清理容器/网络。三份跟踪证据 SHA 仍为 `1eb54a902bc0b47499a2e9bd8cf72dc770bc2024e81df237b6f7162f38e70920` / `37c9a869260b3e2f16f101740d476f7b6d306fbafdebcc4d64b91aa6e877653e` / `72b0adc0e2ae3785974346b685b998f88ad1699e06f9a2ada094cf6e19188b61`，本次未刷新也未声称 RQ3 通过。另查明 integration 不完整时会在拒绝前覆盖 canonical evidence，归 PX.8 独立收口；P1b.2 仍为 TODO。 |
| PX.8 exposure evidence 发布事务 | DONE | 2026-08-09 | `11df2de` 引入的 runner 先把 integration 临时日志 `cp` 到 canonical raw，recorder 又在最终 completeness 判断前写 canonical artifact/results；因此 integration 非零、尾部畸形 JSONL 或后续验收失败会污染 last-complete 证据。修复只从新运行的临时输出生成候选：raw 单次 `read_bytes` 后由同一字节解析并写入同盘 staging，重放五个精确 named terminal event 与两个且仅两个 package 最终 PASS，绑定实际 Docker argv、canonical 路径及 raw→artifact→results 两级 SHA；schema v7、RQ1 21/21、PostgreSQL RQ2 1024/1024 且 0 mismatch、exposure rewrite complete/0、RQ3 deterministic 5/5、RQ4 complete/3 curves 全在首次 replace 前验证。目录 inode 独占锁串行化协作 publisher；拒绝重复 case/ID、leaf symlink、非普通或跨文件系统 destination。只有完整候选才按 raw→artifact→results 发布；文档明确每次 replace 单文件原子，但三固定路径不是 group/power-loss transaction，读者必须从 results 重验两级 SHA。Hermetic `t.TempDir()` 合成测试（不作为论文证据）证明完整 5/5/0 正例字节、五身份、实际 command 与两级 SHA 闭合；完整成功前缀后的坏 JSON、integration exit 1、缺/多 package pass、named test SKIP、RQ1 21/20、symlink destination 均在发布前非零退出且三份 sentinel 原字节不变、无 staging 残留。聚焦测试、host `go test -count=1 ./evaluation/exposure`、`go vet ./evaluation/exposure`、shell/Python syntax 与 `git diff --check` 均 exit 0；最终 Docker build-stage 的 `gofmt` 守卫、`go test ./evaluation/...` 和全部 evaluation binaries build exit 0。canonical `results.json` / `rq3-integration.json` / raw JSONL SHA 仍分别为 `1eb54a902bc0b47499a2e9bd8cf72dc770bc2024e81df237b6f7162f38e70920` / `37c9a869260b3e2f16f101740d476f7b6d306fbafdebcc4d64b91aa6e877653e` / `72b0adc0e2ae3785974346b685b998f88ad1699e06f9a2ada094cf6e19188b61`；本任务未重跑真实 RQ3、未声称 P1b.2 通过，P1b.2 仍为 TODO。 |
| P1b.2 settlement 无回归验证 | DONE | 2026-08-09 | 从已推送且 clean 的 `b55b830` 启动 `GOFLAGS=-buildvcs=false TASKGATE_EXPOSURE_POSTGRES_IMAGE=postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55 make eval-exposure`，完整命令 exit 0；日志 `/tmp/p1b2-eval-exposure-b55b830.log` SHA-256 `d9348f6f85b70306e0f74e9b31733c9bfbe6c49dc1ed434bcfcafd9a099cf4bc`。同次运行的 Docker build-stage `gofmt`、`go test ./evaluation/...` 与全部 evaluation binaries build 通过；认证 oracle 精确报告 PostgreSQL `16.14 (Debian 16.14-1.pgdg12+1)`。fresh schema-v7 evidence 为 RQ1 21/21；PostgreSQL RQ2 generated/unique/executed 1024/1024/1024、duplicate 0、1152 differential checks、1024 metamorphic checks、2176 statements、0 mismatch；exposure rewrite complete/0 mismatch；RQ3 deterministic 5/5，`-race` integration 的两个 control 与三个 gateway named tests 精确 5/5/0、两个 package terminal PASS、raw 中 0 fail/skip，其中 `TestRelationalOnlinePathAgainstPostgreSQL` 与 `TestRelationalGatewayEndToEndAgainstPostgreSQL` 均真实 PASS；in-process RQ4 三条 scaling curve complete。独立、不导入 recorder 的 raw 重放验证五身份、实际 command、时间窗 `2026-08-08T21:52:18.880676866Z`–`21:52:40.141477994Z`、canonical refs 与 results→artifact→raw 两级 SHA 均 PASS；论文侧 `validate_exposure()`、corpus SHA `8b87db47e51db7db42c759155c8581f95af93e766e7731a0174087ce110f2967`、RQ1 oracle SHA `4548b9ae4ce46e056a39ff6cffafc6cd615a7d34966786f03cc59e749b0e064b` 亦闭合。新 `results.json` / `rq3-integration.json` / raw JSONL SHA 分别为 `3b46d5dc4219295b8d9bf29a33c443b9cad875ec57ec8bdc783721a592a1df40` / `931ae57c421c1522510b195ee1713534c49b0b974b25660118b707d83b20a277` / `fa4cce99d2ac7f23803f144bdf4925f221e4197ce2d22370d581f2b515496608`，三者均离开旧基线；本次真实 runner 仅刷新这三份 evidence，credential scan 无命中，oracle 容器/网络无残留。**本任务没有运行 `run-exposure-performance.sh`，没有刷新三部署 31,296-sample 历史性能 campaign（7,896 full-path + 23,400 ablations）；`evaluation/exposure-performance/results.json` 保持 SHA `7c223c3520e5f059039af55435c4b29cd172145e79c44ac93da300a19a4191ad`，不得把本轮 in-process 三曲线表述为 current-HEAD latency/throughput campaign 重测。** |
| P1b.3 论文 Join 限界对账 | DONE | 2026-08-09 | 计划称论文仍把 Join 限于 algebra/oracle-only，但逐提交与逐行审计证明该前提在计划写入时已过时：`e49d346` 已删除四处“未接在线 provenance / 非 end-to-end online Join”旧限界并接入公开在线、Business PostgreSQL 与 Control PostgreSQL settlement，`ff293fdc` 统一 SQL→canonical plan，`9d746cfc` 扩展为 connected 2–16-role equijoin graph，`722c5a7e` 固化 online compiler→materialized-oracle refinement，`332d42ed` 汇总当前 Evaluation/Limitations；这些提交均为计划提交 `73da8ae` 的祖先。当前 `paper/tkde/main.tex` 明确陈述 operational multi-source SQL、2–16-source online lowering、paired PostgreSQL join fanout 及 AST/Catalog→JoinMany→SQL/effect fold，`oracle-only` / `algebra-only` 扫描零命中；“materialized algebra remains the oracle”处于在线实现的差分验证语境，不是执行替代或能力收缩。能力上限是 2–16 个角色，而当前保留的真实 PostgreSQL end-to-end evidence 明确只是代表性 two-source Join–Group；P1b.2 的 fresh PG16.14 `-race` evidence（提交 `36bd815`）已真实通过 online relational 与双库 gateway settlement 两项，因此正文没有把两源实测冒充 16-way live campaign。审计未发现需删除或改写的陈词，故本任务只追加证据对账：TeX/补充材料/论文 README 字节未变、未运行无源码变化所不需要的论文构建；P7.2 的最终范围复核仍保留为后续独立任务。 |
| P2.1 observer V2 化任务重分区 | DROPPED | 2026-08-09 | P2.1 作为独立机械任务的依赖顺序被历史与当前 authority 边界推翻，余量重分区进 P2.2/P2.3，而不是用假 digest 或 V2→V1 降格来满足 grep。计划祖先 `6a0fce5` 已集中实现并测试 `ObserverInvocationV3` / `RunObserverV2`，其提交正文明确“three adapter call sites are deliberately unchanged”且每个 caller 必须随自己的 finalizer cutover 切换；`4418ce5` 随后已把 Artifact 原子迁成 `OpenObserverWindowV3`→同一 window/classifier 的 before/after V2 capture→`FinalizeTaskGateObservationV3`。当前剩余恰为 Scale `executeDependencyE2E` 两处与 ProvSQL `executeProvSQLTaskGate` 两处 schema-v1 caller；其 evidence/validator 仍收 `*ObserverSnapshot` 并走 `NewGatewayControlPlan` / `applyObserverDelta`。可信 V2 window/classifier 只能由 finalizer 预注册，当前 `deploymentContractsV3.ResolveCandidates` 又只枚举 Artifact cells，且测试明确要求 `ExperimentID:"scale"` 解析为零候选；因此单独替换 helper 只能让 Adapter 自行推导分类器、伪造承诺或留下未被 v3 接受的 hybrid，均不允许。**本行不声称原 P2.1 的全局零命中验收已满足**：Scale 两处随 P2.2 的 resolver/evidence/finalizer 原子 cutover 删除；ProvSQL 两处随 P2.3 原子 cutover 删除，并在同一任务删除最后的 legacy helper，届时才执行 `captureBoundObserver` 生产零命中验收；schema-v1 声明迁入 `legacyv14` 仍属于 P2.4。聚焦 observer invocation、strict V2 emission、Artifact carried evidence 与 v1.4 ratchet 测试实际 PASS；未运行 formal build、N4、canary、freeze，未翻任何 capability。 |
| P1.9 finalizer 重放身份闭合 | DONE | 2026-08-09 | `ReproduceExecutionV3` 直接保留授权器产出的 visible/companion `StatementIdentity`；finalizer 在三个 `requiresSchema` 路径上按签名实际存在的 target 闭合 signed↔reproduced 的 exact、strict、row limit、policy fingerprint 与 companion presence，不按执行计数跳过 semantic replay，未改 `physicalquery.Derive` 参数、任何生产包、Adapter carried evidence 或既有 S–C/C–R helper。Gate 31 精确覆盖新边独占的八格：paired visible/companion fingerprint 与 semantic visible/companion 各 exact/strict/fingerprint；direct comparator 测试覆盖两个角色×四字段及 presence 双向。调用点 mutation 后八格逐项明确 FAIL 为 `mutation was accepted`，同时 gate 1–30（含 observer 1/27/28）全部 PASS；comparator no-op mutation 后 2×4 八格及 presence 明确 FAIL；两轮恢复后当前树 38 个 gate/support 顶层测试 PASS、0 SKIP。`GOFLAGS=-buildvcs=false go build ./...`、`go vet ./...` 均 exit 0；`go test -count=1 ./evaluation/internal/experiment/...` package exit 0，但 `TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT` 未设置使 3 个既有 formal-deployment live 测试明确 SKIP，**不记为通过**。v1.4 ratchet 文件与起始 `d42100e` 无 diff，SHA-256 仍为 `85b96783ce7c5fd44393ec5d72280cb5b9d808d20e3d85e88e70b12964dc7b82`，保持 5 文件/11 allowance，聚焦 ratchet/cutover 五项 PASS。改动文件 gofmt 无输出，`git diff --check` 通过；全仓 gofmt 仍只报 P0.2 已有的 `internal/control/execution_binding.go`。P2.2/P2.3 仅修订计划裁决，未碰 cutover、registry、capability、formal build、N4、canary 或 freeze。 |
| P2.2 Scale 切换 | DONE | 2026-08-09 | 仅将 Scale `dependency-e2e` TaskGate 臂原子切换为 deployment finalizer v3：finalizer 以已重验的 `ContractWorkloadCells()` 公有 24 格（12 scale × novel/semantic replay）与独立加载并校验 file/section SHA 的私有 binding 逐格交叉，再经唯一 live Catalog lowering/policy grant 构造候选；Adapter 顺序锁定为 provision → `OpenObserverWindowV3` 失败关闭 → history prefill → V2 before → measured query → V2 after → `FinalizeTaskGateObservationV3`，outcome-merkle/kernel controls 不进入该窗口。novel 只携带实际执行的 visible/companion identity，semantic replay 携带零 executed target；留存 gate 将 acceptance 精确绑定 typed `QueryReceipt` 文档 SHA、sample receipt、window ID/规范 SHA、classifier/operation identity 及私有 binding file/section SHA，双向 same-cell cross-run splice、binding prefix/file/section 与 interval/class 闭集 mutation 均拒绝。`queryreceipt.DocumentSHA256` 是生产 QueryReceipt typed identity 的唯一实现，Adapter、finalizer、RQ5 与 audit 均委托它并 fail closed。v1.4 ratchet 只删除 `scale.go` allowance，仍精确保留 4 文件/10 symbols（含 P2.3 前的 ProvSQL/shared validator）；`exposure-scale` registry 七个 readiness/routing 状态仍全 false、catalog path/SHA 仍空，Adapter capability `scale:false`，registry/capability/frozen contract 无 diff。聚焦四包测试实际 PASS；`go build ./...`、`go vet ./...`、改动文件 gofmt 与 `git diff --check` 均 exit 0/无输出（全仓 gofmt 仍只报 P0.2 既有 `internal/control/execution_binding.go`）。profile verify、11-profile activation-support verify 与 fail-closed registry/capability 断言 exit 0；独立 PostgreSQL 16.14 SQL executability gate 实际为 28 artifacts / 71 rendered cells / 0 failed。`validate.sh` exit 0，但其 SQL 行因未设独立 admin DSN 明确 SKIPPED，**不记为通过**，由前述 disposable SQL gate 补证。第一次准备接受的全库运行在终态前主动中断：只读审查发现 receipt/window/private-binding 跨运行拼接与不可路由 profile 在 history prefill 后才拒绝两处闭合缺口；二者修复并加负测/顺序 AST 守卫后才启动最终审计，该中断不记为通过也不是测试失败。最终从 fresh 两库 harness 执行 `env -u TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN TASKGATE_DB_TEST_TIMEOUT=60m ./scripts/db-test-env.sh test -count=1 -json ./internal/... ./evaluation/... | ... final-v5-dbtest-report ...` exit 0：PostgreSQL `160014`，95 packages、3413 tests、3404 PASS、0 failure、9 declared SKIP、0 undeclared SKIP、0 unmatched allowance、`accepted:true`；9 个 SKIP 不记为通过，其中 SQL-check 两项由同工作树独立 gate 补证，其余保留报告中的 deferred obligations。审计身份 `94455ae1794d84748a9bd346af98fb82c63dab2b+worktree-p2.2-sha256:95c763ea9ac5c5fd5f05832346d35c7163ba6c3690c37b4e9f320e2b25a432f4`；raw JSONL SHA-256（亦为报告内嵌 `report_sha256`）`7ac3e4d072371e8f3b4f4a60398d611c61c2f79f09b731af086c6060c5f0bc38`，summary SHA-256 `523536aa0a0e93f6eef87330cbf3de0c7fcfcb29846089da7f45c1658c1a4840`，文件位于 `/tmp/taskgate-p22-final-dbtest.AJxM16/{raw.jsonl,summary.json}`。未运行 formal build、N4、100×4 canary、v1.5 freeze，未翻任何 capability。 |
| P2.3 ProvSQL 切换 | DONE | 2026-08-09 | 仅将 `executeProvSQLTaskGate` 的 `taskgate` 臂原子切换到 deployment finalizer v3；裸 PostgreSQL `direct` 与 native `provsql` 臂不动。finalizer 从 Contract Index 已 hash-lock 的 workload manifest 重验精确 9 个公开格，只接纳其中 3 个 TaskGate 格；生产 `bindingSource.load()` 独立加载并校验 file/section SHA 的私有 binding，crossing 算法在明确标为 synthetic/non-evidence 的完整校验矩阵上证明 105 个 nonce descriptor（每 scale 35 个）齐全、稳定且精确 `BindingKey` 只选 1 个 private variant，不把合成 fixture 当作部署私有字节证据。acceptance 身份闭合 release/index、binding file/section/key/query 与公开 operation ID；Adapter 顺序锁定为 provision → `OpenObserverWindowV3` 失败关闭 → V2 before → measured query → V2 after → resource delta → `FinalizeTaskGateObservationV3`，只携带签名、paired-novel 且 visible/companion 均实际执行的 target。留存 gate 将 receipt/window/classifier/resource 与精确私有身份闭合，并拒绝同一公开 scale 的 coherent cross-nonce splice、七段数量/release/index/四类 prefix/value/operation mutation、wrong path、legacy hybrid；三臂 schema composition 另发现并修复 TaskGate 空 `field_oids` 从 JSON `null` 变成合法 `[]`，由生产 helper wire test 锁定。删除 ProvSQL 的 v1 accounting 调用、最后一个生产 `captureBoundObserver`、共用 legacy validator 与已无调用的 `statement_plan.go`；ratchet 诚实收紧为 `adapter_bindings.go` 6 项 + `types.go` 1 项，即 2 文件/7 symbols，留待 P2.4 清空，P2.5 的通用 caller 守卫仍保持 report-only。当前冻结 ProvSQL SQL 含 multi-product `ORDER BY`，唯一生产 `sqllowering.Lower` 精确报 `SQL_NOT_LOWERABLE/PAGINATION_UNSUPPORTED`，故 executable candidate construction / pre-registration 在测量前失败关闭；本任务未删除 `ORDER BY`、未另写 plan/推导、未制造 runnable/targeted/live-pass 结论。该新 SQL-profile 缺口不属于已完成 P1b 的“等待项”，必须在 P2.7/P5 前另立任务收口，任何语义扩展须按 P1b.2 标准重跑完整 exposure。`provsql-nonce-join` 保持 `routable:false` / `targeted_validation_passed:false`，registry/capability/frozen contract 无 diff。聚焦四包与 schema/mutation/ratchet/caller/order 测试实际 PASS；`go build ./...`、`go vet ./...`、`git diff --check` 均 exit 0，所有本任务 Go 文件 gofmt 无输出（全仓 gofmt 仍只报 P0.2 既有 `internal/control/execution_binding.go`）；`go test -count=1 ./cmd/...` exit 0，其中 1 个测试包 PASS、4 个包无测试。profile verify、11-profile activation-support verify、Pilot harness exit 0。独立 disposable PostgreSQL `16.14 (Debian 16.14-1.pgdg12+1)` SQL executability gate 实际 PASS：28 artifacts / 71 rendered cells / 0 failed。`validate.sh` exit 0，但其 contract SQL 行因未设独立 admin DSN 明确 SKIPPED，**不记为通过**，由前述独立 gate 补证。fresh-volume 两库 harness 核验 control/business `160014`、31 个 ordinal relations 后，执行 `env -u TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN TASKGATE_DB_TEST_TIMEOUT=60m ./scripts/db-test-env.sh test -count=1 -json ./internal/... ./evaluation/... | ... final-v5-dbtest-report ...` exit 0：95 packages、3456 tests、3447 PASS、0 failure、9 declared SKIP、0 undeclared SKIP、0 unmatched allowance、`accepted:true`；9 个 SKIP 不记为通过，其中 SQL-check 两项由同工作树独立 gate 补证，ProvSQL external live pair 及其余项保留报告中的 deferred obligations。审计身份 `3ead7169569fb2bae40fe74e3a73d91d5bf0643b+worktree-p2.3-sha256:ee4c0d6ecce865b9e339617ad202378d48183b7c4b5e079e2c0c4bcd3040d50c`；raw JSONL SHA-256（亦为报告内嵌 `report_sha256`）`3f671dfb159fddb95df8cc73c96d0cdbd2efdbbaf9016a0a05a7fa633464a96e`，summary SHA-256 `106134830e1e80496e7cac1b8ea877615b97ed8fc4bdc049d62b78bb394b3d41`，文件位于 `/tmp/taskgate-p23-final-dbtest.sqZle3/{raw.jsonl,summary.json}`。未运行 formal build、N4、100×4 canary 或 v1.5 freeze，未翻任何 capability。 |
| P2.4 v1.4 代码隔离 | DONE | 2026-08-09 | 将已审计为 publication-invalid 的 v1.4 observer snapshot/accounting wire schema、严格 bytes decoder 与历史验证测试封装进 `evaluation/internal/legacyv14`；该包不保留 live observer launcher，不依赖当前 module，且任何生产 Go 文件导入它都会失败。删除 Adapter 中无生产调用的 `gatewayStatementCensus` / `applyObserverDelta`，从 current `Sample`、Scale/ProvSQL evidence 与 `sample-v1.schema.json` 删除 `observer_accounting` / `observer_before` / `observer_after`；现役 `StrictJSON` 和 JSON Schema 均在 wire 边界拒绝旧 member，legacy decoder 则因 schema/unknown member 拒绝 V2/v1.5 document，不以同为 1 的 sample schema version 或当前仍为 v1.4 的 release 名伪作 release discriminator。observer source-build closure 改为现役 `observer_invocation_v3.go`，Adapter 与 campaign 共用 copy-returning 单一列表；守卫锁定旧路径/legacy source 零进入。`retiredV14ActiveReferences` 已清空并改为零引用断言，新增 production-import、legacy 反向依赖、current wire surface 与 source closure 守卫；六类可逆 mutation（非空 ratchet、生产导入、legacy 反向依赖、宽松 decoder、current 旧字段回流、legacy source 回流）均按预期使对应测试 FAIL，恢复后聚焦四包与 Gate 29/current-wire/ratchet/import/source tests 全部实际 PASS。首次聚焦编译因 active test 仍调用已迁移的 `resultHeavyPlan` 明确 FAIL，删除该不再属于 current wire 的旧 accounting test（历史等价测试保留于 legacy 包）后终态通过。`go build ./...`、`go vet ./...`、改动文件 gofmt、`git diff --check` 均 exit 0/无输出；全仓 gofmt 仍只报任务前已有的 `internal/control/execution_binding.go`。fresh DB harness 第一次紧接 `up` 的 verify 明确 exit 1：installer 尚未退出时 `taskgate_ordinal relations=0 MISSING`；sidecar 日志随后证明 6 个 publication 安装完成，原样重跑 verify exit 0，control/business 均为 `160014`、ordinal relations=31。独立 disposable PostgreSQL `16.14 (Debian 16.14-1.pgdg12+1)` SQL executability gate实际 PASS：28 artifacts / 71 rendered cells / 0 failed。干净 shell `validate.sh` exit 0，但 SQL 行明确 `SKIPPED`，不记为通过并由独立 gate 补证。最终 fresh 两库命令经 `final-v5-dbtest-report` 接受：96 packages、3467 tests、3458 PASS、0 failure、9 declared SKIP、0 undeclared SKIP、0 unmatched allowance、`accepted:true`；9 个 SKIP 不记为通过，其中两项 SQL-check 已在同工作树补证，ProvSQL external pair、3 个 formal-window live tests、Attack/RLS live preflight 与 v1.5 no-manifest state gate仍保留报告中的阶段义务。审计身份 `a8cbc800923429d2d2018d7732c09f35b70c9624+worktree-p2.4-sha256:370a3d70a39f2a271b4148b89d92c606134b84c8c080f551f2082d410cbc9d7e`；raw JSONL SHA-256（亦为报告 `report_sha256`）`053241692f5d4aea64c6e367cfc8a2518d9cb1adf6bf0e2c3b9547e0a5f94a69`，summary SHA-256 `e5aaacaa49d2315c3e1ef692d1de912b7428889a20171bc5ce373eb9d8980fe9`，位于 `/tmp/taskgate-p24-final-dbtest.eb50lZ/{raw.jsonl,summary.json}`。integration-gates 文档只把 P2.4 structural state 更新为零引用；P2.5 generic caller hard guard 与 ProvSQL multi-product `ORDER BY` 缺口仍明确开放。contract index/release、registry/capability、既有 evidence 字节未改；未运行 formal build、N4、100×4 canary 或 v1.5 freeze，未翻任何 capability。 |
| P2.5 翻转守卫 | DONE | 2026-08-09 | 旧 `TestV3CutoverIsBlockedByTheUnwiredFinalizer` 的 trusted-constructor 扫描只识别带 `experiment.` qualifier 的外包引用，漏掉 `runtime_finalizer_v3.go` 同包 bare `TrustedInputsV3` 构造，因此接线已存在时仍假 PASS；删除该陈旧 blocker、失真 helper 与 blocked 叙述，但把空 reproduced target 必须 fail closed、`internal/physicalquery` 7 个抽取 API 仍导出、`internal/gateway` 8 个已删除推导符号不得回流完整保留为正向 `TestFinalizerRetainsTheSingleSharedTargetDerivation`，未撤掉 §2 红线 6 防线。原 `TestFinalizeObservationV3HasProductionCallers` 现按全生产树闭集硬守 `RuntimeFinalizerV3.FinalizeTaskGateObservationV3`→`finalizeTaskGateObservationV3Core`→`FinalizeObservationV3` 两条 finalizer-side 边；新 `TestRuntimeFinalizerV3HasTaskGateProductionCallers` 独立硬守 Artifact/Scale/ProvSQL 三个固定 workload method 各一次调用 runtime façade。闭集同时固定唯一声明、源码路径、包含函数及其 receiver、identifier/selector 形态、调用 receiver、直接 return/`finalized, err :=` 结果绑定与顶层 statement 形态；扫描包级 initializer、记录 `FuncLit` 深度，并拒绝三个受保护符号在精确 direct `CallExpr.Fun` 外的任何生产引用，因此 method/function value、shadow、闭包、缺失或额外 caller 都失败。六次可逆且可编译 mutation 均由目标守卫自身明确 FAIL：runtime→core 与 core→acceptance 分别变为 0 caller，Artifact→runtime stub 先通过 Adapter 编译再使三路径集合缺 Artifact，额外第二个 `FinalizeObservationV3` 调用使闭集出现 2 caller，额外 function-value 引用命中 indirect-reference guard，将唯一 acceptance call 包进实际执行 IIFE 则以 `functionLiteral:true` / nested result binding 失败；均立即恢复。终态 10 个 caller/trusted-boundary/v1.4-zero/shared-derivation 聚焦测试实际 PASS、0 SKIP；`GOFLAGS=-buildvcs=false go test -count=1 ./...`、`go build ./...`、`go vet ./...` 均 exit 0。首次遗漏显式 `GOFLAGS` 的 `go build ./...` 仅因 VCS stamping `exit status 128` 明确失败，随后按仓库规定带 `-buildvcs=false` 原样重跑通过，未掩盖。Docker DB verify 实测 control/business 均 `160014`、ordinal relations=31；本任务无生产/runtime/schema 变更，未重跑带库全量 harness，也未把 P2.4 结果沿用为本任务 PASS。干净 SQL-check DSN 环境的 `validate.sh` exit 0：protocol/config/schema、profile regeneration、11-profile activation consistency 与 Pilot harness 报 pass；`contract SQL executability` 明确 `SKIPPED`，**不记为通过**，本任务亦未另跑独立 SQL gate。改动 Go 文件 gofmt 与 `git diff --check` 无输出；全仓实际文件 gofmt 仍只报任务前已有的 `internal/control/execution_binding.go`。registry/capability/contracts/profiles/evidence/raw 零 diff；ProvSQL multi-product `ORDER BY` 仍在 shared lowering 前 fail closed，本任务只证明 source wiring，未宣告 runtime integration/live/runnable，未运行 formal build、N4、100×4 canary 或 v1.5 freeze，未翻任何 capability。 |
| P2.6a 封闭 grouped-join 展示 profile | DONE | 2026-08-09 | shared production SQL profile 现只按形状接纳 connected INNER equijoin 的 grouped result：`ORDER BY` 必须恰为完整、直接投影的 group key，每个 key 一次且仅 `ASC`/`DESC`，joined `LIMIT/OFFSET`、partial/unselected/positional/NULLS/aggregate order、Union/ungrouped encoding 与通用 cast 均继续 fail closed；visible delivery order 进入 relational/semantic identity，ordinal companion 仍独立按 group/entity key canonical ASC。Catalog 可证明的 `bigint`/`int8`、`numeric`、`text` identity cast 在 canonical plan 擦除；唯一非 identity presentation 是同一 ordered grouped-join 形状中 natural exact `numeric` 的 `SUM(...)::text`，以 `postgresql-numeric-text-v1` 绑定 QueryPlan、semantic normal form 与 ordinal program，记账自然类型仍为 `numeric`；quoted type/function lookalike（含 `"SUM"`）保留 parser spelling 并拒绝，不被小写重解释。公开 `execute_plan` 不暴露/不接纳该内部 encoding；SQL discovery profile 只陈述上述窄形状，未按 experiment/workload/nonce 开例外。冻结 ProvSQL SQL、public/private matrix 与 105 nonce variant 字节未改；`TestTheProductionProvSQLResolverPreparesEveryValidatedExactFixtureVariant` 将全部 variant 经唯一 `sqllowering.Lower` → `queryplan` → `physicalquery.Prepare` 生产链并实际 PASS（17.63s），且保留 private-matrix drift/crossing 守卫。digest-pinned Business PostgreSQL `16.14` 上 `TestExactProvSQLPreparedPairAgainstPostgreSQL` 实际执行一个 exact visible/companion pair并 PASS（0.44s）：三行 oracle 与顺序一致，公开列为 `status/price/lines/members`，内部 OID 为 `bigint/text/bigint/bigint`，companion 非空且两边均未截断；该测试的 private binding 明示 synthetic，只证明 source/preparation/PG slice，不冒充 targeted/live/deployment/publication pass。首次聚焦真库测试因把 compiler 内部确定性 alias 误断言成公开 `status` 而明确 FAIL，随后测试分离 internal alias 与 public display schema 后终态通过；终态前一次完整 audit 因 reviewer 发现尚待闭合的 lossless/zero-diff guard 而主动中断，无 accepted summary，不记为通过或测试失败。决策 10 的一把 normalizer 与 S2/S4/S5 lexical order 未重开：作者独立提交 `cfc76cf` 只把理由改为冻结查询不依赖总结果顺序；历史 SHA 正向守卫钉住完整 baseline、六份 S2/S4/S5 direct/BDG query/template、normalizer contract 及 `canonical.go`/`result.go`，这些 query/binding/normalizer 字节零 diff。**编译器 identity 单列移动：** `CompilerVersion` v6、SHA `13fd7f3bf8c21209354d04b82c5006c5f29a5b0dd568b820fc2ef43a81f641ed` → v7、SHA `f83c42413d9c38b4cddafb91c7fa3bf7f49f34e2deb9ecaac12f55f75b0a1cb6`；ordinal program/domain 保持 v1。独立 v6 golden gate 覆盖无 relational `OrderBy` 的 single、join、Union、grouped 计划，逐字节锁定 visible/provenance SQL、semantic bytes/digest、ordinal bytes/digest、QueryPlan JSON 与 preparation Plan SHA，唯一排除 compiler identity；ResultEncoding 无完整 `OrderBy` 时本身拒绝，因此没有第二个未声明例外。v7 与 contract release bump 的重认证排入 P4，activation support 不跨 release 继承；本任务未翻 publication registry、Adapter capability/readiness/routing/activation 状态，未改 frozen contract/evidence/tag。最终 `GOFLAGS=-buildvcs=false go build ./...`、`go vet ./...`、`go test -count=1 ./...` 均 exit 0；改动 Go 文件 gofmt 与 `git diff --check` 无输出。`validate.sh` exit 0，但 contract SQL 行因未设 admin DSN 明确 `SKIPPED`，**不记为通过**；同工作树独立 disposable PostgreSQL 16.14 gate 实际 PASS：28 artifacts / 71 rendered cells / 0 failed。fresh DB `up` 后首次 verify 明确 exit 1（installer 尚未完成，`taskgate_ordinal relations=0 MISSING`）；六 publication 安装日志完成后原样复验 exit 0，control/business 均 `160014`、relations=31。最终两库 JSON harness 经 report v2 接受：96 packages、3498 tests、3489 PASS、0 failure、9 declared SKIP、0 undeclared SKIP、0 unmatched allowance、`accepted:true`；9 个 SKIP 均不记为通过，其中两项 SQL-check 由前述同树独立 gate 补证，ProvSQL external live pair与其余 separate-deployment/state 项保留报告中的阶段义务。审计身份 `cfc76cf63840c24289f66b6b22ec1a83061975f2+worktree-p2.6a-sha256:88e1e2f395695db03757693a07566bb404ed23755fd7414c504ec6a72e061eef`；raw JSONL SHA-256（亦为报告 `report_sha256`）`1ca46200627bd939097b2b23082fc7ba2ba854c7edaf99b41e0eecb0f2818d5a`，summary SHA-256 `5dfe268a0b50f86d5cda845936c5cc6ab697db006534e57200e5392d6dc239ae`，位于 `/tmp/taskgate-p26a-final-dbtest.ERxB43/{raw.jsonl,summary.json}`。本任务未运行 P1b.2 exposure、formal build、N4、100×4 canary、campaign 或 v1.5 freeze；P2.7 仍被 P2.6b 阻塞。 |
| P2.6b 按 P1b.2 完整重跑 exposure | DONE | 2026-08-09 | 起始 HEAD `d01a35ccddf07e68cb6cfa95ddfe1313a33b9108`，开跑前 `git status --porcelain` 为空且 `git rev-parse HEAD`=`git rev-parse origin/tkde-artifact-rerun`；Docker 实测 Client/Server 29.1.3。完整命令：`GOFLAGS=-buildvcs=false TASKGATE_EXPOSURE_POSTGRES_IMAGE=postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55 make eval-exposure`。完整 stdout/stderr `/tmp/taskgate-p26b-eval-exposure.log`，SHA-256 `e4a089273462301f41d87e076d76dc2596037f1990b9eb24b489bdae3e5290af`；命令完成并发布 canonical evidence，runner 的认证 PG 实测精确 `16.14 (Debian 16.14-1.pgdg12+1)` / schema v7。A 组基线均已离开：`evaluation/exposure/results.json` `3b46d5dc4219295b8d9bf29a33c443b9cad875ec57ec8bdc783721a592a1df40`→`a0aef7704eaba3459eca5b62fec0de43b83531d0bdd5ff9d97c6b0a4ef31e471`；`rq3-integration.json` `931ae57c421c1522510b195ee1713534c49b0b974b25660118b707d83b20a277`→`e853bb813d865f40eead03d42beea4bd6411e26df5211391d2f2d58726b87094`；raw JSONL `fa4cce99d2ac7f23803f144bdf4925f221e4197ce2d22370d581f2b515496608`→`1e69d2d7a0bcd01da9cde2f3d779a110b37d42d25515e9f9a180aa59a1a9634a`。B 组未变：corpus `8b87db47e51db7db42c759155c8581f95af93e766e7731a0174087ce110f2967`、RQ1 oracle `4548b9ae4ce46e056a39ff6cffafc6cd615a7d34966786f03cc59e749b0e064b`、`evaluation/exposure-performance/results.json` `7c223c3520e5f059039af55435c4b29cd172145e79c44ac93da300a19a4191ad`。C 组实测：RQ1 21/21；RQ2 generated/unique/executed `1024/1024/1024`、duplicate `0`、differential `1152`、metamorphic `1024`、PostgreSQL statements `2176`、mismatches `0`；exposure rewrite `complete` / mismatches `0`；RQ3 deterministic `5/5`，race integration `5/5/0`，两个且仅两个 package terminal PASS（control、gateway），五个 named tests `TestDelegatedTasksShareRootAccountingState`、`TestConcurrentTaskFamilySettlementCannotOverspend`、`TestRelationalOnlinePathAgainstPostgreSQL`、`TestRelationalGatewayEndToEndAgainstPostgreSQL`、`TestExposureV3ChargesDistinctZeroResultPredicates` 均真实 PASS，raw 中 `0 fail` / `0 skip`；RQ4 三条 scaling curve 均 `complete`。独立重放（独立 Python/std-lib 校验，不导入 `record_integration.py`）通过五身份（schema 7、profile `taskgate-exposure-v3`、corpus、RQ1 oracle、PG version）、实际 integration argv/`command_exit_code=0`、canonical refs、results→artifact→raw 两级 SHA；独立重放日志 `/tmp/taskgate-p26b-independent-replay.log` SHA-256 `120466058c692967683af9b4e8473d3e2288f3453822bbba62c6d4064be8eb56`。真实 Docker events 确认 oracle 使用认证 digest、挂载 `db/init` 与 final-v5 datasets，container/network 已 destroy；credential scan 无命中。SKIP 逐条去向：本轮 canonical runner/raw 无任何 SKIP，五个 named test 与两个 package terminal 均实际 PASS；未运行且不记为通过的范围项为 `run-exposure-performance.sh`、formal build、N4、100×4 canary、v1.5 freeze、P2.7；其余无 SKIP。**本轮未跑 `run-exposure-performance.sh`，未刷新三部署 31,296-sample 历史性能 campaign（7,896 full-path + 23,400 ablations），不得把本轮 in-process 三曲线表述为 current-HEAD latency/throughput campaign 重测。** 未改 registry、capability、activation support、frozen contract/evidence 或 tag；未运行 P2.7。 |
| P2.7 已 true 能力的 v3 复核 | DONE | 2026-08-09 | 新增 `docs/final_v5_capability_evidence_provenance.md`。在当前 HEAD `a1db29767a65d9c3a494597bc9d0afc1dc3924b1` 复核 `evaluation/cmd/final-v5-adapter/capability.go` / `main_test.go` 的 6/9 source-controlled truth：RLS、adaptive attacks、ProvSQL、compiler、concurrency、RQ5 为 true；Baseline、Scale、Artifact 为 false。逐项记录 frozen-cell predicate、运行/证据路径、commit/摘要 SHA 与 accounting 边界。I1-B 固定为 `e3622a57fe212f8caaa7857bbe39978b3d867927`；pre-I1-B 的旧支持证据不继承为 v3 publication evidence。结论：ProvSQL 当前只有 v3 source wiring 与 synthetic preparation slice，仍 dormant/unroutable；RQ5 的 `evaluation/v5-outcome/evidence.json` 为 pre-I1-B（implementation `030c1f6`，raw SHA `c5281414953cb4871f50303cbd11dc305e6acd4017ce37af63b010958468e706`）；adaptive security corpus 为 pre-I1-B（`722c9d0`）；RLS 无 qualifying retained run；compiler 仅有 current v7 compatibility/exposure support，concurrency 仅有 current settlement named test，二者都没有 complete formal campaign。全部六项均不升级为 publication claim；需 v3 重测项纳入 P5.2/P5.3/P5.4/P5.5。未改 capability、registry、activation、contract/evidence 或 tag；未运行 formal build、N4、100×4 canary、campaign、v1.5 freeze。 |
| P3.1 formal build | DONE | 2026-08-09 | 从 clean、已推送且等于 `origin/tkde-artifact-rerun` 的起始 HEAD `ca5159e332695f03e4ff3cdad39e5eae242efa84` 执行。红线前置测试实际 PASS、0 SKIP：v1.4 ratchet/active surface 为空，production import、current wire、observer source closure 无 legacy v1.4，finalizer 两级闭集、Artifact/Scale/ProvSQL 三个 runtime caller 与唯一 shared derivation 守卫均通过。先运行 `GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-gateway-build build -root . -tag taskgate-final-v5-gateway:p31-ca5159e`，exit 0；日志 `/tmp/taskgate-p31-formal-gateway-build-ca5159e.log` SHA-256 `23d932157d76a2df4d2a10956a97a59b28a0e36fe55bd106b9ad709e9e078d6c`。1407 个 tracked files 的 context SHA 为 `11b7f1d65f5d10f3c61b4e0f33230b0a07e220b841ac41eceb44f812f8c8a8b8`，source-manifest SHA `ff63f2d9ff2f215f6cb61bf17c28e6e49cbb7d254f43431c957b2ca4e5580daf`，image ID `sha256:9d680b625bc706f6c01fc768524750de6f0f818c5ee8b28f1b84e77ad1879daa`，binary SHA `c2fac46a3757a0193b6356b5a2dc6cd34981db31602d1316497bee900ed5a27f`，platform `linux/amd64`；OCI labels、容器内五份 provenance、重算 binary 与 source-pinned builder/runtime base 逐项一致。随后 `GOFLAGS=-buildvcs=false make formal` exit 0；日志 `/tmp/taskgate-p31-make-formal.log` SHA-256 `1ab6a7a11a743c02a94d92e4566f35652a92d8ed8fd2deb218974e43a52ece97`，TLC image ID `sha256:b36c6645c9a97fdb0b09ada864f53a66703e5d516f85f9f1131b4e03e6593785`，本轮解析到 Temurin base manifest `sha256:3097cbbebb7d490494a98aed2301f284b38f79eba158eef098c6fc8c8af11c23`，TLC 1.7.1 jar SHA-1 检查 OK。十组新 JSON/raw log 均来自该次真实运行并入库：TaskGate `14824257/3255552/depth18`、VectorBudget `263229/201134/6`、SQLAuthorization `3073/2561/2`、MultiTaskAudit `129103/129103/7`、ReceiptAudit `3281/3281/3`、RecoveryLiveness `221/135/7`、ExposureLedger `410766/148706/12`、ArtifactPublication `1324/441/17`、ExposureBitmapRefinement `122976/60680/10`、OutcomeSetAbstractRefinement `20/10/4`（generated/distinct/depth）；10/10 均 `schema_version:1`、`status:passed`、`exit_code:0`，model/config/log SHA 对当前字节逐份闭合，每份 raw 恰有一个无错误完成标记、零 queue 终态与 depth，无 SKIP。Bitmap 在相同 model/config 与相同 generated/distinct state 数下，本次并行 TLC depth 从历史 9 变为 10，按真实输出保留。与既有文档冲突处以本轮证据为准：`formal/REFINEMENT.md` 仍写旧 ArtifactPublication 统计及 bitmap depth 9，`paper/tkde/generated/evidence.tex` 仍写 bitmap depth 9，`evaluation/generated/paper-results.json` 亦为本轮前已陈旧且只覆盖旧六模型；P3.1 不手改/回填这些派生字节，P6 必须从本轮 canonical formal results 独立重生成。provenance 边界：TLC JSON 未嵌 source commit/image，且 `formal/Dockerfile` 的 Temurin base 仍是 mutable tag；`Dockerfile.formal` 虽 pin base，但 `apt-get`/`go mod download` 仍受构建时外部状态影响。因此本任务只声明上述实际 run/image/binary 身份与结果通过，不声明 bitwise reproducible；Gateway build 也不等于 P3.3 的 running-container identity。新增证据和两份 `/tmp` transcript credential scan 0 命中；`go test -count=1 ./evaluation/internal/formalbuild ./evaluation/cmd/final-v5-gateway-build` exit 0（后者无测试文件），`git diff --check` 无输出。只刷新 20 份 formal JSON/log 与本台账；未运行 P3.2/P3.3/P4/campaign，未翻 capability、registry/activation，未推 tag。因本提交会推进 HEAD，本轮 `ca5159e` image 不得冒充 P3.3 measured-tree image；P3.3 必须从届时 clean、已推送 HEAD 重建。 |
| P3.2 N4 重认证 | DONE | 2026-08-09 | 从 clean、已推送且本地/远端均实查等于 `origin/tkde-artifact-rerun` 的起始 HEAD `f5fc9811eef882ec8fc9de5cdf5da24861a31784` 串行执行两次 `KEEP_UP=0 ./evaluation/final-v5-wsl2/scripts/qualify-attestation-footprint.sh`，资格 ID 为 `qualification-p32-01` / `qualification-p32-02`；Docker Client/Server 实测 29.1.3，红线前置的 v1.4 active-surface/production-import/source-closure、finalizer caller/shared-derivation 与 footprint/schema non-scaling 聚焦测试均实际 PASS、0 SKIP。两次真实独立拓扑均 exit 0：目录 `diagnosis-attestation-footprint-qualification-p32-01-20260809T130103Z-f5fc9811eef8` / `diagnosis-attestation-footprint-qualification-p32-02-20260809T130727Z-f5fc9811eef8`；外层 stdout/stderr `/tmp/taskgate-p32-n4-01.log` SHA-256 `35598b1bce9572f6ec47459a4f187734ac1e5604c0bfdcf072e48b2fc6fe45f0`、`/tmp/taskgate-p32-n4-02.log` SHA-256 `d63112123986c859d888dbbb913168d4d7b65b59c7415fd56e38cd2e6d6010bc`。两份 report 文件 SHA-256 为 `e460138559acce31d332cbdba424fa4b15a502c56ddecc359fca2f0563fb1178` / `cfa81d10d1df5b2b2b14fb27764361490e2a64394bc6fc9b5d1ed8e849854645`，full footprint 按设计不同：`ca6c3a769650d6f574078fc61280b50aef6d4ff67ff2569e5aadb77abd02d4f7` / `5ba06cc7d59c1832e0e68bcb72e1ae03cd82dbc87f65fd55c982717970ea80aa`；portable footprint 两轮及 i2b 精确同为 `032e9c53704d8df270693c291d36893e46e6c587b95c59bef2c6d198181ceacf`。独立 JSON 比较不是按总 calls：先锁定 ExpectedSchema digest `e2a3796fb3f5c50f97ebf02a40996b9c95a50e5b028a975cfccd4cf80407d40c`、E=1 与唯一 ordered relation `reporting.final_v5_result_heavy`，再逐 scope 比较精确 `(strict_ast_sha256,calls_per_attestation)` 多重集；constructor/cold-pool、explicit preflight、single-query、paired-query 四 scope 均且仅含 key `e5738df1650276a7f20e677172e067bc62bab12d48c18a378c9b6ed602433842` × 1，全部 stable，constructor==preflight true，11 snapshots / 10 intervals。两轮环境均为 PostgreSQL `160014`、track=`all`、utility=`on`、planning=`off`；load-bearing PostgreSQL ref/repo 均为 `postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55` / `linux/amd64`，每轮 local/container image ID 自洽，独立 identity 文件字节相同（SHA-256 `cd3c6386581f30785080bc292ecd4fa1fc83edafd622329099073b995b7bc215`）。profile registry v1.4 SHA `ca84cba9810d378ea513bdadbf8cb9516a8bc3feaeb26b61af0156d857aee537`、Catalog SHA `533837084c0df141a0fac6e74788a4c2b9eb84611c1f96d4c760806745b4f709`、artifact content-directory SHA `814d4df9971f41b649289aa8b4b3c42f86b588b50c5691f21cf0ec010659f99b` 均与 i2b 相同；run-local manifest digest 按设计不同（`987e773b1b5b31443487bc3e068524e49a723912987501c48254f3afcaf1338a` / `2d0ce4b5478c5c57553ec8aeb535c8914ea6f4185b5b25298fd4e170323ef82d`）。report 列出的 8 项 source identity 及 manifest `2831e532190f1f7b560d6a8fa9a81216ee14052be56971323fcce33ca3c60f94` 两轮/i2b 一致；**边界：这 8 项不是完整 build/runtime 依赖闭包**（未枚举 `internal/sqlidentity`、`internal/dataconnector/statements.go`、其余 Compose overlay/materializer），本轮可追溯性另依赖 report 的 clean/published 完整 commit，不把 8 项表述为完整闭包。新 `attestation-footprint-p32-agreement.json` 由 fail-closed `jq` 比较从两份 report 派生而非复制/手改旧字节，SHA-256 `bb97df1764ecaa06ff08aaae70f383350fc44dc077848dadea818e0813413acf`；比较 transcript `/tmp/taskgate-p32-n4-comparison.json` SHA-256 `fab517d788babb498bd39891378ec02bf442390b7844cd74a36ab7c0347b2af0`，旧 i2b agreement（SHA-256 `7a44d1e1f5bfa7a625a60655b10b148d5358a2f598348f299481de2478e11c27`）及两组旧证据字节未改。生产 `AttestationFootprintV2.SHA256()` / `PortableSHA256()` 对两份新 report 重算均 PASS，日志 `/tmp/taskgate-p32-production-digest-check-rerun.log` SHA-256 `9bc420d72de27ad6da47344ee8a91899b655fc7a60d46bca077091f40012787d`；第一次临时 audit 测试因传入相对路径、从 package 工作目录找不到 report 而明确 FAIL（日志 SHA-256 `cfed293ce03f794ba9f6ee6f124ead5ec1776a405c50e302f6a4dc875e880b46`），改用绝对路径后才计为通过。独立 report↔PostgreSQL identity 与嵌套 profile manifest 配对最终 PASS；前两次审计命令因误认 manifest 顶层字段/record 而 exit 1，不记为通过。两轮脚本退出后的 Compose project containers/volumes/networks 均独立核验 `0/0/0`；两轮各约 5.3 GiB 的 `profile-artifacts` / `snapshot-index-artifacts-full` 保持 ignored，绝未暂存，只 force-add 每轮 7 个小文件及新 agreement（共 15 份 evidence）。15 evidence + 5 transcripts 对 2 份 `.env` secret-like 精确值与 URL-userinfo/PEM/secret-assignment heuristic 终态扫描均 0 命中；第一次 heuristic 命令因本机 `rg` 无 PCRE2 而未执行，不计为通过，随后用默认正则引擎重跑。test-only `retainedQualificationRun` 原子推进到 p32-01，使仓库 resolver 自检实际读取同一轮 report/identity；生产 launcher 仍无默认值，P3.3 必须显式使用同一轮配对文件，不得跨轮拼接。两份 report 的 publication/capability/activation/formal flags 均 false；P3.1 formal image不是本任务输入，未运行 P3.3 canary/P4/campaign，未翻 capability、registry、activation 或 contract，未推 tag。 |
| P3.3-prep 100×4-only runner 准备 | DONE | 2026-08-09 | 从 clean、已推送且本地/远端均为 `f8cc56522189c97d5aa10e8746a6d53141053731` 的起始 HEAD 实施，Docker Client/Server 开工实测 29.1.3。审计确认开工时唯一 Artifact launcher 只能从 `artifact.example.json` 请求默认六格、终局写死 `6 * SAMPLES`，没有 P3.3 单格入口；本任务没有把六格运行冒充 canary。runner 新增严格 `SCALES` 白名单：仅允许冻结六格的非空无重复子集，拒绝显式空值、未知格、首尾空项与空白，按冻结顺序规范化；未设置仍精确默认六格。生成 pilot config 前要求 source-controlled example 仍恰为单一 `result-heavy` / `novel` / 完整六格，再只替换本轮请求 scales；`expected` 改为 `selected_scale_count * SAMPLES`，终局同时要求总数、每个所选 scale 恰 `SAMPLES` 条，以及每条 exact Artifact/Result-heavy/novel/TaskGate PASS、非 publication 且 `taskgate_acceptance_v3 != null`。完整 Dataset Binding 的 `.schema_version == 1` / `.status == "valid"` / `.artifact_cells == 6` 校验保持不变，selector 不参与或放宽 binding。显式 readiness 完成后、任何 measured operation 前新增 fail-closed formal-window live-gate 阶段：导出本轮 Compose project 与固定 Gateway endpoint，以 `go test -count=1 -json` 精确选择三项到期测试，并要求全事件流 0 FAIL/0 SKIP、三个且仅三个 test terminal 各唯一 PASS；go test、tee、FAIL/SKIP/缺失/重复/额外 terminal 任一异常都在 measurement 前 exit 1。由于作者尚未提供私有 binding，本任务**没有启动拓扑或 canary，三个 live gate 也未实际运行、不记为 PASS**；通过的是 launcher 守卫与合成 JSONL 判决器测试。`bash -n evaluation/final-v5-wsl2/scripts/run-artifact-targeted.sh`、`GOFLAGS=-buildvcs=false go test -count=1 ./evaluation/internal/experiment -run '^TestArtifactTargeted' -v`（3 个顶层测试及全部正负子测试 PASS、0 SKIP）、`go vet ./evaluation/internal/experiment`、`go build ./...`、改动 Go 文件 gofmt 与 `git diff --check` 均 exit 0/无输出。实施文件只改 runner，另同步其守卫测试、权威计划 §P3/§6 与本 append-only 台账；`evaluation/final-v5-wsl2/raw/`、registry、capability、activation、contract、既有 evidence/tag 均零 diff。未运行 formal build、N4、P3.3/P5.1、campaign 或 P4，未翻任何能力。计划已纠正：P3.3 与 P5.1 共用字节完整、mode 0600 的私有 deployment binding（Artifact 恰六格且其余段不得裁剪）；P3.3 必须从本提交完成后 clean、已推送的新 HEAD 重建 formal Gateway image，不能复用 P3.1 image。 |
| P3.3 Result-heavy 100×4 v3 canary | BLOCKED | 2026-08-09 | 唯一阻塞输入是作者尚未提供的字节完整、mode 0600 私有 `TASKGATE_DATASET_BINDINGS` 常规文件路径；不存在合法的 100×4-only/cropped/fixture 替代，未绕过。输入到位后须从 clean、已推送的 P3.3-prep HEAD 重建 image，显式 `SCALES=100x4`，并成对传入 P3.2 `diagnosis-attestation-footprint-qualification-p32-01-20260809T130103Z-f5fc9811eef8` 同目录的 `attestation-footprint-v2.json` + `postgresql-identity.json`；三项 live gate 必须届时实际 PASS、0 SKIP 才能开测。当前 canary 未运行，不记为通过。 |
| P3.3-prep2 Artifact canary / publication binding 分离 | DOING | 2026-08-09 | 按 C16 实施 credential-free Artifact-targeted deployment binding 与 selector/runner 边界；本行只记录开工，不声称实现或验证完成。P3.3 未运行，三项 live gate 未运行，无 measured sample；完整 publication binding 仍不可用，未授权 capability/registry/contract-release/evidence/tag 变更。 |
| P3.3 Result-heavy 100×4 v3 canary | TODO | 2026-08-09 | **append-only 口径校正，取代上一条 BLOCKED 行的当前状态：**完整 12/6/105 publication binding 不再是 P3.3 输入；旧行“唯一阻塞是作者提供 binding”的前提已被 C16 与 P3.3-prep2 裁决取代。canary 仍未运行，须等待 prep2 完成后从 clean、已推送 HEAD 以 `SCALES=100x4`、同轮 P3.2 配对文件及三项实际 PASS/0 SKIP live gate 开跑。 |
| P4.0 publication-wide oracle closure and binding generation | TODO | 2026-08-09 | 待裁决 Scale N/K 冲突，建立独立 Scale 与 ProvSQL 105 格 FactSet oracle，裁决 Artifact private dependency section，并在 closure 后自动生成完整 12/6/105 binding 交作者逐字节审阅；禁止 placeholder、测试摘要或运行后反填。 |
| P5.1 Artifact 六格 | TODO | 2026-08-09 | **append-only 口径校正：**旧“需私有 dataset binding”备注不再是当前要求；P5.1 是使用现场生成 Artifact-targeted binding 的非发表 targeted acceptance，全部 sample 保持 `publication_eligible=false`。Artifact 正式 publication samples 与完整 12/6/105 binding 硬门属于 P5.4 `run-deployment.sh` campaign。 |
| P3.3-prep2 Artifact canary / publication binding 分离 | DONE | 2026-08-10 | 实际开工 HEAD 为与 origin 同步的 `71bf925490852e902711cea29037e45151c61493`；附件所列 `b6eb34a` 是其父提交，差异仅 `.gitignore` 与本工作树 `AGENTS.md`。新增 credential-free、canonical、create-exclusive、普通非 symlink/mode 0600 的 Artifact-targeted binding：embedded Contract Index 重验、冻结六格 Bridge query/oracle/NxC/SQL/result identity、Result-heavy profile/clearance、同一 shared read-only live dataset probe、qualification/identity exact file SHA 全部 fail closed；validation report 绑定 exact file SHA。targeted runner 在 fresh Business PostgreSQL ready 后、Gateway/live gates/measurement 前生成并核验该文件，只把 exact SHA 交给 ProfileBinding，marker 限定 Artifact path + V3 observer acceptance 且明确 FactSet oracle 未闭合；三个 publication-private env 与 Adapter `--validate-binding` 已从该 runner 移除。Artifact selector 在三个 env 真正 unset 时成功，Scale/ProvSQL/wildcard 仍 fail closed；`run-deployment.sh`、Adapter、完整 publication binding、Scale/ProvSQL private-load 分支零 diff。离线负例覆盖六格/subset、contract/oracle/Catalog/registry/qualification/identity byte drift、missing/invalid/placeholder probe、canonical/unknown/duplicate、0600/symlink/exclusive 与 secret-free output。规定的 bash、两组 `go test`、`go vet ./...`、`go build ./...`、`git diff --check` 均 exit 0；experiment filter 的 JSON 审计为 100 PASS / 0 SKIP / 0 FAIL。Adapter 全包仍有 4 个既有 live 测试因未设置其显式 live/DSN 开关而 SKIP（Attack、Compiler PostgreSQL、ProvSQL external pair、RLS），明确不记为通过；本任务 command tests 实际 PASS。全仓规定 `gofmt -l` 仍只报 P0.2 已登记的既有 `internal/control/execution_binding.go`，本任务 Go 文件无输出。**P3.3 未运行，三项 live gates 未运行，无 measured sample；完整 publication binding 仍因 C16 所列 oracle/Scale 冲突不可用。**未修改 capability、registry、contract release、dependency oracle、既有 evidence 或 tag，也未运行 P5 formal campaign。 |
| P3.3-premeasurement-E2BIG-fix | DONE | 2026-08-10 | clean、已推送的 `c28523f28344aa6f28ac367153be487c36446a36` 首次 P3.3 尝试在任何 Artifact cell 前于 Adapter build-manifest 阶段失败：`failure_stage=adapter_build_manifest`；`error=E2BIG / source_listing passed as one jq argv`；`formal_gateway_built=false`；`compose_started=false`；`live_gates_run=false`；`measurement_started=false`；`measured_samples=0`；`publication/capability/registry unchanged`。这是 pre-measurement runner/build-manifest argv-size failure，明确**不是 Artifact cell failure**，不得记作 P3.3 canary、live gate 或 measured sample 的 PASS/FAIL。修复保持完整 tracked-file listing 与原 `source_sha256` 字节语义，以 `printf '%s'` stdin → `jq -Rs` → `source_files:.` 取代单一大 argv；同型的正式 publication runner Adapter、Observer、RQ5 manifest 写入一并修复，binary 0700、manifest 0600 与既有 source-build verification 契约不变。动态回归在测试 shell 内生成超过 256 KiB 的 listing 并调用 targeted runner 的 exact helper，严格 JSON 解码及 source bytes/source SHA/binary/commit/build command 核验实际 PASS；规定的 targeted `bash -n`、`go test -count=1 ./evaluation/internal/experiment -run 'ArtifactTargeted|SourceBuildManifest'`、`go vet ./...`、`go build ./...`、`git diff --check` 均 exit 0，publication runner `bash -n` 亦 exit 0；全仓 `gofmt -l` 仍仅报告既有 `internal/control/execution_binding.go`，未修复该文件。P3.3 仍须从本修复的 clean、已推送 HEAD 重新启动；未修改 contract、registry、activation、capability、既有 evidence 或 tag。 |
| P4.0 两项作者裁决 | DONE | 2026-08-10 | 提交 `451695a`（仅 docs，path-limited；开工 HEAD 为与 origin 同步的 `32f879dd9308578c0310da3b9c2d53da01736e8e`）。本行只记录**裁决已作出并入库**，不声称任何实现、oracle、binding 或测量。裁决一（作者决策 18，注册为 C17）：Dependency Scale 的 runtime 向契约靠拢，而非反向缩契约——`DependencyScaleSpec` 增 `ExistingFacts`（=`CandidateFacts`），**每一格**都必须声明 `ExistingFacts` 规模的 history（零重叠格亦然，只是与 candidate 不相交），`RootBefore.DependencyCardinality` 改为 `ExistingFacts`，`RootAfter` 改为 union `2N-5K` 并使用其自身独立摘要（不再等于 candidate 摘要）；记账不变量原样保住：charged `== N-5K`、actual-charged `== 5K`；history 保持决策 5 的 `sum(metric)` 身份于 `(M-K,2M-K]`。裁决依据为既有仓库事实：现 `validateDependencyCellBinding` 要求 `history.DependencyFacts == spec.OverlapFacts` 且拒绝零重叠格的 history，现 `ValidateScaleEvidence` 要求 `RootBefore == spec.OverlapFacts`（零重叠为已验空根）与 `RootAfter == spec.CandidateFacts`，合起来即 existing ⊆ candidate、union == candidate，正是 C6 已否决的模型；且 `DependencyScaleSpec` 从无 `ExistingFacts`，契约的 existing 侧从未实现；冻结的 414,000 行 `final_v5_exposure_scale` 仅在契约侧读法下才有后半段用途。裁决二（作者决策 19，注册为 C18）：Artifact private dependency section **删除**而非补建 independent oracle——现 Artifact 路径只把生产响应的 `ActualInfluenceFacts`/`InfluenceSetSHA256` 抄入 Sample，无任何 finalizer 与绑定值对账（Scale 有 `CandidateDependencySHA256` 对账），故它是无断言消费的死输入，保留即 C8 所指"看似已核验的 exposure 记账"。**尚未做、且不得据本行认为已做**：决策 18/19 的代码实现、三个 Scale FactSet 摘要（existing/candidate/union）的独立 oracle、ProvSQL 105 格 oracle、完整 12/6/105 publication binding 生成，均仍为 P4.0 未完成项；本次未改任何代码、contract、registry、activation、capability、既有 evidence 或 tag，未跑 canary、live gate 或测量。**时序约束**：决策 18 改变实测 runtime 行为，与决策 17 同理，必须在 v1.5 冻结与任何 P5 measurement 之前落地；决策前模型下测得的 Scale 样本一律不得沿用。若实现限于 `evaluation/` 且不触及 `Derive`，不触发决策 17 的全套 `evaluation/exposure` 重跑，此点须在实现时实查确认而非假定。**本行不改 §5 P4.0 与 §6 阻塞清单第 3 行的既有措辞**（二者仍写"待裁决"），以决策文档 18/19 与 C17/C18 为准。 |
| P3.3-premeasurement-profile-artifact-mode-fix | DONE | 2026-08-10 | 从 clean、已推送且与 origin 同步的起始 HEAD `e689a8c2851b607ea2d0789f7e0a96e9912be070` 启动的 retained run `targeted-p33-artifact-100x4-02-20260809T173754Z-e689a8c2851b` 属于 **pre-measurement deployment failure**，明确不是 Artifact cell failure。该次实际达到的新地面：`formal_gateway_built=true`，formal Gateway image `sha256:4f107d7aa9e19c755fb23cb68b47d235ae5d4a6c2120fd287ba22de8c4c8cdfd` 从 1427 个 tracked files 构建成功；fresh deployment phase 1 全部 health checks 通过，三个 snapshot-index 作业均 `Exited (0)`；从共享卷复制 24 个 artifact 文件；现场生成 mode-0600、7828-byte 的 `artifact-targeted-deployment-binding.json`，其 validation 为 `status=valid`、artifact/selected cells `6/1`、binding SHA-256 `cd04544bdb64c34c54c33363d54a8e6cbec7db63bd784f709841ce0a91b656b4`。随后在 phase 2 启动 Gateway 时失败：`failure_stage=phase2_gateway_start`；`error=activate snapshot index publications: load snapshot publication "final-v5-result-heavy-v1": open /var/lib/taskgate/snapshot-index/final-v5-result-heavy-v1: permission denied`。根因为 targeted runner 的 `umask 077` 作用于材料化器唯一未显式 chmod 的 `os.Mkdir(targetDirectory, 0o755)`：profile 根目录为 uid 1000/mode 0755、publication 子目录却为 uid 1000/mode 0700，虽其四个文件均为 0444，但以 UID/GID 10001:10001、read-only rootfs 和 `:ro` mount 运行的 formal Gateway 无法穿越该中间目录；同材料化器已有根目录 0755 与文件 0444 的显式 chmod，故这是 mounted profile directory tree 的输出模式受调用者 umask 影响的可复现性缺陷，不是环境变化。failure capture 保留 ps/log，退出 cleanup 后该 Compose project 的 containers/volumes 实查均为 0，failure directory 原样保留。修复保留 `umask 077` 与宿主顶层 0700 边界，在 publication `Mkdir` 后显式 `Chmod(0755)`；非并行回归在 `syscall.Umask(0077)` 下调用真实材料化器并精确断言 publication 子目录为普通非 symlink/mode 0755，实际 PASS。相关 package tests、`go vet ./...`、`go build ./...`、`git diff --check` 均 exit 0；全仓 `gofmt -l` 仍仅报告既有 `internal/control/execution_binding.go`。`live_gates_run=false`；`measurement_started=false`；`measured_samples=0`；不得记作 P3.3 canary、Artifact cell 或 live gate 的 PASS/FAIL。P3.3 须从本修复的 clean、已推送 HEAD 重新启动；未修改 contract、registry、activation、capability、既有 evidence 或 tag。 |
| P3.3 Result-heavy 100×4 v3 canary | FAIL | 2026-08-10 | 从 clean、已推送且 `HEAD=FETCH_HEAD=origin/tkde-artifact-rerun` 的起始 HEAD `0dc072f9e6be8ba6792bcac9b11b7925224d9e44` 执行；Docker Client/Server 实测 29.1.3。精确命令输入为 `Q=evaluation/final-v5-wsl2/raw/diagnosis-attestation-footprint-qualification-p32-01-20260809T130103Z-f5fc9811eef8`，`SCALES=100x4 RUN_ID=p33-artifact-100x4-03 ATTESTATION_QUALIFICATION="$Q/attestation-footprint-v2.json" POSTGRESQL_IDENTITY="$Q/postgresql-identity.json" ./evaluation/final-v5-wsl2/scripts/run-artifact-targeted.sh`，`SAMPLES` 未设置并按冻结 runner 默认精确为 1；两项输入实查为同目录已跟踪普通文件，SHA-256 分别为 `e460138559acce31d332cbdba424fa4b15a502c56ddecc359fca2f0563fb1178` / `cd3c6386581f30785080bc292ecd4fa1fc83edafd622329099073b995b7bc215`。retained run 为 `targeted-p33-artifact-100x4-03-20260809T181354Z-0dc072f9e6be`；整轮 stdout/stderr `/tmp/taskgate-p33-artifact-100x4-03.log` SHA-256 `56bea09881ec45e1a875c3df8c783c18b73f9aacbd730aec786d17ee440675c5`，runner exit 1。`formal_gateway_built=true`：从 1427 个 tracked files 真重建，formal build log SHA-256 `e9f9e969c742c8a0135d576d96fdc7a966e8c128af60a5c167b0d4f7296682c7`，image `sha256:e6b4d61f14c7cf0428f72e4b0467a28aacdc6d6368850e2bd2252ad750aeaf6f` / `linux/amd64`，Adapter/Observer binary SHA-256 `fe0e0ecf9ed53ee4bb7d7d861f9a62f673b772fd138e6c464ca291c4f167d382` / `71cc60beb5d32e3c6b12833ba1866b419d0b48948869b572d84034261d8be159` 且两份 build manifest 均绑定起始 HEAD 与 source SHA `2149c29739832d8dd0717acf5df9730376b10dc86083f248c406e2e6737946f8`。phase 1 全部 health checks 与 jobs 完成，复制 24 个真实 artifact 文件；现场 targeted binding validation 为 `status=valid`、artifact/selected cells `6/1`、binding SHA-256 `a0fee13660160cc94f4ef65f413ebb47dd72a3234ceb55f5c88c33c2b30cea6d`；mode fix 后 formal Gateway phase 2 与首次 readiness 均通过，running PostgreSQL identity 与 retained p32-01 identity 精确一致。`live_gates_run=true` 且在 measurement 前实测 `TestFormalDeploymentRunsTheApprovedHealthcheckLive`、`TestPeriodicLivenessProbesAddNoBusinessStatements`、`TestExplicitReadinessOutsideTheWindowStillAttests` 各唯一 PASS，总计 3 PASS / 0 FAIL / 0 SKIP（JSONL SHA-256 `c69cdf3254f3087b8d2c88ebb2bc005714c1b519a180ca4413f2df195f5c02f6`）。随后 `measurement_started=true`，第一格 `deployment-01-p01-sample-0001` 的原始失败输出为 `adapter sample deployment-01-p01-sample-0001: sample carries no deployment profile binding`、`adapter process replicate 1: adapter wrote stderr; content was suppressed by the evidence secret boundary`、`exit status 1`；retained sample JSONL SHA-256 `efa58673b88b930ffddeaac7398bccb0978fac3316ebc5dd85c181b2cb0c00c1`，恰 1 条 Artifact/result-heavy/100x4/novel/TaskGate 记录，`status=invalid`、`error_code=adapter_sample_validation_failure`、`taskgate_acceptance_v3=null`、`publication_eligible=false`，故 `samples passed/total=0/1`。这是 **Artifact cell failure**，不是 pre-measurement runner/构建/部署缺陷；依规则本轮即 P3.3 FAIL，未修复、未重跑、未改判定、未加 v1.4 回退。退出 cleanup 后该 Compose project containers/volumes/networks 实查 `0/0/0`，failure 目录及大体积 `snapshot-index-artifacts-full` / `profile-artifacts` 原样保留且保持 ignored；只 force-add 14 份小型 first-hand run/provenance 文件，连同 `/tmp` transcript 共 15 文件对 `.env` 的 20 个 secret-like 精确值及 URL-userinfo/PEM/secret-assignment heuristic 扫描均 0 命中。本轮不声明 Artifact path 或 V3 observer acceptance 通过，更不声明 independent FactSet oracle closure、publication readiness 或任何 capability/registry/activation/contract-release 状态翻转；未修改这些状态，未推 tag。 |
| P3.3 Result-heavy 100×4 v3 canary 口径校正 | FAIL | 2026-08-10 | **append-only 校正，后行覆盖前行的归因措辞但不改判定或证据：**`d633c85` 上一行的 P3.3 `FAIL` 事实、命令输入、构建/部署/live-gate、摘要与 SHA-256、cleanup/secret scan、未翻状态等全部事实及摘要保持不变，既有 evidence 字节未改。仅将“Artifact cell failure”收窄为保留证据能支持的结论：measurement 已开始，三项 live gate 实测 3 PASS / 0 FAIL / 0 SKIP；第一格在产出 receipt 与 v3 acceptance 之前于 adapter protocol 层中止；`raw/deployment-01.jsonl` 的唯一记录是 `runner.go:195-199,229-266` 合成的 schema-safe 占位而非 Adapter 原 sample，其 receipt/result/root/facts/pipeline 字段为空或零，且 `taskgate_acceptance_v3=null`。因此该 FAIL **不得读作“v3 Artifact 路径未通过验收”**：它根本未走到 `artifact.go:305-317` 的 v3 acceptance；`runner.go:580-581` / `profile_binding.go:218-222` 只是最先撞上的 protocol gate，`ProfileBinding` 到 `artifact.go:281` 才赋值，而 `runner.go:491-510` 抹掉了真正 stderr，故根因在 `d633c85` 记账时未知。 |
| P3.3-diagnose 根因诊断 | DONE | 2026-08-10 | 仅起一轮非 canary diagnosis 拓扑：`Q=evaluation/final-v5-wsl2/raw/diagnosis-attestation-footprint-qualification-p32-01-20260809T130103Z-f5fc9811eef8; SCALES=100x4 RUN_ID=diagnosis-p33-adapter-stderr-01 KEEP_UP=1 ATTESTATION_QUALIFICATION="$Q/attestation-footprint-v2.json" POSTGRESQL_IDENTITY="$Q/postgresql-identity.json" ./evaluation/final-v5-wsl2/scripts/run-artifact-targeted.sh`；retained diagnosis 路径 `evaluation/final-v5-wsl2/raw/targeted-diagnosis-p33-adapter-stderr-01-20260810T004257Z-d633c85d5e13`，transcript `/tmp/taskgate-diagnosis-p33-adapter-stderr-01.topology.log` SHA-256 `d5f1befb237addef98c7a9394fd2c497894d20a0da8b7d69ec58443fef9e9688`；这不是 P3.3 attempt，3/3 diagnosis live-gate PASS 不计作 P3.3 PASS/FAIL，产物不作 canary evidence。复用该拓扑、当轮 source-built Adapter 与当轮 ProfileBinding，绕开 evidence Runner 直接向 `final-v5-adapter --experiment artifact` 喂同一 JSONL operation，stdout/stderr 仅落 mode-0600 `/tmp`；精确 secret 值与 URL-userinfo/PEM/secret-assignment 扫描均 0 命中。Adapter stderr `/tmp/taskgate-diagnosis-p33-adapter-stderr-01.adapter.stderr.log` SHA-256 `393a9aa7142bfa4ee60b8ff787a3c9b0751c84b9bd241927ca9524ff595dafaf` 为 `artifact cell artifact/result-heavy/100x4/novel: run observer before: exit status 1`；再以同一最小 observer environment 直接运行同一 binary，stderr `/tmp/taskgate-diagnosis-p33-adapter-stderr-01.observer.stderr.log` SHA-256 `a0fec5e3802d8ae62611e5ce85bdbf88b666b2281301b6630758ad9dc558338b` 取回 PostgreSQL 原错：`read Business PostgreSQL census: ... ERROR: UNION types text and numeric cannot be matched`，指向 `pg_current_wal_lsn() - '0/0'::pg_lsn`。根因为 **harness 的 Final-V5 observer 缺陷**：`evaluation/cmd/final-v5-observer/snapshot_v2.go:37-57` 的四臂 `UNION ALL` 在第 5 列把 `E/T/R` 的 text 与 `S` 臂未 cast 的 numeric LSN 差值合并；认证 Business PostgreSQL 上只读 `pg_typeof` 实测 `numeric|text|160014`。失败发生于 `artifact.go:205-210` 的 observer-before，早于 `artifact.go:214-217` 的被测 TaskGate query、`:281` ProfileBinding 和 `:305-317` v3 acceptance，故不是被测系统缺陷或 v3 验收失败。静态枚举 `artifact.go:281` 前 18 个 error 早退点为 `156,160,166,173,178,185,189,203,210,217,224,228,235,240,244,256,262,270`（其中字面 `return sample, err` 为 `240,244,256,262,270`）；本轮 stderr 将实际命中唯一定位到 `210`。prep2 差分命令 `git show c28523f^:evaluation/final-v5-wsl2/scripts/run-artifact-targeted.sh | nl -ba | rg 'TASKGATE_DATASET_BINDINGS|TASKGATE_FINAL_V5_BINDING_(FILE|SECTION)_SHA256|validate-binding'` 证明移除的是 `TASKGATE_DATASET_BINDINGS`、`TASKGATE_FINAL_V5_BINDING_FILE_SHA256`、`TASKGATE_FINAL_V5_BINDING_SECTION_SHA256` 与 inspection flag `--validate-binding`；当前 Artifact 精确 selector 在 `deployment_finalizer_v3.go:398-414` 不调用只供 Scale/ProvSQL 的 private binding loader `:416-435`，故 `removed_env ∩ exact_artifact_required_env = ∅`，prep2 未切断 Artifact 输入。聚焦 selector/wiring 两测试实际 PASS、无 SKIP。诊断结束执行同一 Compose 文件集 `down --volumes --remove-orphans`，该 project containers/volumes/networks 实查 `0/0/0`；未修 bug、未改 runtime/Adapter、未重跑 P3.3、未动 `d633c85` evidence、未翻 capability/registry/activation/contract、未推 tag。 |
| P3.3-fix observer census 修复 | DONE | 2026-08-10 | 从 clean、已推送且等于 `origin/tkde-artifact-rerun` 的起始 HEAD `6845d303cd304e3c7f126e0ec44f38609da157ef` 开工。只改 Final-V5 observer census：S 臂第 5 列由原生 `numeric` 的 LSN 减法改为与既有 Control 写法一致的 `pg_wal_lsn_diff(...)::bigint::text`；强制 production Docker TTY 的首次 live regression 随后真实暴露 CRLF framing，第一次测试原始失败为 `Business census returned a malformed WAL position`（JSONL SHA-256 `ffb124fbeb1f5dc45a9b4e35d26a18387ca815d5148bb596895f1d9a253a37c6`），故在同一 census parser 单点把 `\r\n` 规范为 `\n`，未改 Docker/runtime/Adapter/finalizer/断言/secret boundary。四臂五列逐格审计后，E/S/T/R 的 20 个 effective cell 修后均为 `text`；同一认证容器的 live `pg_typeof` 输出精确为四行 `arm|text|text|text|text|text`，transcript SHA-256 `e873e24898614fb9e5f9630f888291a43fe81ee330346701e125a3fed70c129b`。新增 DSN-gated regression 直接走 production `collectV2(..., before, ...)` 与协商过版本的 Docker engine，对 digest-pinned `postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55` / `linux/amd64` 真执行完整 `businessCensusShell` 恰一次，把 DSN/container 的 `system_identifier` 与 postmaster、running container identity 和合法 `ObserverSnapshotV2` 逐项对齐；定向 JSON terminal 为 1 PASS / 0 SKIP / 0 FAIL（SHA-256 `0abf1687dbf2e9c4cc929cdc1c050aa8ae8771bb848c2ce05991893c14af25f2`），observer 全包带 DSN 为 78 PASS / 0 SKIP / 0 FAIL（SHA-256 `deed4f84b564af06b06c881b0ab673f1dae51f57940f8c58505ffdaf90e80ada`）。**Q1：**P3.3 事故前从未有完整 shell 的真实 PostgreSQL 执行证据；修复前唯一可证 live 接触是本次失败的 Artifact observer-before 及其直接 binary 复现，这些尝试均在 PostgreSQL 类型解析处中止，故成功 full-shell live execution=0、合法 observer-before V2 snapshot=0；既有测试全为 canned `censusEngine`/`fakeEngine`，旧 2026-08-04 schema-v1 diagnosis 早于 `businessCensusShell` 引入，不计入。**Q2：**observer 目录不在 P3.2 p32-01 报告的 8 项闭式 source identity 中，membership 实查为 0；8 项 manifest 重算仍为 `2831e532190f1f7b560d6a8fa9a81216ee14052be56971323fcce33ca3c60f94`，报告文件 SHA-256 `e460138559acce31d332cbdba424fa4b15a502c56ddecc359fca2f0563fb1178`，消费端也不拿 observer/current commit 重验该 qualification，故本修复不使 `qualification-p32-01` 失效，P3.3 仍可使用它与同目录 PostgreSQL identity 作配对输入；observer 新字节将由 P3.3 新 HEAD 的 whole-tree build manifest 与 observer 自身 source inventory 独立绑定。fresh DB 第一次 verify 在 installer 完成前真实报 `taskgate_ordinal relations=0 MISSING`，不记通过；六 publication 安装完成后原样复验为 control/business `160014`、relations=31、track=`all/on/off`，exit 0（SHA-256 `21e32316c6785732b5cd54240e8740f2d2ae931cdd7713e5bc52c8f5bab18ebe`）。改动 Go 文件 gofmt 无输出；`GOFLAGS=-buildvcs=false go build ./...`、`go vet ./...`、`git diff --check` 均 exit 0。P3.3 canary 尚未启动；未重跑 N4，未改 capability/registry/activation/contract/evidence/tag。 |
| P3.3-04-record run 04 失败证据入库 | FAIL | 2026-08-10 | 从 clean、已推送且 `HEAD=origin/tkde-artifact-rerun` 的起始 HEAD `9316682fa30c68045174eee648856dbb0683d962` 运行所得目录 `evaluation/final-v5-wsl2/raw/targeted-p33-artifact-100x4-04-20260810T014910Z-9316682fa30c`，本任务只按原字节 force-add 其中 14 份小型 first-hand run/provenance 文件，未纳入大体积 `profile-artifacts` / `snapshot-index-artifacts-full`、binary 或空 `run.log`。formal image 为 `sha256:9e7b417bda89656f6077bcaafa10f36da3f5c2a38d93ba79ff140f5f13c48af1` / `linux/amd64`；measurement 前三项 live gate 实测 3 PASS / 0 FAIL / 0 SKIP。唯一 100x4 sample 为 `0/1`，`status=fail`、`error_code=artifact_measurement_failed`、`publication_eligible=false`；整轮 transcript `/tmp/taskgate-p33-artifact-100x4-04.log` SHA-256 `b1d2f0c320ad5cfde00823c0dd2e845d0bb56ce817cc43133028ce4cccba3ea6`，raw JSONL SHA-256 `3ad230ec470581be9def8fc510e578e173207589309320e751878c7e23b7d283`，live-gate JSONL SHA-256 `0eb3029ab636a7b3bb025c87328deda59382a0d7749be04c1ddb73d6dcb4eda6`。失败归属只收窄到代码与保留样本能证明的边界：`artifact.go:281` 的 ProfileBinding 已成功并出现在 sample，before/after observer window 完整，visible/companion Business statement 精确 `+1/+1`（`business_sql_delta=2` 且 `:288-290` 的分量守卫未早退），`:305` 的 v3 acceptance entry point 已到达；finalizer 返回 error 后命中 `:314-315` 早退，`:317` acceptance 赋值未执行，故 `TaskGateAcceptanceV3` 为 nil 指针且因 `omitempty` JSON 无该 key。可表述为“到达验收并未通过”；finalizer 内部具体哪道 gate 失败本行仍**未确认**，不得据此断言 core adjudication 缺陷、被测系统缺陷或新的 harness 缺陷。凭据门禁按作者裁定修正并复跑：14 份 evidence + transcript 共 15 文件对 `.env` 的 20 个 secret-like 值有 2 次 exact-value 子串命中，二者恰为 `artifact_verification.observer_window.before/after.runtime_identity.gateway.version`；两字段逐字节等于 source-controlled 公开常量 `GatewayRuntimeIdentityV1Version = "taskgate-gateway-runtime-identity-v1"`（`evaluation/internal/experiment/gateway_runtime_identity.go:12`），而当前 16-byte `GATEWAY_OBJECT_STORE_ACCESS_KEY` 只是该常量的字面子串，故泄露面为零。JSON-scalar 与 credential 完整相等、URL-userinfo、PEM、secret-assignment 四类均严格为 0；旧“任何 exact-value 子串命中必须为 0”的口径已由“非零命中逐项证明为公开常量子串，四类强门仍为 0”取代。未修 finalizer、未改验收逻辑、未重跑 canary、未翻 capability/registry/activation/contract，未推 tag。P3.3-04-record |
| P3.3-04-diagnose finalizer 失败根因诊断 | DONE | 2026-08-10 | **Q1 原始 error：**`artifact cell artifact/result-heavy/100x4/novel: no frozen contract operation reproduces the signed execution; 1 candidate(s) were refused: artifact/result-heavy/100x4/novel: the Gateway signed a preparation the finalizer does not reproduce: two independent preparations of one operation disagree on [predicate footprint]`。**Q2 gate：**`evaluation/internal/experiment/runtime_finalizer_v3.go:369-371` 因 `ReproduceExecutionV3` 返回 error 拒绝唯一 Artifact candidate，随后 `:379-383` 返回 zero-match error；根判定是 `evaluation/internal/experiment/reproduce_execution_v3.go:187-188` 的 `binding.RequirePreparedSame(prepared.Binding())`，底层 `internal/preparedbinding/preparedbinding.go:381-403,441` 逐成员比较并唯一列出 `predicate footprint`。**Q3 归属：(a) 被测系统的真实不合规。**run 04、diagnosis runner、绕过 evidence runner 的直跑 Adapter 三个 live operation 均由 Gateway 签出相同 `preparation_inputs_sha256=87dbe20076297104b0a5c12f01a80a4515f4e4cad8fa9b208da026b9a9d40c42`、`predicate_footprint_sha256=09d696f60327552c01da5a43d240059de493b3a21baae932e341a8bbdd6fb840` 与 prepared binding `9f3ff32bb6edee2a25a8917919c53e9010a3e6b11bd703f206c0dac7dda1a4d7`；finalizer 已取得并重放同一 preparation input identity，逐成员比较除 predicate footprint 外全部一致，因此这是 TaskGate 已签名运行时 preparation 不满足 v3 独立可重建不变量，不是本轮没给/给错 finalizer 输入；本任务不进一步补全同输入为何产生不同 footprint，也不修。**Q4 targeted 边界：无设计边界冲突。**Artifact selector 在 `evaluation/internal/experiment/deployment_finalizer_v3.go:398-414` 直接解析六格，publication-wide private binding loader 只由 Scale/ProvSQL 的 `:416-435` 调用；Artifact reproduction 需要的 mounted publication bundle manifests 由 targeted runner 的 profile artifact 目录提供，且本轮已越过解析/材料加载直达 preparation member comparison。finalizer 链没有要求完整 FactSet oracle、publication-wide binding 或 Artifact private dependency section，故 marker 的 `publication_factset_oracle_ready=false` / `claim_scope=artifact_path_and_v3_observer_acceptance_only` 与本错无关，作者决策 19 未被冲撞。仅启动非 canary `diagnosis-p33-04-finalizer-stderr-01`，retained 路径 `evaluation/final-v5-wsl2/raw/targeted-diagnosis-p33-04-finalizer-stderr-01-20260810T022827Z-7abd59212c4c`，起始 clean/pushed HEAD `7abd59212c4c2c3475858275dd806ef024e3cac5`，两 build manifest 实测 `source_sha256=0a66e443194daa35ed9e452023acbaa619378f6ce2756bcd9e35bd3139bc2eac`，Adapter/Observer binary SHA-256 `fe0e0ecf9ed53ee4bb7d7d861f9a62f673b772fd138e6c464ca291c4f167d382` / `9142aab0d0aa29fad59e96c9fa9a368dcf0f2ed8ae6dc31d07586121eb6fb5e3`；topology transcript SHA-256 `8a30d6a60137f292d87418e135757778b7502e94c847177feb0112ba768a3fda`，direct operation/stdout/stderr SHA-256 `5bf9b8fce9817d61e3c0a64c0f1b8530ddaefadb375cabbefdb3a1a16782a32f` / `c245858149d1e416a5761e0acc53ad5d55fbb45b70556ec96205f9b251fad0ec` / `46bd01c62381a152c625086b08d3c00405f8d2e55a7813013338836c37ae41b8`，diagnosis raw SHA-256 `e95881e748c6caced0a1dee7c9759605e6023eb20e373ce241a49304c810849b`，targeted/profile binding SHA-256 `32aa45cada42338f7b418617d6fb4fe29b4833d8fe3f04d61efe912d4a5af832` / `4b20125a226417f9b9dbe8de799e034038b41c5e746392e9d047418633a9cbec`。所有 direct operation/stdout/stderr/exit 与外层 logs 均 mode 0600；终态 24 文件/15 JSON streams 对 `.env` 20 个 secret-like 精确值扫描仅 4 次 exact 子串命中，全部是 before/after 两份 sample 中 source-controlled `GatewayRuntimeIdentityV1Version` 公开常量包含 access-key 默认字面子串，JSON scalar 完整相等、URL-userinfo、PEM、secret-assignment 均为 0（scan SHA-256 `87b1b59e3812bec3c4a30cebfb92f48e25c555eb3db493d2125efa1722dfd634`）。同一 Compose 文件集 cleanup exit 0，project containers/volumes/networks 实查 `0/0/0`，cleanup log SHA-256 `6b85bcee423de0dbbb346fb6256b13072c4ac431cbfb128e51f1010111a55ca1`。这不是 P3.3 attempt，diagnosis 的 3/3 live-gate PASS 不计入 P3.3；未跑 canary、未改代码/finalizer/assertion/`.env`/既有 evidence/capability/registry/activation/contract/tag。`P3.3-cred-rotate` 按作者裁决延后到 P5 正式 campaign 前空档：公开默认 16 字节无泄露面，活体诊断期间为一次只动一个变量不改部署配置，届时须连带验证 fresh deployment 仍可启动。P3.3-04-diagnose |
| PF-investigate predicate footprint 不可重现机理定位 | DONE | 2026-08-10 | **append-only 归因校正：本行以实查机理覆盖上一行 `P3.3-04-diagnose` 的 Q3“被测系统真实不合规”归因，但不改 run 04 的 FAIL、既有 evidence 字节或 fail-closed 判定。Q1 两侧值与稳定性：**Gateway 已签名侧在 run 04、diagnosis runner、直跑 Adapter 三个真实 operation 中均为 `predicate_footprint_sha256=09d696f60327552c01da5a43d240059de493b3a21baae932e341a8bbdd6fb840`、prepared binding `9f3ff32bb6edee2a25a8917919c53e9010a3e6b11bd703f206c0dac7dda1a4d7`；从同一 frozen candidate 精确重建的 finalizer 侧在同进程 3 次及另 3 个独立 `go test -count=1` 进程中均为 `predicate_footprint_sha256=7d236af7147d348bab02ee0612ae49e40e592853982fe05c29a6b02956b3fe72`、prepared binding `882fb36eeaebc846d0b674fb10762902b4a297ae24481eb9ff60429d604ad511`，故两侧都是**稳定且确定性地不同**，不是 map 迭代、时间、指针或进程 seed 非确定性；两侧 `preparation_inputs_sha256=87dbe20076297104b0a5c12f01a80a4515f4e4cad8fa9b208da026b9a9d40c42`、`grant_sha256=646a66b82e4e2196481198a64583925acdd1a866d9710835cf2be7aa27602163` 仍相同。**Q2 红线 6：未发现 Gateway/finalizer 第二份生产实现。**Gateway 实际链为 `evaluation/cmd/final-v5-adapter/artifact.go:214-216` → `internal/gateway/query.go:381,464,477` → `internal/gateway/view_execution.go:25,39-41` → `internal/gateway/query.go:667-676` → `internal/gateway/preparation_inputs.go:63-87` → `internal/physicalquery/prepare.go:32-54,68-71,167-174,1036-1052` → `internal/queryplan/predicate_footprint.go:111-134,181-278` → `internal/physicalquery/preparation.go:183-190,240-330,461-505,741-752` → `internal/preparedbinding/preparedbinding.go:458-466`；finalizer 实际链为 `evaluation/cmd/final-v5-adapter/artifact.go:305-313` → `evaluation/internal/experiment/runtime_finalizer_v3.go:252-278,287-332,343-371` → `evaluation/internal/experiment/reproduce_execution_v3.go:127-188` → **同一** `physicalquery.Prepare`、`derivePredicateFootprint`、`queryplan.BuildPredicateFootprint`、`predicateFootprintSHA256` 与 `preparedbinding.DomainDigest` 链，再由 `internal/querybinding/v2.go:428-437` → `internal/preparedbinding/preparedbinding.go:375-451` 比较且在 `:441` 唯一报 footprint；Gateway 只转交该 sealed binding。静态审计同时发现 semantic View 的 shape adapter 在 `internal/physicalquery/semantic_view.go:481-488` 重复了 raw-scope 子推导后仍汇入同一 `BuildPredicateFootprint`，它不在本 cell 或两侧分叉上，但修复不得继续复制该子推导。**Q3 结构化前像实际差异：**两侧 footprint 的 `Version=taskgate-predicate-footprint-v1`、`RawLiteralCount/UniqueAtomCount/DuplicateCount/NullAtomCount=5/5/0/0` 以及五个语义 atom 完全相同，即 `category EQ s:alpha/s:beta/s:delta/s:gamma`（text、`en_US.utf8`/`2.36`、同一 product/role/public field/expression）和 `row_id LE i:100`（bigint）；唯一上游输入字节差异是同一 scope 的 Gateway/Control JSONB 前像 `{"category": ["alpha", "beta", "delta", "gamma"]}` 与 finalizer `json.Marshal` 前像 `{"category":["alpha","beta","delta","gamma"]}`。认证 PostgreSQL pinned 16.14（`server_version_num=160014`）实查 JSONB 回读确为前一字节串；写入/回读链为 `internal/control/migrations/001_initial.sql:27-33`、`internal/control/entities.go:372-375,420-430,462-486`，finalizer 紧凑编码为 `evaluation/internal/experiment/deployment_finalizer_v3.go:861-878`。`Grant.Canonical` 在 `internal/physicalquery/preparation_inputs.go:86-115,168-194,655-688` 先规范化 scope，解释了 grant/preparation inputs 摘要为何相同；但 ordinary footprint 在 `internal/physicalquery/prepare.go:1042-1047` 直接散列 raw `MandatoryScope`，使 Gateway/finalizer 的 `effective_scope_sha256` 分别为 `37edbe3b0f3fb133a3e2f6097d9d06044e70031eee6c8e6ab502a245bb479a7f` / `e9e96aa0ff815178646539ecc3a1f1bf40025ce99a1fc5e1be99504171c2db8b`，进而 `ContextSHA256` 分别为 `b4557b748e2c79ed7324dc7bc1f03d62bd8bc279922ad7d1c77c9fc9acff30ac` / `5d9dbf0532bd5038a31ebdfa47fee2550b106c7697a0c9979ca63eec1ccd19b3`，每个 atom 的 `PredicateContextSHA256` 同步变化；按 atom hash 排序后的实际 literal 顺序分别为 `delta,i:100,gamma,alpha,beta` / `alpha,gamma,beta,i:100,delta`，`AtomSetSHA256` 分别为 `57dc5c88ec4c5779a50023b7264bde8109ad346b270ce18037d93e5983e3ce58` / `4a0ab63ca89c8ba820d8fa91311a4208e21ada48fcdbb37c33d0d44cfccb8200`，最终得到上述两个 footprint。只把 finalizer 输入的 scope raw bytes 换成 JSONB 带空格字节、不改任何其他字段，即精确重建 signed context、atom set、footprint **及完整 prepared binding**，已把因果定位到该字段。**Q4 决策 16 关联性：排除。**`PredicateBindings.Products` 当前只能是 `map[PredicateProductKey]Product`，新 `(branch role, product)` 键与 converter 在 `internal/queryplan/predicate_footprint.go:92-134`；决策 16 实现提交 `09df11a` 是 run 04 起始 `9316682` 的祖先，两侧又运行同一当前 `physicalquery.Prepare`。100×4 frozen SQL `evaluation/final-v5-wsl2/sql/contracts/S6-x4-bdg.sql:1-5` 仅有 `final_v5_result_heavy` 单 source，`internal/sqllowering/lower.go:107,121-130` 生成 `Plan.Product` 而非 `Plan.From`，`internal/physicalquery/prepare.go:167-170` 构造恰一个 composite key，无 branch 次序或跨 branch 去重；三项 composite-key 聚焦测试 `TestPredicateFootprintResolvesUnionBranchesByRoleAndProduct`、`TestPredicateFootprintRequiresEveryUnionBranchBinding`、`TestPredicateFootprintRejectsNonCanonicalLegacyProductRole` 实跑 3 PASS / 0 FAIL / 0 SKIP，且 `git blame` 将 raw-scope 散列追溯到早于决策 16 的 `5b49731`。**明确成因归属：我们的确定性 canonicalization/sealing 缺陷，不是被测系统或 workload/DBMS 的性质；fail-closed 拒绝本身仍正确，但不得作为捕获被测系统真实不合规的论文卖点。**建议修复方向（只提未实施）：在唯一 `internal/physicalquery` 推导内复用 `Grant.Canonical()` 或抽出单一 canonical effective-scope helper，让 ordinary 与 semantic View 都只散列规范化 scope，并增加 compact JSON、PostgreSQL JSONB round-trip、ordinary/View 与 finalizer full-binding 等价回归；不得在 Gateway/finalizer 各补一份。修复后 P3.3 重跑前置：另立获准的修复任务与独立提交；从 clean、已 push 的新 HEAD 在 digest-pinned PostgreSQL 16.14 下完成 focused regressions、全仓 build/vet/test（DB 项不得 skip）、validate 与 3/3 live gates；离线 exact reproduction 必须先做到 PreparedOperation 全成员相同；再同源 fresh formal build/deploy，用新 run ID 生成新证据，绝不复用/重标 run 04，且 capability/registry/activation/contract 在真实验收通过前保持不变。p32-01 闭式 8 文件身份当前不含 `internal/physicalquery`，是否可复用须在修复时按其 exact manifest 与 consumer 重新实查，若触及其 source/语义则先重跑 P3.2。本调查只使用保留证据、`/tmp` overlay 调试与临时 `diagnosis-pf-jsonb-20260810` PostgreSQL 容器（已清理）；未改 Go/finalizer/验收/`.env`/既有 evidence/状态，未重跑 canary、未推 tag。PF-investigate |
| PF-freeze-restore 冻结 normalizer 字节恢复与台账校正 | DONE | 2026-08-10 | **append-only 事实校正，既有行原字节不改：**唯一引入漂移的 commit 为 `a1db29767a65d9c3a494597bc9d0afc1dc3924b1`（parent `d01a35ccddf07e68cb6cfa95ddfe1313a33b9108`）；`git log -p -1 a1db29767a65d9c3a494597bc9d0afc1dc3924b1 -- evaluation/finalv5oracle/result.go` 证明它只改了 `SortedResultHasher` 的 ordinary-path 注释，却使文件 SHA-256 从 author-approved `3704a5180883d542f6ca155cb9157e8d66e16a24a257096424079817c1407fb9` 漂移为 `39f40567178d8d5b2de7268356360e898a793b1d3e0e693ebab49ecfff201cd4`。本文件原 `P2.6a` 行（追加本行前第 708 行）声称“这些 query/binding/normalizer 字节零 diff”；该声明在 `P2.6a` 完成时成立，但从紧接着的 `a1db297` 起与事实不符，且同 commit 的 `P2.6b` 行“未改 ... frozen contract/evidence”遗漏了这次冻结文件漂移，本行明确校正。冻结守卫由 parent `d01a35c` 引入并 pin approved SHA，故自 `a1db297` 起首次且持续失败；实查其间没有任何提交或台账行据该守卫、完整 `go test ./...` 或全仓 freeze gate 判过“通过”。可信度边界：P2.6b exposure 的真实 runner 在漂移前的 clean/pushed `d01a35c` 上完成（RQ3 终止于 `2026-08-09T12:17:56Z`，`a1db297` 提交于 `12:21:51Z`），其 canonical evidence 不受影响；其后 P3.1 formal 不消费该 oracle、P3.2 closed-source 8 项不含该文件，P3.3 run 03/run 04/diagnosis 均保持 FAIL/non-publication 且 build manifests 如实记录漂移后的 `39f405...` source identity，没有把 approved 字节冒充为运行字节，也没有据此翻转任何结论。因此未发现已入库 evidence 的值或可信结论被这次纯注释漂移扭曲；但 `a1db297` 至本提交 parent 的历史树确实不满足冻结守卫，历史 source/build identity 与恢复后 identity 不得互换或替代。现已只恢复 `evaluation/finalv5oracle/result.go` 的 author-approved 原字节，文件 SHA-256 精确回到 `3704a518...07fb9`，approved SHA/冻结契约未改；exact guard terminal PASS（`/tmp/pf-freeze-restore-guard.log` SHA-256 `e3a140a5f52c6795a923921eeedcf7a3bbb0b4cb9f0e5cf0c6f69c81cb27ef32`），oracle package PASS（`/tmp/pf-freeze-restore-oracle.log` SHA-256 `a8233ea507443988c12e3c7838b8434ee99fc9eb266e13ea20b824d68b40b441`），审计 transcript `/tmp/pf-freeze-restore-audit.log` SHA-256 `52972f9d582ce25836f1923d38334382fe1be16da21fec1b4b6f1831edfbac18`；未改任何既有 evidence、contract release、activation/registry/capability，未推 tag。PF-freeze-restore |
| PF-fix canonical effective-scope 可重现性修复 | DONE | 2026-08-10 | 起始于已 push 的 `PF-freeze-restore` HEAD `aba43e977ee1bc88d3ad895389d80a84fc337442`。**三项前置均实查且不阻塞：**（1）对旧 run 04 footprint `09d696f60327552c01da5a43d240059de493b3a21baae932e341a8bbdd6fb840`、binding `9f3ff32bb6edee2a25a8917919c53e9010a3e6b11bd703f206c0dac7dda1a4d7`、context `b4557b748e2c79ed7324dc7bc1f03d62bd8bc279922ad7d1c77c9fc9acff30ac`、predicate-set `57dc5c88ec4c5779a50023b7264bde8109ad346b270ce18037d93e5983e3ce58` 逐值搜索全部 tracked files 与 contract release tags v1–v1.4，只命中历史台账、唯一 run 04 FAIL evidence `raw/deployment-01.jsonl`，footprint 另作为本回归的“新值必须不同”负向哨兵；frozen contract、activation support、registry、contract release 与非历史 evidence 均 0 命中，故没有 author-approved frozen pin 被本修复失效。run 04 原始 evidence 一字节未改，其四摘要明确是 **pre-fix** 身份，不能与 post-fix 比较、互换或替代。（2）helper 不直接修改 `physicalquery.Derive`；生产链为 ordinary `gateway/query.go -> physicalquery.Prepare -> derivePredicateFootprint -> canonicalEffectiveScopeSHA256` 与 View `gateway/view_execution.go -> PrepareSemanticView -> deriveViewPredicateFootprint -> 同一 helper`，随后经 `prepared_exposure.go`、V5 reservation、semantic replay/ordinal derivation、execution binding、`ordinal_derivation.go` FactSet 及 `control` exposure/budget settlement 写入同一运行时记账；因此虽未改 Derive 本体，确实改变 production exposure Fact identity，触发决策 17 的完整 `make eval-exposure`，作者已批准在下一独立提交真实重生成，旧 canonical evidence 不可沿用或回填。（3）`qualification-p32-01` 的 `source_identities` 恰 8 项，`internal/physicalquery/**` membership=0；当前 8/8 working/committed SHA 仍匹配，按生产 framing 重算 manifest 精确为 `2831e532190f1f7b560d6a8fa9a81216ee14052be56971323fcce33ca3c60f94`，qualification/paired PostgreSQL identity SHA 分别为 `e460138559acce31d332cbdba424fa4b15a502c56ddecc359fca2f0563fb1178` / `cd3c6386581f30785080bc292ecd4fa1fc83edafd622329099073b995b7bc215`；targeted consumer 显式绑定并重验该 pair，但不把 physicalquery 纳入这份闭式 source identity，故资格仍有效且无需重跑 P3.2。**单一实现：**只在 `internal/physicalquery` 新建 `canonicalEffectiveScopeSHA256`，复用 `Grant.Canonical()`；ordinary 与 semantic View 两条路径共用，domain literal 全仓该包仅 1 处，没有在 Gateway/finalizer 复制推导，符合红线 6。**离线硬门与语义不变证明：**用 run 04 同一 frozen operation inputs 分别以 compact 与 PostgreSQL JSONB 带空格 scope 重建，完整 `PreparedOperationBindingV1` 深比较相同、双向 `RequireSame` 与 `QueryExecutionBindingV2.RequirePreparedSame` 均 PASS；post-fix binding/footprint/context/predicate-set 精确为 `882fb36eeaebc846d0b674fb10762902b4a297ae24481eb9ff60429d604ad511` / `7d236af7147d348bab02ee0612ae49e40e592853982fe05c29a6b02956b3fe72` / `5d9dbf0532bd5038a31ebdfa47fee2550b106c7697a0c9979ca63eec1ccd19b3` / `4a0ab63ca89c8ba820d8fa91311a4208e21ada48fcdbb37c33d0d44cfccb8200`；structured preimage 的 version、四计数 `5/5/0/0` 与五个 semantic atoms 逐项等于 run 04 记录，新 footprint 与旧 `09d696...` 不同是预期 byte canonicalization 结果而非回归。硬门+真实 PostgreSQL `::jsonb::text` round-trip 为 2 PASS / 0 SKIP（`/tmp/pf-fix-run04-hard-gate.log` SHA-256 `1931d69fa98d6bc17addc6ee944ca1ebffec262b864b6728249276040d36b689`）；semantic View compact/indented full-binding/structured-footprint live test terminal PASS（84.85s 全仓轮，独立日志 SHA-256 `572e5022c799fadb6d85d03c70aebaf91548e9a1359b6700d2c2d6925ab2cfe8`）。**门禁：**第一次 fresh verify 在 installer 完成前如实 FAIL（两库 160014 但 relations=0），installer exit 0 后原样复验为两库 160014、relations=31、track=`all/on/off`；为全仓轮再次 fresh-volume 并在 installer exit 后 verify PASS（日志 SHA-256 `94bcd87dd01d8d0e7b8e73440ba94fdf743a8399ae0e6f6087283249dcfe26e4`）。先前因误用预置 benchmark schema DSN 失败的两项 SQL-check 已按仓库隔离约定在 digest-pinned 空白 PostgreSQL 16.14（初始 `final_v5_benchmark` schema count=0）实际 terminal 2 PASS / 0 SKIP / 0 FAIL（`/tmp/pf-fix-finalv5sqlcheck-live.log` SHA-256 `0032fd05783d2795f7eb54723ad8eeeae8d2ca17d7c132b66f95765592427bf5`）；独立 contract SQL gate PASS 28 artifacts / 71 cells / 0 failed（SHA-256 `b025d30c314ad462abbab65292305458d3320b8f990b3325bcb5a905fc3b9f96`）。相关三包带库终态 physicalquery/gateway/experiment PASS（`0.072s/2409.090s/36.507s`，日志 SHA-256 `86d5d11f34466ee7af0cab871f1ece935fdbae7a2f176ed52e81126e6f281420`）。全仓 `go test -count=1 -json ./...` 报告 v2 接受：102 packages、3629 tests、3620 PASS、0 failure、9 declared SKIP、0 undeclared SKIP、0 unmatched allowance、`accepted:true`；9 个 SKIP 不记为通过，其中 SQL-check 两项由上述同树独立 2 PASS 补证，其余 7 项保持 report 的 separate-deployment/state 义务；raw/summary SHA-256 `90326dcd263892ae8393339063cd4423c699660d821fbe94e7dae59fbbbd36a8` / `a60f74e1704c635f4d48f27b43e5961d762993bed570d920c9dec9227a4e5fa5`。改动文件 gofmt、`go build ./...`、`go vet ./...`、`git diff --check` 均零输出/exit 0；`validate.sh` 使用另一空白 digest-pinned PostgreSQL 16.14 真跑，protocol/profile/activation/SQL/Pilot 全 PASS 且日志 0 `SKIP/SKIPPED`（SHA-256 `8e7851cc5336fe724a8be0a4219f237a8bc5a3679794d22c2d89519680fc04e8`）。未改既有 evidence/frozen contract/activation/registry/capability，未跑 canary，未推 tag。PF-fix |
| PF-exposure-harness exposure 构建上下文与记录器修复 | DONE | 2026-08-10 | 从 clean/pushed `187967dba7be61a995f05986f48ba68616622046` 开工；三份 canonical exposure evidence 在本任务中保持 `a0aef7704eaba3459eca5b62fec0de43b83531d0bdd5ff9d97c6b0a4ef31e471` / `e853bb813d865f40eead03d42beea4bd6411e26df5211391d2f2d58726b87094` / `1e69d2d7a0bcd01da9cde2f3d779a110b37d42d25515e9f9a180aa59a1a9634a`，未运行 exposure measurement。Docker 实际 `FROM scratch; COPY . /` 解包审计证明 build context 由 1048 regular files / 33,837,414 bytes / 0 symlink / 0 raw 精确变为 1050 / 33,870,135 / 0 / 2；无删除路径，新进入且仅进入 `diagnosis-attestation-footprint-qualification-p32-01-20260809T130103Z-f5fc9811eef8/{attestation-footprint-v2.json,postgresql-identity.json}`（31,638 / 431 bytes，SHA-256 `e460138559acce31d332cbdba424fa4b15a502c56ddecc359fca2f0563fb1178` / `cd3c6386581f30785080bc292ecd4fa1fc83edafd622329099073b995b7bc215`）；其余 raw 仍被排除，context 审计 transcript SHA-256 `930b71bdd4564e16dcfb1200edbc2458367f1b2de6ce1ecaf239f947bcb988f1`。build stage 按既有未版本锁定的 apt 约定加入 `jq`，最终镜像 `sha256:98eb7b129c9990c1df6cd145f1c58920abe3bd8820b99556b8b9f14f04b625d2` 实际构建 exit 0，日志 SHA-256 `2fe4cfaa37958a69bd3393c3b40d6089c8d5b5682e8918688f57111fc082d03c`，镜像内为 `jq=1.6-2.1+deb12u2`、`libjq1=1.6-2.1+deb12u2`、`libonig5=6.9.8-1`。该步骤依赖构建时 `golang:1.25-bookworm` tag 解析、实时 Debian mirror/index、网络与当时包版本，**不是 hermetic**；未含糊表述为已完全可复现。qualification/binding 与真实执行 `jq -Rs` 的聚焦容器测试为 36 terminal PASS / 0 FAIL / 0 SKIP，日志 SHA-256 `b2d12980b793cd128a9edabf6db59880f70330b6d71e401942a9c32746687a76`；qualification 缺失分支由 `Skipf` 改为 `Fatalf`，不得跳过。一次额外审计命令包含 `TestTheDeploymentResolversPreRegisterAClassification` 时，该测试因本要求明确不进入 context 的另一 retained snapshot artifact 报 1 SKIP；此项不记为通过，也不用于本任务聚焦门禁。外层 tee 记录器使用 `set -euo pipefail`，在任何后续命令前保存完整 `PIPESTATUS`，算日志 SHA 后显式恢复 producer/tee 状态，因此尾随 `sha256sum` 不再覆盖失败。改动 Go 文件 gofmt 无输出，`go build ./...`、`go vet ./...`、`git diff --check` 均 exit 0；未改 runtime、既有 evidence、contract release、registry、activation、capability 或 tag。PF-exposure-harness |
| PF-exposure-refresh PF-fix 后 exposure 证据刷新 | DONE | 2026-08-10 | 从已 push、clean 且 `HEAD=origin/tkde-artifact-rerun=e44817ed9990c9e1e417eb2629c32bdbe22dd8d8` 的 PF-exposure-harness 树启动，唯一一次完整命令为 `GOFLAGS=-buildvcs=false TASKGATE_EXPOSURE_POSTGRES_IMAGE=postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55 make eval-exposure`；外层 producer/tee 状态均为 0，完整 stdout/stderr `/tmp/taskgate-pf-exposure-refresh.log` SHA-256 `8f5a483cbf932c4ee34889e58cd821fda09480ccd14ab3d001f252466677c125`。本批 canonical evidence 是在 PF-fix 之后从修复树**真实重新生成**：旧字节来自 canonical effective-scope / footprint 语义修复之前，因 production exposure Fact identity 已变化而失效；本次不是手改、复制或回填任何字节。三份且仅三份 canonical 文件刷新：`results.json` `a0aef7704eaba3459eca5b62fec0de43b83531d0bdd5ff9d97c6b0a4ef31e471`→`4532a59c5278993c0f508174f5bf6d553b9fc15e23a7ee1f833d169a43748f80`，`rq3-integration.json` `e853bb813d865f40eead03d42beea4bd6411e26df5211391d2f2d58726b87094`→`c7be32148592ca6dc3efaeeae6a69f0a3e0fa1eb871877ab45f20e0cd1b12b4d`，raw JSONL `1e69d2d7a0bcd01da9cde2f3d779a110b37d42d25515e9f9a180aa59a1a9634a`→`3565056ddc70d463d04cb72ab0609fab7f63613889b456cbe02754a5d54f89cf`。RQ1 实测 21/21；RQ2 generated/unique/executed `1024/1024/1024`、duplicate 0、differential 1152、metamorphic 1024、PostgreSQL statements 2176、mismatch 0，认证 PostgreSQL `16.14 (Debian 16.14-1.pgdg12+1)`，exposure invariance `complete` / 19 cases / 0 mismatch；RQ3 deterministic 5/5，`-race` integration 五个 named tests `TestDelegatedTasksShareRootAccountingState`、`TestConcurrentTaskFamilySettlementCannotOverspend`、`TestRelationalOnlinePathAgainstPostgreSQL`、`TestRelationalGatewayEndToEndAgainstPostgreSQL`、`TestExposureV3ChargesDistinctZeroResultPredicates` 各唯一 run + terminal PASS，control/gateway 两包各唯一 terminal PASS，artifact 5/5/0，全 raw 0 FAIL / 0 SKIP，时间窗 `2026-08-10T06:49:14.325121151Z`–`06:49:35.426272118Z`。RQ4 三条 in-process scaling curve 均 `complete`：`observe_rows` 10/100/1000/10000 的 ns/op=`132453/1728210/20618270/223947934`；`normalizer_depth` 1/4/8/16/32=`3765/12678/32137/87534/489863`；`novel_vs_replay` 10/100/1000/10000=`10567/115236/1531349/14669397`，Fact/charge 不变量逐点闭合。独立 stdlib 重放未导入 recorder，验证 schema/profile/corpus/RQ1 oracle、实际 argv、唯一 terminal、时间窗、canonical refs 与 results→artifact→raw 两级实字节 SHA，日志 SHA-256 `698793b00cc0fb6f225784542fcc638a5d5e915f68bf951aed4e182f742c0e93`；精确差分审计为 results 14 个叶值（1 个新 artifact SHA + 13 个 RQ4 计时）、artifact 8 个 run-local 时间/摘要叶值、raw 28 events 除时间/耗时外结构相同，最终审计日志 SHA-256 `84bfc328792a692a791de738a4b2b6fac4b4a59e5ef5618aa18062954ffbe138`。论文 `validate_exposure()` 第一次从仓库根直接 import 因 `ModuleNotFoundError: v4_evidence` exit 1，验证函数未执行且不记为通过；加脚本目录 `PYTHONPATH` 后原函数最终 PASS，日志 SHA-256 `aec5817efc140e2ae69b1a03864ca54ac401f4bf5c245780d5ff79adad09c5d9`。corpus / RQ1 oracle / 历史 31,296-sample performance campaign 仍分别为 `8b87db47e51db7db42c759155c8581f95af93e766e7731a0174087ce110f2967` / `4548b9ae4ce46e056a39ff6cffafc6cd615a7d34966786f03cc59e749b0e064b` / `7c223c3520e5f059039af55435c4b29cd172145e79c44ac93da300a19a4191ad`；本轮未运行 `run-exposure-performance.sh`，不得将上述三条 in-process 曲线表述为 current-HEAD 三部署 latency/throughput campaign。凭据扫描在单一进程内覆盖既有 20 个部署 secret 名并加入严格自动发现与 runner PostgreSQL credential，去重 22 值：exact-value 子串 0 group / 0 raw / 0 decoded，URL-userinfo、PEM、secret-assignment、JSON scalar 完整相等均严格为 0；mode-0600 日志 SHA-256 `9f4b846a16ae434fc1b173c1ec1165097f44119297dfcee842e04c3dd2fc1f0b`。`git diff --check` exit 0，oracle container/network 实查 0/0；未改 registry、capability、activation、contract release 或 tag，未运行 formal/N4/canary/refreeze。PF-exposure-refresh |
| P3.3-05-diagnose run 05 finalizer 拒绝诊断 | DONE | 2026-08-10 | **原始字节边界与 raw rejection 原文：**run 05 retained sample `evaluation/final-v5-wsl2/raw/targeted-p33-artifact-100x4-05-20260810T070442Z-1efc3e616e63/raw/deployment-01.jsonl` SHA-256 `0b5376263cd6047c0ae1040dec46087750443d4ffd66ac45dc02be4d3a32d602`；`evaluation/internal/experiment/runner.go:491-510` 只在宿主进程内存收集 Adapter stderr，随后丢弃字节并只保留 suppression 文本，Compose logs 与空 `run.log` 均无副本，故 **run 05 原进程的 stderr 字节无法找回且本行不冒充找回**。按批准打法起唯一非 canary `diagnosis-p33-05-finalizer-stderr-01` 拓扑（current/run 05 同一 clean+pushed HEAD `1efc3e616e63faa7ce4eb8b2c8e85143d8f82e74`，`KEEP_UP=1`，3/3 diagnosis live gates PASS、0 SKIP，不计 P3.3），复用当轮 source-built Adapter/ProfileBinding、绕开 evidence runner 直跑同一 frozen operation，mode-0600 stderr 的可追溯 raw rejection 原文为：`artifact cell artifact/result-heavy/100x4/novel: the window contains 3 unexpected structural statement(s): [165037157799(toplevel=true)x1 8aea30c89326(toplevel=true)x1 e9ddb9460635(toplevel=true)x2]`（SHA-256 `cd409bcf811436275e202b543ca7ef7c6cb06041ada4894fc266e54c2da4b6a6`）。run 05/diagnosis 的 Adapter/Observer binary 分别同为 `052fee3e212448d73cae88bb78d595c471468b339eabc31806df68074a26ba97` / `ce260ed53e92e563df3d294dfaffd12db800f12d2153a07b8b3b6ef41a5f92e1`，两轮 build manifest 同绑 source SHA `09a848af78b9f76d70ec5d6adf2e504a45c773b091c7199e553418b5a216463a`；从两份 sample 独立派生的 `{status,error_code,prepared sha,signed target strict digests,total delta,11 个非零结构 delta}` canonical 摘要逐字节相同，SHA-256 均为 `01b5e2cf2caa9f1f6b3f078de2b508746741346f53fd0451e1753cbc40f6ae8c`，故该 raw rejection 是 run 05 同输入 gate 的可追溯精确重现，而不是旧字节回填。**具体 gate 与输入/期望：**这次不是 `RequirePreparedSame` 成员不一致；执行已越过 `evaluation/internal/experiment/reproduce_execution_v3.go:187-188` 的全 binding 比较及后续 reproduction/signed-target gates，故 prepared 差异成员列表为“不适用/空”。调用链是 `runtime_finalizer_v3.go:277` → `finalize_taskgate_v3.go:274-276` → `finalize_observation_v3.go:268-274` → `observer_snapshot_v2.go:395-403,512-519`；`ObserverWindowV2.Delta` 用 finalizer classifier 将未命中 `(strict_ast_sha256,toplevel)` 的 delta 放入 `Unexpected`，`ObservedDelta.Accept` 要求 `len(Unexpected)=0` / plan 的 `unexpected=0`，实际是 3 个结构 key、调用数合计 4。逐项两侧实际值：（1）visible classifier/signed/finalizer reproduced=`6488b1c75af81124dd4e9ea1885d0579f73e5c01b4a3981ed62cafa0158b080e`，live `pg_stat_statements`=`8aea30c8932618a1be568c4fa5e9aeedbccfaa5f810f4f1de29da6dee523b0ff`×1；（2）companion classifier/signed/finalizer reproduced=`14cf320f9266aed933badef36a93229894eab3817d29ae38a855c94c47e42f20`，live=`1650371577994476cf1195997dd59613f5146e4457e86c4ee6ddbcd8e778f0d3`×1；（3）source-controlled `ViewDefinitionAttestationSQL` classifier=`1f5549df196a68b434cc5148f6fff8fd9eb4aed88ba5d65742579f18ba4e155b`，live=`e9ddb946063549f3265b7076fa278c34848b6f999cc1d2043350b1d8b3f84e08`×2。只读 live pgss 映射证明前两项分别是真实 visible/ordinal companion，第三项是真实 `pg_get_viewdef` attestation；source `pg_query.Normalize` 对第三项编号为 `set_config($5,$6,$7)` / `format($3,$1,$2)` / final `$4`，PostgreSQL retained text 则为 `set_config($3,$4,$5)` / `format($6,$1,$2)` / final `$7`，语义相同而 ParamRef 编号/AST 不同。`StrictASTDigest` 只去位置字段并保留 ParamRef，classifier 从 source SQL 构造，observer 却再次散列 pgss 已参数化文本；现有 `internal/sqlidentity/strict_ast_live_test.go:23-113` 只覆盖 safety/representation/timeout pins，未覆盖三条 attestation 与 frozen targets。**归属：我们的 classifier/SQL identity canonicalization 与 live 覆盖缺陷**，不是被测系统性质或部署材料不匹配；认证 PostgreSQL 16.14 identity、qualification、Catalog/profile、prepared binding 与 exact 16-call window 均一致，fail-closed 拒绝本身正确。**尽可能后探：**paired-novel plan 精确期望 16 calls；除上述三 key 外，实际 BEGIN/COMMIT/safety/representation/timeout/datasource/column/internal 分别为 `1/1/1/1/2/2/2/2`，与期望逐类相同；若只把上述三真实结构身份正确绑定为 viewdef×2、visible×1、companion×1，则 `Unexpected=0`、所有 class/internal multiset/total 均闭合。`finalize_observation_v3.go:283-287` 的末道 runtime identity 也由 retained before/after 与 qualification 精确相等证明材料齐备；但 `artifact.go:125-133` 后置 artifact/crossbind splice 需要真实 `TaskGateAcceptanceV3`，当前 fail sample 没有，若合成 acceptance 就会伪造证据，若改代码才能继续则违反本任务，所以该最后边界明确 **未确认**，不能断言没有更多 gate。首次只读 pgss 查询误选 `postgres` DB，真实失败 `ERROR: relation "pg_stat_statements" does not exist`，改为 Compose 实际 Business DB 后才取得上述 25 rows；首次 secret-like scanner 把公开 `GATEWAY_DATA_KEY_ID` 当凭据而 FAIL，按 JSON path 实查其 8 个完整相等值全是公开 `key_id` metadata，修正为 19 个 credential values 后终态扫描 PASS：唯一 4 个 exact substring 全是既有公开 `GatewayRuntimeIdentityV1Version` 包含 access-key 默认字面子串，credential JSON scalar、URL userinfo、PEM、secret assignment 均 0，含 cleanup 的最终 mode-0600 scan SHA-256 `b5094df47bd5079a48ae3353421db01797c091da47da3e803f026e631c3e3e67`。**credential-free rejection taxonomy 提案（只提未实施）：**新增闭式 `TaskGateRejectionV1`，字段仅含 `version`、`phase`（receipt authentication / observer ticket / trusted material / contract identification / execution reproduction / signed consistency / schema qualification / classifier derivation / carried consistency / observer interval / closed-world accounting / runtime identity）、稳定 `gate_code`、`failure_kind`（missing/invalid/mismatch/ambiguous/unavailable）、`expected_source`/`actual_source` 枚举，可选闭式 `path_kind`/`target_role`/`statement_class`/`prepared_member`、有界 counts 与稳定排序有界 `differences[]`；difference value 只允许 `{boolean,count,lowercase_sha256,enum}` tagged union。绝不保存 raw stderr/error chain、SQL/SQLSTATE text、路径、DSN、task/request/operation 字符串、任意 map 或 raw-error hash，不写会随调查变化的 `fault_owner`；所有字符串来自编译期 enum，SHA 只能来自已类型化 credential-free durable identity，unknown fields 拒绝，resolver/parser/control cause 用 sentinel secret 单测并 fuzz serialized rejection，现有 secret scanner 仅作二道防线。evidence 位置为 Sample 顶层与 `taskgate_acceptance_v3` 并列的 `taskgate_rejection_v1`，二者互斥；finalizer-reached FAIL 必须有 rejection、PASS 禁止有，finalizer 以 typed error 携 safe record、Adapter `errors.As` 只留下 safe record，runner 继续丢 raw stderr。schema 不静默改 sample-v1：用 sample-v2 或显式 evidence-schema revision + `oneOf`，旧 evidence 原字节不动；该动作不改变 workload/acceptance/prepared-binding 语义，不移动/回填 v1.4 tag，若作者把 Sample schema 纳入 umbrella contract，只能在 v1.5 冻结时新增 taxonomy/sample-v2 SHA 并由作者批准。诊断 topology transcript/operation/stdout/stderr/exit SHA-256 分别为 `4d6aec9b936e98fad36f4d054997f9bb42f3d0e1ea35c96fb5b9005faa0dcad1` / `d6cdab77903b9481afb3cab0e53480ea4ed826112177154e26de4de29d80f97e` / `3dd7373a4cdd788e4cc69bb927c8bf8dbbe8d43b26503085063395925b2ae715` / `cd409bcf811436275e202b543ca7ef7c6cb06041ada4894fc266e54c2da4b6a6` / `9a271f2a916b0b6ee6cecb2426f0b3206ef074578be55d9bc94f6f3fe3ab86aa`，均 mode 0600；cleanup exit 0，project label/prefix containers/volumes/networks 均 `0/0/0`，cleanup log SHA-256 `18a856a942295b47d773689bff64a9c28c2155a4ed93c08b87927730f08fd2b6`。未改 production/finalizer/验收/断言/`.env`/既有 evidence/capability/registry/activation/contract，未重跑 canary、未推 tag；本诊断不是 P3.3 attempt。P3.3-05-diagnose |
