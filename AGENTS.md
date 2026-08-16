# AGENTS.md — 项目事实

TaskGate：agentic 数据库系统的累计数据暴露记账与控制原型。全部工作服务于 TKDE 投稿。
**这不是产品代码，是论文证据链**：一段无法追溯到真实运行的证据，比没有证据更糟。
本文件是**项目事实的唯一出处**；Claude 的作业纪律（第一原则、决策权、工作循环、心跳）
在 `CLAUDE.md`，互不复述。

## 分工

作者（wuminmin）保留全部裁决权与 tag；规划、审计、派工由 Claude 出；代码、运行、提交由
Codex 执行。作者只与 Claude 对话，Claude 驱动 Codex（2026-08-13 作者定）。Codex 两条纪律：

1. **不自行扩大范围**：任务书没写的不顺手做；该做但不在范围的写进台账"遗留"上报。
2. **自报"通过"不作数**：台账留可复核证据（命令 + 关键输出摘要），不是结论词；
   需要裁决的岔口停下列选项，不自行决定。

驱动方式：`codex exec -C <worktree> -s danger-full-access "<任务书>"`。任务书自带起始
HEAD、环境命令、范围边界、停手判据、验收命令与提交格式。预计超过几分钟的任务一律
后台执行，且任务书要求 Codex **每完成一个验收命令就打一行带数字的结论行**，
使 Claude 在跑完前就能取到可汇报的事实。

## 权威文档（开工必读，按此顺序）

1. `docs/codex_publication_execution_plan.md` — **唯一权威任务队列**。§2 红线完整版、
   §3 已封存决策、§8 会话协议、§10 进度台账。**开工先读台账最后五行**；台账
   append-only，后行对前行做口径校正，**以最新一行为准**（照旧行做会去等不需要的输入）。
2. `docs/final_v5_author_decisions.md` — 作者决策 1–19，**不得重开已裁决项**。
3. `dev.md` — 系统拓扑与接口。

## 环境

工作树 `/home/wmm/worktrees/agent_task_gateway`，分支 `tkde-artifact-rerun`。
主工作树 `/home/wmm/agent-scope/task_gateway` 停在 `main`，**永不触碰**。

```bash
export GOFLAGS=-buildvcs=false
docker version                                  # Docker 反复上下线，每次实测，不要假设
./scripts/db-test-env.sh up
./scripts/db-test-env.sh verify                 # 期望 server_version_num=160014
gofmt -l $(git ls-files '*.go'); go build ./...; go vet ./...
./scripts/db-test-env.sh test -count=1 ./...    # 带库全量的唯一支持写法（只在门禁点跑）
env -u TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN ./evaluation/final-v5-wsl2/scripts/validate.sh
./evaluation/final-v5-wsl2/scripts/run-sql-executability-gate.sh  # 真执行 SQL 门禁（自建一次性空库）
```

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
- **改了 `internal/` 的实跑必须带 `TASKGATE_REAL_PILOT_BUILD=1`**：Compose 镜像不自动
  重建，不带等于跑旧代码，而运行照样成功照样出数字——假结论比没数据更危险
  （台账 P15-image-staleness）。pilot 类运行现不记录镜像 ID，此缺口未补前尤其要小心。
- **实跑期间不得修改该运行依赖的任何文件**（脚本被读到半截会中止运行）。
- **创建任何具名产物（Product、视图、schema）之前先全仓 grep 这个名字**——
  已有方案或视图链可能早就存在（台账 P17 记过同根因两次）。
- 全仓 `gofmt -l` 稳定报 `internal/control/execution_binding.go` 与
  `internal/exposure/factset_differential_test.go` 两个既有失败项，**不要顺手修**；
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
`git push origin tkde-artifact-rerun`。**永不推 tag。**
