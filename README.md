# Task-bound Agent Data Gateway

Task-bound Agent Data Gateway 是一个本地演示系统：Agent 必须先申请一个有目的、有范围、有预算且有期限的数据任务，经 OA 审批后，才能查询只读数据产品。Gateway 负责确定性授权、PostgreSQL SQL AST 审计、预算扣减、结果加密和审计凭证；DeepSeek 只负责把自然语言翻译成候选 JSON，不参与最终授权。

> 当前仓库是单机 Demo，不是可直接上线的生产网关。生产差距见[威胁模型与生产化差距](docs/threat-model.md)。

## 架构概览

```text
本地 Codex / MCP Client
          │ Bearer Token + MCP
          ▼
 Gateway :8080 ──────► DeepSeek API（仅问题与脱敏逻辑目录）
    │    │
    │    ├───────────► OA Demo :8090（草稿、提交、审批、签名回调）
    │    │
    │    ├───────────► PostgreSQL 16（gateway_reader，只读 Reporting Views）
    │    │
    │    └───────────► SQLite /data/gateway.db（任务、预算、AES-GCM 结果、审计链）
    │
    └─ Alice：申请与查询自己的任务；Carol：只读审计，不读取原始结果
```

核心安全边界与数据流见[一页式架构与安全边界](docs/architecture.md)。

## 快速启动

宿主机需要 Docker Engine 与 Docker Compose v2；构建、格式检查和 Go 测试都在容器内完成，不要求宿主机安装 Go。

```bash
cp .env.example .env
```

编辑 `.env`：至少生成并填写一个稳定的 32 字节 Base64 数据密钥，替换所有 Demo Token、共享密钥和密码；要运行自然语言申请与查询，还需填写有效的 `DEEPSEEK_API_KEY`。

```bash
# 若宿主机有 OpenSSL：
openssl rand -base64 32
# 或者只使用 Docker：
docker run --rm --entrypoint sh postgres:16-bookworm -c 'head -c 32 /dev/urandom | base64'
docker compose up --build -d --wait
docker compose ps
curl -i http://127.0.0.1:8080/health/ready
curl -i http://127.0.0.1:8090/health/ready
```

两个健康检查成功时返回 `204 No Content`。服务只发布到本机回环地址：

- Gateway / MCP：`http://127.0.0.1:8080/mcp`
- OA Demo：`http://127.0.0.1:8090/login`

仓库已提供 [`.codex/config.toml`](.codex/config.toml)。启动 Alice 的 Codex 会话：

```bash
export TASKBOUND_GATEWAY_TOKEN='<与 .env 中 TASKBOUND_ALICE_TOKEN 相同的值>'
codex
```

完整的 Alice 自动审批、Bob 人工审批和 Carol 审计流程见[Compose 启动与 Codex MCP 演示](docs/getting-started.md)。

## Demo 身份

| 身份 | 入口 | 权限 | 凭据来源 |
|---|---|---|---|
| Alice | MCP | 申请任务、查询自己的 ACTIVE 任务、读取自己的结果与凭证 | `TASKBOUND_ALICE_TOKEN` |
| Carol | MCP | 列出审计事件、读取查询凭证；不能读取原始查询结果 | `TASKBOUND_CAROL_TOKEN` |
| Alice | OA | 提交自己的 OA 草稿 | 用户名 `alice`，密码 `OA_ALICE_PASSWORD` |
| Bob | OA | 审批分配给 Bob 的人工审批任务 | 用户名 `bob`，密码 `OA_BOB_PASSWORD` |

## 数据产品与默认策略

| 逻辑产品 | 内容 | 有效敏感级别 | 强制范围 | 审批 | 默认预算 |
|---|---|---|---|---|---|
| `expense_summary` | 月份、部门、费用类型汇总 | `low` | `department` | Alice 提交后自动批准 | 10 次、500 行、30 秒 DB 时间、单次 5 秒、TTL 30 分钟 |
| `expense_detail` | 员工级报销明细 | `high` | `department` | Bob 人工批准或拒绝 | 5 次、100 行、15 秒 DB 时间、单次 5 秒、TTL 15 分钟 |

Agent SQL 只引用逻辑产品名和获批字段，例如：

```sql
SELECT month, sum(total_amount) AS amount
FROM expense_summary
GROUP BY month
ORDER BY month
```

不得引用 `reporting.*`、`legacy.*` 或系统目录；Gateway 会在服务端注入强制范围并加上剩余行预算限制。

## 验证与运维

```bash
make verify
make logs
docker compose down
```

`make verify` 在 Docker 中执行格式检查、`go vet`、race 单元测试、镜像构建和隔离的 Compose 端到端验收：官方 Go MCP Client、Alice 自动审批、Bob 批准/拒绝、预算耗尽、重启恢复、身份隔离、数据库权限及模型停机降级均会被实际验证。测试使用本地确定性模型替身，不调用外部 DeepSeek，并在成功或失败后删除专用测试 Volume。`docker compose down` 保留普通开发栈的 `gateway-data` 与 `pg-data`；`docker compose down --volumes` 会永久删除 Demo 数据。

## 文档

- [一页式架构与安全边界](docs/architecture.md)
- [Compose 启动与 Codex MCP 演示](docs/getting-started.md)
- [Catalog 编写指南](docs/catalog-guide.md)
- [OA 与数据源适配器接口](docs/adapters.md)
- [SQL 安全规则与已知限制](docs/sql-security.md)
- [威胁模型与生产化差距](docs/threat-model.md)

## 明确不做

本项目不建设 OA、BI 或数据仓库，不复制生产数据库，不安装 PostgreSQL 扩展，不管理 Agent Skill，也不渲染图表。它返回结构化数据与审计证据，由 Codex 及其 Skill 负责分析和呈现。
