# Compose 启动、Navicat 与 MCP 演示

## 1. 准备环境

需要 Docker Engine 与 Docker Compose v2。人工 MCP 演示还需要本机 MCP 客户端；服务只绑定 `127.0.0.1`，无法从不具备宿主机回环访问能力的云端客户端直接连接。

```bash
cp .env.example .env
openssl rand -base64 32
./scripts/generate-ed25519-env.sh
```

将第一条随机输出填入 `GATEWAY_DATA_KEY`，并把密钥脚本输出的五个非注释变量填入 `.env`。OA 只持有审批回执私钥，Gateway 只持有对应公钥；Gateway 查询回执使用另一把独立私钥。随后替换 `.env` 中全部空密码、Token 和共享密钥。系统控制库与业务库使用不同的管理员密码和应用密码：

- 控制库：`CONTROL_POSTGRES_ADMIN_PASSWORD`、`CONTROL_DB_PASSWORD`
- 业务库：`POSTGRES_PASSWORD`、`GATEWAY_DB_PASSWORD`

`.env` 已被 Git 忽略。不要把真实值复制到提交、提示词、日志或截图中。同一批加密查询结果必须继续使用原 `GATEWAY_DATA_KEY`，否则无法解密。

## 2. 启动与健康检查

```bash
docker compose up --build -d --wait
docker compose ps
curl -i http://127.0.0.1:8082/health/live
curl -i http://127.0.0.1:8082/health/ready
curl -i http://127.0.0.1:8092/health/ready
```

三个请求应返回 `204 No Content`。Gateway readiness 同时检查系统控制 PostgreSQL、业务 PostgreSQL、Reporting View 列/类型与 Catalog 的 Schema Attestation，以及是否存在尚未成功持久化的预算结算。

| 服务 | 地址 | 暴露范围 |
|---|---|---|
| MCP | `http://127.0.0.1:8082/mcp` | 回环地址 |
| Gateway 健康检查 | `http://127.0.0.1:8082/health/live`、`/health/ready` | 回环地址 |
| OA 登录 | `http://127.0.0.1:8092/login` | 回环地址 |
| 系统控制 PostgreSQL | `127.0.0.1:25433` | 回环地址 |
| 业务 PostgreSQL | 默认无宿主机地址 | 仅 Compose 内部网络 |

查看日志：

```bash
docker compose logs -f gateway control-postgres business-postgres oa-demo
```

## Navicat 连接参数

### 系统控制库

| Navicat 字段 | 值 |
|---|---|
| 主机 | `127.0.0.1` |
| 端口 | `25433`，或 `.env` 的 `CONTROL_POSTGRES_PORT` |
| 数据库 | `.env` 的 `CONTROL_POSTGRES_DB`，默认 `taskbound_gateway` |
| 管理用户名 | `postgres` |
| 管理密码 | `.env` 的 `CONTROL_POSTGRES_ADMIN_PASSWORD` |
| Gateway 应用用户名 | `gateway_control` |
| Gateway 应用密码 | `.env` 的 `CONTROL_DB_PASSWORD` |

调试表结构建议使用管理员账号；验证 Gateway 最小权限时使用 `gateway_control`。

### 业务数据源库

默认论文部署不发布业务库端口。只有明确用于本机调试时才运行：

```bash
docker compose -f compose.yaml -f compose.debug.yaml up --build -d --wait
```

| Navicat 字段 | 值 |
|---|---|
| 主机 | `127.0.0.1` |
| 端口 | `25434`，或 `.env` 的 `POSTGRES_PORT` |
| 数据库 | `.env` 的 `POSTGRES_DB`，默认 `travel_demo` |
| 管理用户名 | `.env` 的 `POSTGRES_USER`，默认 `postgres` |
| 管理密码 | `.env` 的 `POSTGRES_PASSWORD` |
| Gateway 只读用户名 | `gateway_reader` |
| Gateway 只读密码 | `.env` 的 `GATEWAY_DB_PASSWORD` |

`gateway_reader` 只能读取已发布的 `reporting.expense_summary` 和 `reporting.expense_detail` View，不能访问 `legacy.*`，也不能执行写操作。

也可以用容器内 `psql` 验证：

```bash
docker compose exec control-postgres \
  psql -U postgres -d taskbound_gateway

docker compose exec business-postgres sh -c \
  'PGPASSWORD="$GATEWAY_DB_PASSWORD" psql -U gateway_reader -d "$POSTGRES_DB" -c "SELECT count(*) FROM reporting.expense_summary"'
```

## 3. 连接 MCP 客户端

客户端连接参数：

```toml
[mcp_servers.taskbound_gateway]
url = "http://127.0.0.1:8082/mcp"
bearer_token_env_var = "TASKBOUND_GATEWAY_TOKEN"
required = true
startup_timeout_sec = 10
tool_timeout_sec = 120
```

Alice 会话的环境变量值必须与 `.env` 中的 `TASKBOUND_ALICE_TOKEN` 一致：

```bash
export TASKBOUND_GATEWAY_TOKEN='<Alice Token>'
codex
```

MCP `serverInfo.version` 为 `2.0.0`。Alice 可见 12 个任务/查询工具，Carol 只可见两个审计工具：

| 身份 | 工具 |
|---|---|
| Alice | `list_data_products`、`request_data_task`、`list_my_tasks`、`get_task_status`、`wait_for_approval`、`get_task_context`、`execute_plan`、`query_sql`、`get_query_result`、`get_budget`、`list_receipts`、`complete_task`、`revoke_task` |
| Carol | `list_audit_events`、`get_audit_receipt` |

## 4. 结构化申请与自动审批

先调用 `list_data_products`。响应会给出逻辑产品字段，以及全部 Scope 的类型、允许值或日期边界。申请不得省略产品、字段或 Scope：

```json
{
  "objective": "按月份分析销售部差旅报销",
  "data_products": ["expense_summary"],
  "columns": {
    "expense_summary": ["month", "total_amount"]
  },
  "scopes": {
    "department": ["销售部"]
  },
  "requested_budget": {
    "max_queries": 2,
    "max_rows": 50
  }
}
```

每个申请产品必须有非空字段列表；未知产品、字段、Scope 或越界值会被拒绝。`requested_budget` 只能缩小 Catalog Profile 的上限。

`request_data_task` 返回 `task_id`、`oa_url`、审批模式和 `AuthorizationManifestV1` 摘要。用 `.env` 的 `OA_ALICE_PASSWORD` 以 `alice` 登录 OA，打开草稿并提交。低敏 `expense_summary` 会自动批准。

任务 ACTIVE 后可提交 QueryPlan：

```json
{
  "task_id": "task_...",
  "request_id": "summary-plan-001",
  "plan": {
    "product": "expense_summary",
    "columns": ["month"],
    "aggregates": [
      {"function": "sum", "column": "total_amount", "alias": "amount"}
    ],
    "group_by": ["month"],
    "order_by": [{"column": "month", "direction": "asc"}],
    "limit": 20
  },
  "output_format": "json"
}
```

`execute_plan` 在本地严格校验产品、字段、聚合、过滤、排序和 Limit，确定性编译为 SQL，再进入与 `query_sql` 相同的 PostgreSQL AST 策略。Gateway 不调用外部模型。

## 5. 人工审批与直接 SQL

申请高敏 `expense_detail` 时，Alice 提交草稿后任务停留在 `AWAITING_APPROVAL`。以 `bob` 和 `OA_BOB_PASSWORD` 登录 OA 批准或拒绝。批准前查询返回 `TASK_NOT_ACTIVE`；拒绝后任务归档为 `ARCHIVED(rejected)`。

批准后可执行直接 SQL；调用 `query_sql` 时还必须提供任务内唯一的 `request_id`：

```sql
SELECT receipt_no, amount
FROM expense_detail
ORDER BY receipt_no
```

SQL 只能使用逻辑产品和获批字段。Gateway 会注入申请时批准的 Scope、外层行数限制和只读超时。

## 6. 预算、结果与审计

- `get_task_context`：获批产品、字段、Scope、凭证与期限。
- `get_budget`：查询数、累计行数和累计 DB 毫秒的上限、已用、预留和剩余值。
- `get_query_result`：Alice 按 `task_id + query_id` 读取 AES-256-GCM 加密保存的结果。
- `list_receipts`：Gateway Ed25519 签名的 V1 查询回执，绑定 Manifest/Grant/Catalog 摘要、`request_id`、SQL 指纹、预算预留/结算、结果 Hash 和审计链位置。
- `complete_task`：主动归档任务。
- `revoke_task`：阻止新查询；已在途查询不会被宣称立即取消，仍受原超时和 Grant 到期约束。

达到任一硬预算时，当前合法查询会在允许范围内返回，随后任务归档为 `budget_exhausted`。Carol 只能读取审计事件和凭证，不能读取原始行。

## 7. 停止、恢复与重置

```bash
docker compose down
```

再次启动时，Gateway 从 `control-pg-data` 恢复任务、Grant、预算、加密结果与审计链；业务数据保存在独立的 `business-pg-data`。不确定是否完成的 RESERVED 查询按完整预留量保守计费并标记 `INDETERMINATE`，同一 `request_id` 禁止自动重执行；PROCESSING 回调变为可重试。OA Demo 草稿仍在内存中，OA 容器重启后会丢失。

彻底重置当前版本数据：

```bash
docker compose down --volumes --remove-orphans
```

该命令删除当前 Compose 项目的 `control-pg-data` 和 `business-pg-data`。旧版本曾使用的 `gateway-data` Volume 不再被 Compose 引用，本次改造不会自动删除它；如需恢复或清理，请先用 `docker volume ls` 确认确切名称并手工处理。

## 8. 常见问题

| 现象 | 原因与处理 |
|---|---|
| Gateway 启动即退出 | 检查必填环境变量、32 字节数据密钥、Catalog、控制库和业务库连通性 |
| 修改 Alice/Carol Token 后 Gateway 拒绝启动 | 控制 PostgreSQL 已保存原 Principal 摘要；恢复旧 Token，或明确接受丢失 Demo 历史后重置 `control-pg-data` |
| 旧结果无法解密 | `GATEWAY_DATA_KEY` 与写入时不同；恢复原密钥 |
| ACTIVE 任务目录版本冲突 | Catalog 版本已变化；重新申请新版本任务 |
| OA 页面没有旧草稿 | OA Demo 为内存实现，重启不持久化草稿 |
| Navicat 无法连接业务库 | 默认部署不发布该端口；仅调试时显式叠加 `compose.debug.yaml`，再使用 `127.0.0.1:25434` |
