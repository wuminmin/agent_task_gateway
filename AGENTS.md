# AGENTS.md — 项目事实

TaskGate：agentic 数据库系统的累计数据暴露记账与控制原型。全部工作服务于 TKDE 投稿。
**这不是产品代码，是论文证据链**：一段无法追溯到真实运行的证据，比没有证据更糟。
本文件是**项目事实与 Codex 作业纪律的唯一入口**。`CLAUDE.md` 不再是本项目的规划、
审计、派工或复核前置；与本文件冲突时，以本文件、作者最新明确指令和可复核证据为准。
2026-08-22 的作者指令只替换协作与执行分工，不自动重开任何已封存研究决策，
也不转移作者专属的逐字节批准与 tag 权限。

## 职责与裁决边界

作者（wuminmin）保留研究主张、运行时行为变更、协议/冻结字节、证据采用与投稿口径的最终
裁决权，并独占逐字节批准、tag、外部账号授权和最终投稿确认。除此之外，**Codex 接手全部
工作**：直接负责规划、审计、任务拆分与派工、代码与文档、测试与部署、证据留存、论文与
投稿材料、提交与分支推送、进度汇报和后续任务选择。作者直接与 Codex 对话；不再等待
Claude 任务书、Claude 复核或 Claude 转述才开工、继续或交付。

Codex 的纪律：

1. **不自行扩大范围**：任务书没写的不顺手做；该做但不在范围的写进台账"遗留"上报。
   没有外部任务书时，Codex 依据权威队列、最新台账和证据自行写出当前任务的起始 HEAD、
   目标、范围、停手判据、验收命令与提交格式，再执行；这不构成扩大范围。
2. **自报"通过"不作数**：台账留可复核证据（命令 + 关键输出摘要），不是结论词；
   需要作者裁决的岔口直接向作者列出证据、选项和影响，不自行代签或决定。
3. **catm 直发允许且欢迎（2026-08-17 作者定，取代 54cc9c0 的一日禁令）**：作者要求
   多渠道掌握进度。Codex 开工、重大阶段后、验证后、完成前及连续工作约每五分钟调用
   `sync_session`，有用进展/警告用 `notify_author`，最终回复先原样交
   `notify_work_completed`。CATM 只传通知、不收裁决；裁决仍在当前作者对话中取得。
   `sync_session` 不等于手机通知：派工、实证结论、失败、路线变化、等待裁决等里程碑即时
   `notify_author`，连续工作至少每 30 分钟发送一次带当前工作、数字事实和作者动作需求的
   简短心跳。通知、对话与台账不一致时一律以原始证据为准，并由 Codex 在台账追加校正行。
4. **主动派工但责任不外包**：可把互不冲突的只读审计、实现或验证交给子 agent 并行；
   Codex 主 agent 必须核对其结果、保护共享工作树、汇总证据并对最终提交负责。

## Codex loop 模式（默认持续到论文完成）

本项目默认运行在持续 loop，而不是“一项任务做完即等待下一张任务书”。Codex 将
“完成 TaskGate TKDE 论文”保持为持续目标；每个任务/提交只是 checkpoint。一次循环为：

1. **恢复事实**：每次新会话或上下文恢复先读权威文档、§10 台账最后五行和当前 handoff，
   实测 Git/远端/Docker/残留状态；后写台账和当前 HEAD 永远覆盖旧 handoff 的状态描述。
2. **选下一项**：沿唯一权威队列的关键路径选择当前最高优先级、未完成且不受阻的工作；
   先写清任务 ID、起始 HEAD、证据输入、范围、红线、停手判据和分层验收。
3. **静态审计先行**：所有昂贵活体前先确认对应零部署路由/身份/闭包审计与聚焦验证已经
   完成；未完成才先审计/实现，已由最新证据闭环的不得无故重做。然后才运行已获授权的
   活体，实跑期间冻结其依赖文件。
4. **证据闭环**：逐项记录命令、退出状态和带数字的关键输出；按规则更新 append-only
   台账，完成一个任务一次提交并推送分支。超过几分钟的命令放入可监控的后台/PTTY，
   持续观察并经 CATM 汇报，不因等待而结束 loop。
5. **立即续跑**：提交和推送后重新读取最新台账、选择下一项并继续，不把普通阶段完成、
   长命令等待、上下文压缩或会话重启当作停手理由。一个分支等待作者时，先推进不依赖
   该裁决且不会污染其证据的其它已授权工作。

持续性必须靠机制而不是散文承诺。当前 Codex 的主机制是 durable `/goal`（或宿主暴露的
等价 goal API）：它把目标保持在同一线程中跨回合推进，可查看、暂停、恢复或清除。新接手
会话必须创建或恢复该 goal；AGENTS/handoff 只提供约束与恢复上下文，不能代替激活 goal。
标准目标词：

```text
/goal 完成 TaskGate TKDE 论文并持续工作，直到 docs/codex_publication_execution_plan.md §1 的 Paper 完成定义和 AGENTS.md 的可验证停止条件全部满足；从最新台账与 handoff 恢复，按 checkpoint 留证、提交、推送并立即续跑，严格遵守作者专属裁决、红线及 measurement 不重试规则。
```

用 `/goal` 查看状态，用 `/goal pause`、`/goal resume`、`/goal clear` 控制生命周期。若宿主
另有 `/loop` 或 scheduled-task 周期唤醒，可把它作为检查后台任务与 CATM 心跳的兼容外壳，
不得用它替代 durable goal 或扩大任何实验重试额度。每次实际恢复仍须重读现场；机制名称
不同不改变本节的证据、停手与续跑规则。

这里的 loop 是**工作推进循环**，不改变实验判定：measurement 后失败仍立即保留并记 fail，
不得重试到过、改门槛或选择性丢弃；pre-measurement 重启也只能按成文额度与条件执行。

只在以下情况暂停 loop 并直接请求作者：必须取得新的研究/运行时/主张裁决；需要作者逐字节
批准、tag、外部账号或最终投稿确认；继续会触红线；环境故障在安全自恢复后仍失败；所有
未完成路径都被同一个外部条件真实阻塞；或作者明确叫停。作者回复后从同一证据点恢复。
只有 `docs/codex_publication_execution_plan.md` §1 的 Paper 完成定义全部满足、所需作者
动作已完成且投稿包/提交状态有证据可复核时，才把持续目标记为完成；不得因 token、时间、
单项任务完成或一次失败提前宣布论文完成。

## 当前恢复入口

本轮接手的实现锚是 `8a660e8`，bootstrap handoff 是 `docs/handoff_2026-08-21.md` 所在提交
`16f5381`；两者之间只有该交接文档。handoff 记录：P69 插桩代码已完成，人工关机中止的
`p69-callback-phase-diagnosis-04` 不得作为证据，下一步应从**当前 clean 且已推送的 HEAD**
以新目录 `p69-callback-phase-diagnosis-05` 完整重跑已授权诊断段。

交接文档中的 `HEAD=origin=8a660e8` 是 P69 已完成插桩的 implementation anchor，不是治理
提交后的现行 exact-HEAD 门。P69 起跑前必须同时证明：工作树干净、
`HEAD=origin/tkde-artifact-rerun`、`8a660e8` 是当前 HEAD 的祖先，并逐项复核
`git diff --name-status 8a660e8..HEAD` 只含 `docs/handoff_2026-08-21.md` 与本文件的
docs/governance 变化。若出现任何 runtime、evaluation、config、launcher/build 或 retained
evidence 字节变化，立即停手复核，不能把 docs-only 例外扩成源码漂移许可。formal build
和证据必须记录实际 HEAD，不得把 `8a660e8` 伪报为当前提交。

P69 产出 `stuck_phase` 后，修复本身仍未授权：必须直接向作者提交定点修复选项，禁止顺手
修改 `internal/` 或启动 formal campaign。当前只授权 `-05` 这一完整 replacement
deployment；measurement 后失败绝不追加部署，pre-measurement 也只可使用 handoff 明定的
一次追加额度。仍须按 loop 第 1 步核对最新台账和现场；一旦出现更新的台账/交接记录，
本段只作历史入口，不得覆盖更新事实。

## 权威文档（开工必读，按此顺序）

1. `docs/codex_publication_execution_plan.md` — **唯一权威任务队列**。§2 红线完整版、
   §3 已封存决策、§8 会话协议、§10 进度台账。**开工先读台账最后五行**；台账
   append-only，后行对前行做口径校正，**以最新一行为准**（照旧行做会去等不需要的输入）。
2. `docs/final_v5_author_decisions.md` — 作者决策记录，**不得重开已裁决项**
   （不写条数：计数会漂移，2026-08-16 实查已 26 条而散文仍写 19）。
3. `dev.md` — 系统拓扑与接口。
4. 最新 `docs/handoff_*.md` — 仅作会话恢复线索；必须和最新台账、Git 与现场实测交叉核对，
   冲突时不得用 handoff 覆盖更新证据。

## 环境

工作树 `/home/wmm/worktrees/agent_task_gateway`，分支 `tkde-artifact-rerun`。
主工作树 `/home/wmm/agent-scope/task_gateway` 停在 `main`，**永不触碰**。

```bash
export GOFLAGS=-buildvcs=false
docker version                                  # WSL 原生 dockerd（systemd）；每次仍实测
./scripts/db-test-env.sh up
./scripts/db-test-env.sh verify                 # 期望 server_version_num=160014
gofmt -l $(git ls-files '*.go'); go build ./...; go vet ./...
./scripts/db-test-env.sh test -count=1 ./...    # 带库全量的唯一支持写法（只在门禁点跑）
env -u TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN ./evaluation/final-v5-wsl2/scripts/validate.sh
./evaluation/final-v5-wsl2/scripts/run-sql-executability-gate.sh  # 真执行 SQL 门禁（自建一次性空库）
```

Docker Desktop 已于 2026-08-18 卸载；当前 Docker 是本 WSL 发行版内由 systemd 管理的
原生 `dockerd`，数据根目录为 `/var/lib/docker`。此前“Docker 反复上下线”是 Docker
Desktop 历史环境的现象，不再描述当前运行方式；但开工仍逐次实测 daemon 状态，不能假设。

### 测试分层（2026-08-16 定，取代「每任务一次全量」）

| 层 | 触发时机 | 跑什么 |
| --- | --- | --- |
| 每个任务 | 每次代码改动 | 受影响的包 + 聚焦测试，`-count=1` |
| 门禁 | 契约冻结、翻 capability、正式 campaign、打 tag 之前 | 完整带库全量一次 |

受影响包 = 改动的包 + 直接依赖它的包；拿不准往宽跑，但不退回全量。依据是实测量级差
（`internal/gateway` 单包 3630s ≈ 全量墙钟 89%，典型改动受影响包约差 40 倍；
全部数字见台账 DOC-dbtest-recipe 行）。

### 带库测试与运行的坑（每条都实翻过车，出处在台账）

- 全量必须走 `test` 子命令，**不得** `eval "$(./scripts/db-test-env.sh env)"` 后裸跑
  `go test`：`env` 导出 SQLCHECK 管理 DSN、裸跑用 10 分钟默认预算，稳定制造三类假失败
  （probe 撞已有 schema、`internal/gateway` timeout panic、validate.sh 门禁跑进被污染的
  business 库）。`env`/`test` 的不对称是设计，不是疏漏；`validate.sh` 必须在没有该
  变量的环境里跑，真执行 SQL 门禁走 `run-sql-executability-gate.sh`。
- `internal/gateway` 实测 2777s，`test` 默认 `-timeout=60m` 余量约 23%——将来超时
  **先怀疑预算，不要先判 hang**。
- 不接 DSN 时 DB 测试**静默 skip，skip 不算 pass**（红线 7）。
- **不得安装宿主机 PostgreSQL**：digest-pinned 的 16.14 容器就是整套记账被认证的环境。
- **跑活体（canary / N4 / campaign）前必须 `./scripts/db-test-env.sh down`**：
  与测试环境争同一批宿主端口；runner 自检只报告占用者、不代劳删除。
- **活体轮昂贵（一轮 fresh 部署几十分钟），静态审计先行（2026-08-17 作者定）**：
  凡 runner/工具对 profile 路由策略的隐含假设，先零部署静态审计一次修齐，
  再烧活体轮——隐含假设一轮只暴露一个（P26 closure、P27 max_queries，
  各烧一轮，见台账）。派工任务书必须显式包含静态审计阶段。
- **改了 `internal/` 的实跑必须带 `TASKGATE_REAL_PILOT_BUILD=1`**：Compose 镜像不自动
  重建，不带等于跑旧代码，而运行照样成功照样出数字——假结论比没数据更危险
  （台账 P15-image-staleness）。pilot 类运行现不记录镜像 ID，此缺口未补前尤其要小心。
- **实跑期间不得修改该运行依赖的任何文件**（脚本被读到半截会中止运行）。
- **创建任何具名产物（Product、视图、schema）之前先全仓 grep 这个名字**——
  已有方案或视图链可能早就存在（台账 P17 记过同根因两次）。
- 2026-08-19 复核，全仓 `gofmt -l` 仍只报
  `internal/control/execution_binding.go` 这一项既有失败项，
  **不要顺手修**；
  只需确认本轮改动的 Go 文件无输出。

### 论文构建（2026-08-16 实测定）

| 用途 | 用哪个 |
| --- | --- |
| 迭代、排版预判（页数、溢出、断行） | 宿主 `paper/tkde/compile.sh` |
| **交付字节、留证、打 tag** | **容器 `make paper-tkde`** |

实测同一提交两边页数一致（main 12 页 + supplement 12 页）但 PDF 字节必不同
（宿主 pdfTeX 1.40.29 vs 容器 1.40.24）。交付必须出自受控可复现环境；
**卡页数边界的改动以容器页数为准**，宿主只作快速预判。

## 红线（违反则此前工作作废，完整版见计划 §2）

1. 不得伪造、复制、回填任何证据字节；不得给旧证据重打新 contract release 标签。
2. v1.4 活引用集合非空前，禁止 formal build、N4、100×4 canary、v1.5 冻结、
   任何能力从 false 翻 true。
3. 不得移动 tag（`final-v5-contracts-v1…v1.4` 归作者）；只推分支。
   **作者批准的冻结字节只有作者能改**：发现冻结文件漂移，先恢复原字节，
   再由作者批准后单独重新冻结并更新 approved SHA。
4. v3 验收失败即 `fail`，**不得**加 v1.4 回退。
5. finalizer 的资源实现落在 `evaluation/internal/experiment`，
   绝不落在被测方包 `evaluation/cmd/final-v5-adapter`。
6. **关键推导只允许一份实现**（准备语句只在 `internal/physicalquery`）；
   第二份会漂移，而漂移会被当成测量结果读。
   推论：**oracle 必须保持 pre-run、evaluation-only 的固定契约模型——不解析任意 SQL、
   不 Prepare statement、不调用 `Derive`**；一旦越界它就是被禁的第二份实现，
   "独立 oracle"的论证随之失效。**读取生产输出用于比对是允许的，
   把生产输出当作 oracle 的来源不允许。**
7. skip、SKIPPED、未运行，一律不记为通过。
8. 改运行时行为的决策必须**先于** v1.5 冻结与任何 P5 measurement 落地。
   冻结后再落地等于作废整轮 campaign；决策前模型下测得的样本一律不得沿用。
9. 测量所用的 formal image 与 N4 footprint 认证都**绑树**。被测树一旦推进，
   旧 image 与旧认证不得冒充新树的证据；P5 正式 campaign 必须从冻结后的 HEAD
   重建并按需重认证。
10. **不得为了方便测量而修改被测工具**。没有测量入口时，如实记 BLOCKED，
    不要用 micro-benchmark 冒充端到端实测，也不要临时给 runner 造一个入口。

## 当前必须知道的状态（会致命出错的几处）

- **StrictAST 已是 schema v2**（窄 ParamRef 规范化）。整个摘要空间已迁移，
  **唯一合法的 N4 配对输入是 `qualification-p33-01` / `-02`**；
  旧 `qualification-p32-01` 是 schema-v1 的历史证据，**绝不可作为配对输入**。
  新旧 digest space 混用不会报错，只会静默产出错误结论。
- **Sample schema 有三代并存**：v1（早期 run）、v2（rejection-only）、v3（当前）。
  旧字节一律不动、按各自版本读取，永久兼容回归必须原样通过；
  新增能力走显式新版本，**不得静默重解释旧样本**。
- **finalizer 拒绝必须携带 `TaskGateRejectionV1`**：phase / gate_code / failure_kind /
  source / path_kind / target_role / statement_class / difference_kind / difference_field
  九张闭式枚举表，拒绝 unknown 值；与 `taskgate_acceptance_v3` 互斥。
  （不写具体条目数：计数会漂移；要遵守的属性是"闭式 + 拒绝 unknown"。）
  **已知覆盖空洞**：pre-finalizer 的 phase-1 启动失败没有 sample 当载体，
  taxonomy 不适用——这类失败仍需人读日志。

## 反复踩过的那个坑

已修的四个缺陷是同一个病根：**散列了未规范化 / 未独立的形式**。
observer census 混用 `text` 与未 cast 的 `numeric`；predicate footprint 散列原始
`MandatoryScope` 字节；`StrictASTDigest` 保留 ParamRef 编号；Dataset identity 与 probe
被写成同一个 digest 再校验相等（等于用自己验自己）。

**每加一个新身份，先问一遍：我散列的是规范形式吗？它是独立的吗？**

## 汇报纪律

跑过并通过才写"通过"；失败贴失败输出；跳过明说跳过。与既有文档冲突时以**证据**为准，
冲突写进台账，不静默改文档。触到红线立即停下上报，不绕过。

**pre-measurement failure 不是 cell failure，必须分开记。** runner、构建、部署、binding
生成阶段的失败属于前者：允许最小修复、单独提交、从新的 clean 已推送 HEAD **完整重启**，
台账单独记一行，写清 `failure_stage` 与 `formal_gateway_built` / `live_gates_run` /
`measurement_started` / `measured_samples` 的真实值，不得记作 canary 或 live gate 的
PASS/FAIL。measurement 开始之后的 cell 失败属于后者：**即 fail，不重试到过、不改判定
条件、不降级**，保留失败目录与日志后停下上报。**只有真正到达 v3 acceptance 并被判否，
才准表述为"未通过验收"**；未到达就中止的，一律表述为"在 X 阶段中止，未产生验收判定"。

**凭据门禁口径**：exact-value 子串命中允许非零，但每一个命中都必须在台账里被证明为
source-controlled 公开常量的子串（给出常量、文件:行号、命中字段逐字节等于该常量的
证明）；URL-userinfo / PEM / secret-assignment / JSON-scalar 完整相等 四类必须严格为 0。

## 提交

一个任务一次提交，首行 `<type>(<scope>): <祈使句>`，正文写清**改了什么行为、为什么、
验证了什么**，末行注明任务 ID（如 `P4.0-C1`）。完成验证后
`git push origin tkde-artifact-rerun`。**永不推 tag。**一次 loop 可以连续完成多个任务/
提交；每次推送是可恢复 checkpoint，不是默认停手点。
