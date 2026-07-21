# Task-bound Agent Data Gateway

Task-bound Agent Data Gateway 是一个本地演示系统：Agent 必须先提交明确的数据产品、字段、Scope、预算和目的，经 OA 审批后，才能查询只读数据产品。Gateway 不包含模型层；它只接受结构化任务申请、声明式 `QueryPlan` 或 SQL，并以确定性策略完成授权、PostgreSQL AST 校验、预算扣减、结果加密和审计。

> 当前仓库是单实例 Demo，不是可直接上线的生产网关。生产差距见[威胁模型与生产化差距](docs/threat-model.md)。

## 架构概览

```text
本地 Codex / MCP Client
          │ Bearer Token + MCP 2.0
          ▼
 Gateway :8082 ─────────► OA Demo :8092（草稿、审批、HMAC 回调）
    │
    ├───────────────────► Control PostgreSQL :5432（宿主机 127.0.0.1:25433）
    │                       任务、Grant、预算、AES-GCM 结果、审计链
    │
    └───────────────────► Business PostgreSQL :5432（宿主机 127.0.0.1:25434）
                            gateway_reader 只读 Reporting Views
```

两个 PostgreSQL 使用独立容器、账号和 Volume。Gateway 仍按单实例部署；数据库行锁保证请求并发安全，本版本不提供多 Gateway 租约协议。

## 快速启动

宿主机只需要 Docker Engine 与 Docker Compose v2：

```bash
cp .env.example .env
openssl rand -base64 32  # 填入 GATEWAY_DATA_KEY
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
| 业务数据源库 | `127.0.0.1:25434` / `travel_demo` |

Navicat 的用户名和密码对应关系见[本地启动与数据库调试](docs/getting-started.md#navicat-连接参数)。数据库端口仅绑定宿主机回环地址。

## MCP 2.0 工作流

1. 调用 `list_data_products` 获取完整逻辑产品、字段和 Scope 的允许值/日期边界。
2. 调用 `request_data_task`，显式提交非空 `objective`、`data_products`、每个产品的非空 `columns` 及 `scopes`。请求预算只能缩小 Catalog 上限。
3. 在 OA 提交并完成自动或人工审批。
4. ACTIVE 后调用 `execute_plan(task_id, plan)` 或 `query_sql(task_id, sql)`。

`execute_plan` 的最小示例：

```json
{
  "task_id": "task_...",
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
make logs
docker compose down
```

`make verify` 会执行格式检查、`go vet`、真实 PostgreSQL `go test -race ./...`、镜像构建和隔离的 Compose 端到端验收。

`docker compose down` 保留 `control-pg-data` 与 `business-pg-data`；`docker compose down --volumes` 会删除当前 Compose 项目的这两个 Volume。旧版本的 `gateway-data` Volume 已不再挂载，也不会被本次改造自动删除，可按需手工备份或清理。

## 文档

- [架构与安全边界](docs/architecture.md)
- [Compose 启动、Navicat 与 MCP 演示](docs/getting-started.md)
- [Catalog 编写指南](docs/catalog-guide.md)
- [OA 与数据源适配器接口](docs/adapters.md)
- [SQL 与 QueryPlan 安全规则](docs/sql-security.md)
- [威胁模型与生产化差距](docs/threat-model.md)
