# AGENTS.md

TaskGate：agentic 数据库系统的累计数据暴露记账与控制原型。当前全部工作服务于一个
目标——TKDE 投稿。**这不是产品代码，是论文证据链**：一段无法追溯到真实运行的证据，
比没有证据更糟。

## 权威文档（开工必读，按此顺序）

1. `docs/codex_publication_execution_plan.md` — **唯一权威任务队列**。
   §2 红线、§3 已封存决策、§8 会话协议、§10 进度台账。
   开工先读台账最后三行，接着做，不要重开已完成任务。
2. `docs/final_v5_author_decisions.md` — 作者决策 1–17。**不得重开已裁决项**。
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
论文只能容器内构建（宿主缺 `texlive-publishers`）。

## 红线（违反则此前工作作废，完整版见计划 §2）

1. 不得伪造、复制、回填任何证据字节；不得给旧证据重打新 contract release 标签。
2. v1.4 活引用集合非空前，禁止 formal build、N4、100×4 canary、v1.5 冻结、
   任何能力从 false 翻 true。
3. 不得移动 tag（`final-v5-contracts-v1…v1.4` 归作者）；只推分支。
4. v3 验收失败即 `fail`，**不得**加 v1.4 回退。
5. finalizer 的资源实现落在 `evaluation/internal/experiment`，
   绝不落在被测方包 `evaluation/cmd/final-v5-adapter`。
6. 关键推导只允许一份实现（准备语句只在 `internal/physicalquery`）；
   第二份会漂移，而漂移会被当成测量结果读。
7. skip、SKIPPED、未运行，一律不记为通过。

## 汇报纪律

跑过并通过才写"通过"；失败就贴失败输出；跳过的明说跳过。与既有文档冲突时以**证据**
为准，把冲突写进台账，不要静默改文档。触到红线立即停下上报，不要绕过。

## 提交

一个任务一次提交，首行 `<type>(<scope>): <祈使句>`，正文写清**改了什么行为、为什么、
验证了什么**，末行注明任务 ID（如 `P1a.1`）。完成验证后
`git push origin tkde-artifact-rerun`。**永不推 tag。**
