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
**P3.3 Result-heavy 100×4 v3 canary**（活体）：这是 v3 路径第一次真跑量。
失败即 `fail`，不降级。

验收：三项各自的证据文件入库（`raw/` 是 `.gitignore *`，值得留的用 `git add -f`，
入库前必须查凭据）。

---

### P4 — 契约 v1.5 冻结（前置：P3；需作者批准）

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
| P2.6b 按 P1b.2 完整重跑 exposure | TODO | 2026-08-09 | 下一且唯一可执行任务：必须先将 P2.6a 作为独立 commit 推送，使工作树 clean，再从该准确 HEAD 在认证 PostgreSQL 16.14 digest 上完整运行 `make eval-exposure`；只允许真实 runner 的事务发布路径刷新合法 evidence，失败不得污染/复制/回填 canonical evidence。须按 P1b.2 全标准验收 build-stage、RQ1/RQ2、rewrite、RQ3 五个 named live tests与两个 package terminal PASS、RQ4 三曲线、两级 SHA、credential scan及容器/网络清理；任何 SKIP/未运行不算通过。完成前不得进入 P2.7。 |
