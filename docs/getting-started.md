# Compose 启动、Navicat 与 MCP 演示

## 1. 准备环境

需要 Docker Engine 与 Docker Compose v2。人工 MCP 演示还需要本机 MCP 客户端；服务只绑定 `127.0.0.1`，无法从不具备宿主机回环访问能力的云端客户端直接连接。

```bash
cp .env.example .env
openssl rand -base64 32
./scripts/generate-ed25519-env.sh
```

将第一条随机输出填入 `GATEWAY_DATA_KEY`，并为该数据密钥设置稳定的 `GATEWAY_DATA_KEY_ID`；同一批已保存结果必须继续使用原 key 和 key ID，直到对应 key ID 被擦除。把密钥脚本输出的七个非注释变量填入 `.env`。OA 只持有审批回执私钥，Gateway 只持有对应公钥；Gateway 查询回执使用另一把独立私钥。生产网关也可用 `OA_RECEIPT_KEYRING_JSON` 提供多把 OA 验签公钥及 `valid_from`/`retired_at` 窗口，旧的单公钥变量仍可用于本地演示。Gateway 查询回执验证公钥由 `/.well-known/taskgate/query-receipt-keyring.json` 发布；`GATEWAY_RECEIPT_KEYRING_JSON` 可加入历史公钥和 active key 的有效/退役窗口。配置 `GATEWAY_AUDIT_ANCHOR_URL` 后，Gateway 会用独立 audit-anchor Ed25519 key 定期向外部日志/WORM 服务 POST 当前审计 checkpoint。随后替换 `.env` 中全部空密码、Token 和共享密钥。系统控制库与业务库使用不同的管理员密码和应用密码：

- 控制库：`CONTROL_POSTGRES_ADMIN_PASSWORD`、`CONTROL_DB_PASSWORD`
- 业务库：`POSTGRES_PASSWORD`、`GATEWAY_DB_PASSWORD`

`.env` 已被 Git 忽略。不要把真实值复制到提交、提示词、日志或截图中。同一批加密查询结果必须继续使用原 `GATEWAY_DATA_KEY` 和 `GATEWAY_DATA_KEY_ID`，否则无法解密；管理员擦除 key ID 后，即使密文仍在控制库中，Gateway 也会拒绝读取。

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
| 查询回执验证 Keyring | `http://127.0.0.1:8082/.well-known/taskgate/query-receipt-keyring.json` | 回环地址 |
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

`gateway_reader` 只能读取 `reporting.datasource_attestation` 以及已发布的 `reporting.expense_summary` 和 `reporting.expense_detail` View，不能访问 `legacy.*`，也不能执行写操作。

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

MCP `serverInfo.version` 为 `2.0.0`。Alice 可见 14 个任务/查询工具，Carol 只可见两个审计工具：

| 身份 | 工具 |
|---|---|
| Alice | `list_data_products`、`request_data_task`、`list_my_tasks`、`get_task_status`、`wait_for_approval`、`get_task_context`、`execute_plan`、`query_sql`、`get_query_result`、`get_budget`、`list_receipts`、`complete_task`、`revoke_task` |
| Carol | `list_audit_events`、`get_audit_receipt` |

## 4. 结构化申请与人工审批

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
    "max_rows": 50,
    "max_release_facts": 100,
    "max_influence_facts": 500,
    "max_outcome_facts": 2
  }
}
```

每个申请产品必须有非空字段列表；未知产品、字段、Scope 或越界值会被拒绝。
`requested_budget` 只能缩小 Catalog Profile 的资源和三维 exposure 上限。

`request_data_task` 返回 `task_id`、`oa_url`、审批模式和 `AuthorizationManifestV1` 摘要。用 `.env` 的 `OA_ALICE_PASSWORD` 以 `alice` 登录 OA，打开草稿并提交。所有敏感级别（包括低敏 `expense_summary`）都会停在 `AWAITING_APPROVAL`；必须由 `bob` 登录 OA 明确批准或缩小后批准，任务才会进入 `ACTIVE`。

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

V2 任务也可提交受限的树形 `from`。当前只允许两个 Scan 叶子的 INNER
equijoin，或同一产品两个过滤分支的 `union_distinct`；字段用 Catalog 稳定角色
限定，Union 必须显式列出完整去重 schema。可在其上继续 `group_by` 和四种
聚合。嵌套关系树、self-join、`UNION ALL`、关系计划的 order/limit/offset 会
关闭式拒绝。详细 JSON 示例见仓库根目录 README。

默认 Catalog 定义 `taskgate-exposure-v3` budget profiles，且所有 approval
route 都是人工审批。Gateway 会在一个只读
`REPEATABLE READ` 事务中执行可见查询和 provenance companion，先在内存
缓冲，再按根任务已知集合结算。响应中的 `exposure` 给出本次
`actual_*_facts` 与真正新增的 `charged_*_facts`；`exposure_budget` 给出共享
根账本。内部补取的 `entity_key` 不会出现在客户端结果中。

## 5. 人工审批、规划与委托

申请高敏 `expense_detail` 时，Alice 提交草稿后任务停留在 `AWAITING_APPROVAL`。以 `bob` 和 `OA_BOB_PASSWORD` 登录 OA 批准或拒绝。批准前查询返回 `TASK_NOT_ACTIVE`；拒绝后任务归档为 `ARCHIVED(rejected)`。

启用 exposure 的 Grant 不接受直接 SQL，因为任意 SQL 尚不能生成完整、可证明
的 provenance companion；`query_sql` 会返回 `EXPOSURE_EVIDENCE_REQUIRED`。
它只保留给旧的 resource-only 兼容 Grant。默认 Demo 应继续使用
`execute_plan`，例如上面的聚合。

`request_data_task` 还支持 `parent_task_id` 和 `delegate_principal_id`。子任务必须
由父任务所有者发起，所有授权维度只能收缩，且父子共享同一 exposure 账本。
默认 Compose 只注册 Alice 这个 query Principal，因此跨主体演示需要部署者先
注册第二个已启用 query Principal；同主体也可创建受父 Grant 约束的子任务。

## 6. 预算、结果与审计

- `get_task_context`：获批产品、字段、Scope、凭证与期限。
- `get_budget`：查询数、累计行数和累计 DB 毫秒的上限、已用、预留和剩余值，并在 exposure 启用时返回根任务三维账本。
- `get_query_result`：Alice 按 `task_id + query_id` 读取 AES-256-GCM 加密保存的结果。
- `list_receipts`：V3 exposure 结算使用 V5；V2/V1 兼容 exposure 使用 V4，无 exposure evidence 的兼容终态使用 V3。
- `complete_task`：主动归档任务。
- `revoke_task`：阻止新查询；已在途查询不会被宣称立即取消，仍受原超时和 Grant 到期约束。

任务被撤销、过期或完成归档后，旧查询结果仍可由任务所有者读取，直到结果保留清理删除对应密文，或管理员擦除对应结果密钥 ID；查询回执和审计证据不会随密文清理或 key ID 擦除删除。设置 `GATEWAY_RESULT_RETENTION_TTL` 会启动定期密文清理；设置 `GATEWAY_ADMIN_TOKEN` 会启用 `/admin/v1/retention/purge`、`/admin/v1/retention/legal-holds/{task_id}` 以及 `/admin/v1/result-encryption-keys/{key_id}/erase` 的本机管理员接口。active legal hold 会阻止该任务密文被清理，但不会自动阻止单独的 key ID 擦除流程；生产环境应把该接口接入组织级审批/KMS 流程。禁用 Principal 会阻止该身份继续列出或调用任何 MCP 工具，即使客户端仍持有旧 Bearer Token。达到任一资源预算硬上限时，当前合法查询会在允许范围内返回，随后任务归档为 `budget_exhausted`；exposure 超限则不返回当前结果。Carol 只能读取审计事件和凭证，不能读取原始行。

## 7. 停止、恢复与重置

```bash
docker compose down
```

再次启动时，Gateway 从 `control-pg-data` 恢复任务、Grant、预算、加密结果与审计链；业务数据保存在独立的 `business-pg-data`。不确定是否完成的 RESERVED 查询按完整资源预留量保守计费并标记 `INDETERMINATE`，释放未结算的 exposure reservation，并在同一恢复事务中写入查询回执；结果从不在结算提交前释放，同一 `request_id` 也禁止自动重执行。PROCESSING 回调变为可重试。OA Demo 草稿仍在内存中，OA 容器重启后会丢失。

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
| 旧结果无法解密 | `GATEWAY_DATA_KEY` 或 `GATEWAY_DATA_KEY_ID` 与写入时不同；恢复原密钥和 key ID；若 key ID 已被管理员擦除，则只能读取查询记录、回执和审计证据 |
| ACTIVE 任务目录版本冲突 | Catalog 版本已变化；重新申请新版本任务 |
| OA 页面没有旧草稿 | OA Demo 为内存实现，重启不持久化草稿 |
| Navicat 无法连接业务库 | 默认部署不发布该端口；仅调试时显式叠加 `compose.debug.yaml`，再使用 `127.0.0.1:25434` |
