# TaskGate

Task-bound Agent Data Gateway 是一个本地演示系统：Agent 必须先提交明确的数据产品、字段、Scope、预算和目的，经 OA 审批后，才能查询只读数据产品。Gateway 不包含模型层；它只接受结构化任务申请、声明式 `QueryPlan` 或 SQL，并以确定性策略完成授权、PostgreSQL AST 校验、预算扣减、结果加密和审计。

> 当前仓库是单实例 Demo，不是可直接上线的生产网关。生产差距见[威胁模型与生产化差距](docs/threat-model.md)。

## 架构概览

```text
本地 Codex / MCP Client
          │ Bearer Token + MCP 2.0
          ▼
 Gateway :8082 ─────────► OA Demo :8092（草稿、审批、HMAC 回调 + Ed25519 回执）
    │
    ├───────────────────► Control PostgreSQL :5432（宿主机 127.0.0.1:25433）
    │                       任务、Grant、预算、AES-GCM 结果、审计链
    │
    └───────────────────► Business PostgreSQL :5432（仅内部网络，默认不发布宿主机端口）
                            gateway_reader 只读 Attestation + Reporting Views
```

两个 PostgreSQL 使用独立容器、账号和 Volume。Gateway 仍按单实例部署；数据库行锁保证请求并发安全，本版本不提供多 Gateway 租约协议。

## 快速启动

宿主机只需要 Docker Engine 与 Docker Compose v2：

```bash
cp .env.example .env
# 按 docs/getting-started.md 生成加密密钥和两个独立 Ed25519 密钥
# 同时替换 .env 中的全部 Token、共享密钥和数据库密码
docker compose up --build -d --wait
docker compose ps
curl -i http://127.0.0.1:8082/health/ready
curl -i http://127.0.0.1:8092/health/ready
```

本机入口：

| 服务 | 地址 |
|---|---|
| Gateway / MCP | `http://127.0.0.1:8082/mcp` |
| OA Demo | `http://127.0.0.1:8092/login` |
| 系统控制库 | `127.0.0.1:25433` / `taskbound_gateway` |
| 业务数据源库 | 仅 Gateway 内部网络 / `travel_demo` |

如需本机数据库客户端调试，可显式启用非论文部署覆盖：

```bash
docker compose -f compose.yaml -f compose.debug.yaml up --build -d --wait
```

Navicat 的用户名和密码对应关系见[本地启动与数据库调试](docs/getting-started.md#navicat-连接参数)。Business PostgreSQL 仅在显式启用调试覆盖时绑定宿主机回环地址。

## MCP 2.0 工作流

1. 调用 `list_data_products` 获取完整逻辑产品、字段和 Scope 的允许值/日期边界。
2. 调用 `request_data_task`，显式提交非空 `objective`、`data_products`、每个产品的非空 `columns` 及 `scopes`。请求预算只能缩小 Catalog 上限。
3. 在 OA 提交并完成自动或人工审批。
4. ACTIVE 后调用 `execute_plan(task_id, request_id, plan)` 或 `query_sql(task_id, request_id, sql)`。

`request_id` 由客户端生成并在一个任务内保持唯一。相同 ID 和相同请求只返回首次持久化结果/状态；相同 ID 搭配不同请求会关闭式拒绝，重试不会产生第二次执行或预算消费。

`execute_plan` 的最小示例：

```json
{
  "task_id": "task_...",
  "request_id": "analysis-step-001",
  "plan": {
    "product": "expense_summary",
    "columns": ["month", "total_amount"],
    "order_by": [{"column": "month", "direction": "asc"}]
  }
}
```

直接 SQL 只能引用逻辑产品名和获批字段，例如：

```sql
SELECT month, sum(total_amount) AS amount
FROM expense_summary
GROUP BY month
ORDER BY month
```

不得引用 `reporting.*`、`legacy.*` 或系统目录。Gateway 会注入强制 Scope，并按剩余预算限制结果行数和执行时间。

## 身份

| 身份 | 入口 | 权限 | 凭据来源 |
|---|---|---|---|
| Alice | MCP | 申请任务、查询自己的 ACTIVE 任务、读取自己的结果与凭证 | `TASKBOUND_ALICE_TOKEN` |
| Carol | MCP | 读取审计事件和查询凭证，不能读取原始结果 | `TASKBOUND_CAROL_TOKEN` |
| Alice | OA | 提交自己的 OA 草稿 | `alice` / `OA_ALICE_PASSWORD` |
| Bob | OA | 审批人工任务 | `bob` / `OA_BOB_PASSWORD` |

## 验证与数据保留

```bash
make verify
make formal
make eval-smoke
make paper
make logs
docker compose down
```

`make verify` 会执行格式检查、`go vet`、真实 PostgreSQL `go test -race ./...`、镜像构建和隔离的 Compose 端到端验收。

设置 `GATEWAY_RESULT_RETENTION_TTL` 会让 Gateway 定期删除超过保留期的结果密文，同时保留查询记录、回执和审计证据。每个结果密文还绑定 `GATEWAY_DATA_KEY_ID` 并登记在 `result_encryption_keys`；带 `GATEWAY_ADMIN_TOKEN` 的管理员可以擦除 key ID，使对应密文保留但后续读取 fail closed。该 Demo 不销毁外部 KMS 中的真实 key material，生产环境需要把 key ID 擦除接入 KMS/HSM/Secret Manager。设置 `GATEWAY_ADMIN_TOKEN` 也会启用本机管理员接口，用于手动 purge 以及设置/释放 legal hold；active hold 会阻止对应任务的结果密文被清理。

查询回执验证方可读取 `/.well-known/taskgate/query-receipt-keyring.json`，获得 `taskgate-query-receipt-keyring/v1` 公钥 Bundle。Bundle 包含 active Gateway Key ID、历史验签公钥以及 `valid_from`/`retired_at` 窗口，不包含私钥材料。

设置 `GATEWAY_AUDIT_ANCHOR_URL` 会让 Gateway 定期把当前审计 Hash Chain checkpoint 签名为 `taskgate-audit-checkpoint-anchor/v1` 并 POST 到外部日志或 WORM 服务。该外部服务的保留和不可篡改性由部署环境保证。

`docker compose down` 保留 `control-pg-data` 与 `business-pg-data`；`docker compose down --volumes` 会删除当前 Compose 项目的这两个 Volume。旧版本的 `gateway-data` Volume 已不再挂载，也不会被本次改造自动删除，可按需手工备份或清理。

## 文档

- [架构与安全边界](docs/architecture.md)
- [Compose 启动、Navicat 与 MCP 演示](docs/getting-started.md)
- [Catalog 编写指南](docs/catalog-guide.md)
- [OA 与数据源适配器接口](docs/adapters.md)
- [SQL 与 QueryPlan 安全规则](docs/sql-security.md)
- [威胁模型与生产化差距](docs/threat-model.md)
