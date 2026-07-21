# Compose 启动与 Codex MCP 演示

## 1. 准备环境

必需：

- Docker Engine
- Docker Compose v2（`docker compose version` 可用）

人工 MCP 演示还需要本机 Codex CLI。宿主机不需要安装 Go、PostgreSQL 或 SQLite。该配置访问 `127.0.0.1`，因此只适用于本地 Codex，不适用于无法访问宿主机回环地址的 Codex Cloud。

复制环境文件：

```bash
cp .env.example .env
```

生成 32 字节 Base64 数据密钥。可以使用宿主机 OpenSSL：

```bash
openssl rand -base64 32
```

或者只使用 Docker（该镜像也是 Compose 的数据库镜像）：

```bash
docker run --rm --entrypoint sh postgres:16-bookworm -c 'head -c 32 /dev/urandom | base64'
```

用编辑器更新 `.env`：

- 将输出填入 `GATEWAY_DATA_KEY`。同一个 `gateway-data` Volume 必须持续使用同一密钥，否则旧结果无法解密。
- 替换 Alice/Carol Token、OA 服务 Token、回调密钥、会话密钥、Alice/Bob 密码和 PostgreSQL 密码。
- 填写有效的 `DEEPSEEK_API_KEY` 才能使用 `request_data_task` 与 `query_data`。未配置时，健康检查、目录、状态、审计和已有 ACTIVE 任务的 `query_sql` 等确定性路径仍可用，但全新环境无法完成自然语言任务申请演示。

`.env` 已被 Git 忽略，不要把其中的值复制到提交、提示词、日志或截图中。

## 2. 启动与验证

```bash
docker compose up --build -d --wait
docker compose ps
curl -i http://127.0.0.1:8080/health/live
curl -i http://127.0.0.1:8080/health/ready
curl -i http://127.0.0.1:8090/health/ready
```

预期三个请求都返回 `204 No Content`。Gateway `ready` 会检查 SQLite、PostgreSQL，以及是否存在尚未持久化成功的查询预算结算；OA Demo 的 `ready` 检查 HTTP 进程。

查看日志：

```bash
docker compose logs -f gateway oa-demo
```

服务地址：

| 服务 | 地址 | 暴露范围 |
|---|---|---|
| MCP | `http://127.0.0.1:8080/mcp` | 仅宿主机回环地址 |
| Gateway 健康检查 | `http://127.0.0.1:8080/health/live`、`/health/ready` | 仅宿主机回环地址 |
| OA 登录 | `http://127.0.0.1:8090/login` | 仅宿主机回环地址 |
| PostgreSQL | Compose 内部 `postgres:5432` | 未发布到宿主机 |

## 3. 连接 Codex

仓库根目录的 `.codex/config.toml` 已配置：

```toml
[mcp_servers.taskbound_gateway]
url = "http://127.0.0.1:8080/mcp"
bearer_token_env_var = "TASKBOUND_GATEWAY_TOKEN"
required = true
startup_timeout_sec = 10
tool_timeout_sec = 120
```

在仓库根目录启动 Alice 会话；环境变量值必须与 `.env` 中的 `TASKBOUND_ALICE_TOKEN` 相同：

```bash
export TASKBOUND_GATEWAY_TOKEN='<Alice Token>'
codex
```

可先让 Codex“列出可用数据产品”，确认 MCP 已加载。Alice 会看到 12 个查询者工具；Carol 只会看到两个审计工具。所有工具成功或失败结果都有 `trace_id`。

| 身份 | 工具 |
|---|---|
| Alice | `list_data_products`、`request_data_task`、`list_my_tasks`、`get_task_status`、`wait_for_approval`、`get_task_context`、`query_data`、`query_sql`、`get_query_result`、`get_budget`、`list_receipts`、`complete_task` |
| Carol | `list_audit_events`、`get_audit_receipt` |

`wait_for_approval` 默认轮询 30 秒、最大 45 秒。Gateway 不向已经关闭的 Codex 会话推送消息；审批完成后应回到原线程说“继续”，或重新读取任务状态。

## 4. 汇总数据自动审批

在 Alice 的 Codex 会话输入：

> 申请查询销售部各月份差旅报销总额，使用 expense_summary，并等待审批。

`request_data_task` 返回 `task_id`、`oa_url`、审批模式、敏感级别和预算。复制或打开 `oa_url`：

1. 在 `http://127.0.0.1:8090/login` 以用户名 `alice`、密码 `.env` 中的 `OA_ALICE_PASSWORD` 登录。
2. 打开草稿，核对页面展示的已批准产品、字段、强制范围、预算、TTL、Catalog 版本与授权快照 Hash，再点击“提交申请”。
3. `expense_summary` 为 `low`，OA 会依次发送 `submitted` 和 `approved` 签名回调并自动批准。
4. 回到原 Codex 线程输入：“继续等待刚才的审批；批准后按月份查询总额并分析变化。”

也可以要求 Codex 使用直接 SQL：

```sql
SELECT month, sum(total_amount) AS amount
FROM expense_summary
GROUP BY month
ORDER BY month
```

这里使用逻辑名 `expense_summary`，不使用 `reporting.expense_summary`。Gateway 会在服务端注入已批准的 `department=销售部` 范围并施加剩余行预算。

## 5. 明细数据人工审批

继续使用 Alice Token，新建任务：

> 申请查询销售部员工报销明细，需要报销单号、员工编号、姓名、报销日期、金额和城市，使用 expense_detail。

1. 以 Alice 打开新 `oa_url` 并提交。任务停留在 `AWAITING_APPROVAL`，此时查询会返回 `TASK_NOT_ACTIVE`。
2. 在 OA 退出 Alice，以用户名 `bob`、密码 `.env` 中的 `OA_BOB_PASSWORD` 登录。
3. Bob 的列表只显示人工审批任务。打开任务后点击“批准”或“拒绝”。
4. 批准后回到 Alice 的原 Codex 线程输入：“继续，读取任务上下文，再查询已批准的销售部员工明细。”

若 Bob 拒绝，任务会归档为 `ARCHIVED(rejected)`，之后不能重新批准或查询。若批准，默认 TTL 为 15 分钟；到期扫描最多约 15 秒后归档任务。

## 6. 预算、结果与凭证

让 Codex 对同一 ACTIVE 任务调用：

- `get_task_context`：查看批准产品、字段、强制范围、审批凭证和期限。
- `get_budget`：查看查询数、累计行数和累计 DB 毫秒的上限、已用、预留和剩余值。
- `get_query_result`：按 `task_id + query_id` 读取自己的已保存结果。
- `list_receipts`：查看请求摘要、SQL 指纹、目录版本、预算前后、结果 SHA-256 和错误码。
- `complete_task`：主动归档为 `completed`。

达到查询数、累计行数或累计 DB 时间任一硬上限时，本次合法查询会在允许范围内返回，随后任务自动归档为 `budget_exhausted`。数据库执行失败也会结算一次查询和实际允许范围内的 DB 时间。

## 7. Carol 审计演示

结束或另开 Codex 进程，以 Carol Token 启动：

```bash
export TASKBOUND_GATEWAY_TOKEN='<与 .env 中 TASKBOUND_CAROL_TOKEN 相同的值>'
codex
```

示例提示：

> 列出任务 `<task_id>` 的审计事件，检查 previous_hash/current_hash 链，并读取查询 `<query_id>` 的审计凭证。

Carol 可以使用 `list_audit_events` 和 `get_audit_receipt`，但服务端不会向 Carol 注册 `get_query_result`，也不会在审计凭证中返回原始敏感行。

## 8. 停止、保留与重置

停止但保留数据：

```bash
docker compose down
```

再次启动后，Gateway 会从 `gateway-data` 恢复任务、Grant、预算、结果与审计；不确定是否执行完成的 RESERVED 查询会按完整预留量保守计费并标记 `INTERRUPTED`。PostgreSQL 使用 `pg-data`。OA Demo 草稿目前只保存在内存中，OA 容器重启后会丢失，因此未完成草稿需要重新申请。

彻底重置 Demo：

```bash
docker compose down --volumes --remove-orphans
```

该命令会永久删除两个命名 Volume 中的全部 Demo 数据。

## 9. 常见问题

| 现象 | 原因与处理 |
|---|---|
| Gateway 启动即退出 | 检查 `docker compose logs gateway`；常见原因是缺少环境变量、数据密钥不是 32 字节、Catalog 无效或 PostgreSQL 不可达 |
| `MODEL_UNAVAILABLE` | `DEEPSEEK_API_KEY` 未配置、API/模型不可用；已有任务的直接 SQL 与确定性工具不依赖模型 |
| 修改 Alice/Carol Token 后 Gateway 拒绝启动 | SQLite 已保存旧 Token 的摘要；Demo 中恢复旧 Token，或明确接受删除历史后重置 `gateway-data` |
| 旧结果返回密文错误 | `GATEWAY_DATA_KEY` 与创建结果时不同；恢复原密钥。Demo 尚无密钥轮换 |
| ACTIVE 任务突然返回目录版本冲突 | `catalog_version` 已变更；现有任务会关闭式拒绝，重新申请新版本任务 |
| OA 页面没有旧草稿 | OA Demo 是内存实现，重启不持久化草稿 |
| Codex 找不到 MCP | 确认从仓库根目录启动、`.codex/config.toml` 存在、`TASKBOUND_GATEWAY_TOKEN` 已导出且与 Gateway 配置一致 |
