# AGENTS.md

TaskGate：agentic 数据库系统的累计数据暴露记账与控制原型。当前全部工作服务于一个
目标——TKDE 投稿。**这不是产品代码，是论文证据链**：一段无法追溯到真实运行的证据，
比没有证据更糟。

## 分工

作者（wuminmin）保留全部裁决权与 tag。规划、审计、任务下发由 Claude 出；代码、运行、
提交由 Codex 执行。**Codex 不自行扩大任务范围**：任务书没写的改动不要顺手做；发现该做
但不在范围内的事，写进台账"遗留"并上报。遇到需要作者裁决的岔口，停下把选项列清楚，
不要自行决定。

## 权威文档（开工必读，按此顺序）

1. `docs/codex_publication_execution_plan.md` — **唯一权威任务队列**。
   §2 红线、§3 已封存决策、§8 会话协议、§10 进度台账。
   开工先读台账**最后五行**，接着做，不要重开已完成任务。
   台账是 append-only，**后面的行会对前面的行做"口径校正"，以最新一行为准**。
   例：P3.3 曾有一行 BLOCKED 写"唯一阻塞是作者提供完整 publication binding"，
   已被后续 C16 与 P3.3-prep2 推翻——照旧行做就会去等一个不需要的输入。
2. `docs/final_v5_author_decisions.md` — 作者决策 1–19。**不得重开已裁决项**。
3. `dev.md` — 系统拓扑与接口。

## 环境

工作树 `/home/wmm/worktrees/taskgate-artifact-rerun`，分支 `tkde-artifact-rerun`。
主工作树 `/home/wmm/agent-scope/task_gateway` 停在 `main`，**永不触碰**。

```bash
export GOFLAGS=-buildvcs=false
docker version                                  # Docker 反复上下线，每次实测，不要假设
./scripts/db-test-env.sh up && eval "$(./scripts/db-test-env.sh env)"
./scripts/db-test-env.sh verify                 # 期望 server_version_num=160014
gofmt -l $(git ls-files '*.go'); go build ./...; go vet ./...; go test -count=1 ./...
./evaluation/final-v5-wsl2/scripts/validate.sh
```

不接 DSN 时 DB 测试**静默 skip**，而 skip 不算 pass。不得安装宿主机 PostgreSQL：
digest-pinned 的 PostgreSQL 16.14 容器对就是整套记账被认证against的那个环境。
论文构建走容器（`make paper-tkde`）：论文字节必须与 evidence 出自同一受控构建环境，
**不要绕过它在宿主直接编译**。宿主已装用户级 TeX Live 2026，`kpsewhich` 能找到
`IEEEtran.cls` 与 `IEEEtran.bst`，缺的只是 Debian 包 `texlive-publishers`——
所以"宿主编译不了"已不是理由，理由是构建环境一致性。

**跑活体（canary / N4 / campaign）之前必须先 `./scripts/db-test-env.sh down`**：
带库测试环境与活体部署争同一批宿主端口，残留会让 phase 1 直接起不来。
活体 runner 现有开跑前自检会 fail fast 并报出占用者，但它**只报告、不代劳**，
不会替你删任何容器。

全仓 `gofmt -l` 会稳定报告 `internal/control/execution_binding.go`，这是 P0.2 已登记的
既有失败项，**不要顺手修**；只需确认本轮改动的 Go 文件无输出。

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
- **finalizer 拒绝必须携带 `TaskGateRejectionV1`**（12 phases / 54 个闭式 gate code），
  与 `taskgate_acceptance_v3` 互斥。**已知覆盖空洞**：pre-finalizer 的
  phase-1 启动失败没有 sample 当载体，taxonomy 不适用——这类失败仍需人读日志。

## 反复踩过的那个坑

已修的四个缺陷是同一个病根：**散列了未规范化 / 未独立的形式**。
observer census 混用 `text` 与未 cast 的 `numeric`；predicate footprint 散列原始
`MandatoryScope` 字节；`StrictASTDigest` 保留 ParamRef 编号；Dataset identity 与 probe
被写成同一个 digest 再校验相等（等于用自己验自己）。

**每加一个新身份，先问一遍：我散列的是规范形式吗？它是独立的吗？**

## 汇报纪律

跑过并通过才写"通过"；失败就贴失败输出；跳过的明说跳过。与既有文档冲突时以**证据**
为准，把冲突写进台账，不要静默改文档。触到红线立即停下上报，不要绕过。

**pre-measurement failure 不是 cell failure，二者必须分开记。** runner、构建、部署、
binding 生成阶段的失败属于前者：允许最小修复、单独提交、从新的 clean 已推送 HEAD
**完整重启**，台账单独记一行，写清 `failure_stage` 与 `formal_gateway_built` /
`live_gates_run` / `measurement_started` / `measured_samples` 的真实值，并注明不得记作
canary 或 live gate 的 PASS/FAIL。measurement 开始之后的 cell 失败属于后者：
**即 fail，不重试到过、不改判定条件、不降级**，保留失败目录与日志后停下上报。
**只有真正到达 v3 acceptance 并被判否，才准表述为"未通过验收"**；未到达就中止的，
一律表述为"在 X 阶段中止，未产生验收判定"。

**凭据门禁口径**：exact-value 子串命中允许非零，但**每一个命中都必须在台账里被证明为
source-controlled 公开常量的子串**（给出常量、文件:行号、命中字段逐字节等于该常量的证明）；
URL-userinfo / PEM / secret-assignment / JSON-scalar 完整相等 四类必须严格为 0。

## 提交

一个任务一次提交，首行 `<type>(<scope>): <祈使句>`，正文写清**改了什么行为、为什么、
验证了什么**，末行注明任务 ID（如 `P4.0-C1`）。完成验证后
`git push origin tkde-artifact-rerun`。**永不推 tag。**
