# TaskGate: Task-Bound Exposure Accounting for Database Agents

TaskGate 是一个数据库研究原型：它把累计数据暴露绑定到人类授权的根任务，使自适应查询、重试、分页和子 Agent 共享同一知识账本。Agent 必须先提交明确的数据产品、字段、Scope、预算和目的，经 OA 审批后，才能查询只读数据产品。Gateway 不包含模型层；授权、provenance、计量、结算和规划均由确定性代码完成。

> 当前仓库是单实例 Demo，不是可直接上线的生产网关。生产差距见[威胁模型与生产化差距](docs/threat-model.md)。

## 核心模型

每个事实至少绑定 `(product, snapshot, entity key, field, value version)`。
根任务维护两个集合账本：实际交付的 `release exposure`，以及影响 Join、过滤
或聚合结果的 `source influence`。一次查询只支付相对根任务已知集合的新事实：

```text
delta(T, q) = (|F_release(q) - K_release(T)|,
               |F_influence(q) - K_influence(T)|)
```

在线路径是 `reserve -> execute/buffer -> derive provenance -> settle -> release`。
可见结果与 provenance companion 在同一个只读 `REPEATABLE READ` 事务执行；
只有双账本结算、加密结果、审计和 V4/V5 签名回执原子提交后，结果才会释放。
超出任一 exposure 上限的结果不会交付或保存。

实现范围和非目标见[任务级数据暴露记账](docs/exposure-accounting.md)。

## 架构概览

```text
本地 Codex / MCP Client
          │ Bearer Token + MCP 2.0
          ▼
 Gateway :8082 ─────────► OA Demo :8092（草稿、审批、HMAC 回调 + Ed25519 回执）
    │
    ├───────────────────► Control PostgreSQL :5432（宿主机 127.0.0.1:25433）
    │                       任务族、双 Exposure 账本、资源预算、密文结果、审计链
    │
    └───────────────────► Business PostgreSQL :5432（仅内部网络，默认不发布宿主机端口）
                            同一快照的结果 + Provenance Companion
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
2. 调用 `request_data_task`，显式提交非空 `objective`、`data_products`、每个产品的非空 `columns` 及 `scopes`。资源预算与 release/influence 上限都只能缩小 Catalog Profile。
3. 在 OA 提交并完成自动或人工审批。
4. ACTIVE 后使用 `execute_plan(task_id, request_id, plan)`。默认 Catalog 已启用 exposure Profile，因此该路径会生成同快照 provenance 并进行双账本结算。
5. `taskgate-exposure-v2` 的 `plan_exposure` 会在同一快照执行候选 QueryPlan，以服务端 FactSet 的真实重叠做 exact planning，并在同一 root-lock 事务结算后只释放选中结果；V1 标量 planner 仅作不混用的兼容路径。详见 [V2 Profile](docs/exposure-v2.md)。子 Agent 任务通过 `parent_task_id` 和 `delegate_principal_id` 创建，且共享根账本。

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

成功响应包含 `exposure` 与 `exposure_budget`：前者区分本次 actual facts 和
相对根任务的 charged facts，后者给出共享账本的上限、已用和剩余值。相同
`request_id` 只观察首次终态；新的 request ID 查询相同事实时，增量收费为零。

`query_sql` 仍用于 resource-only 兼容 Grant；对启用 exposure 的任务，它会因
无法构造完整 provenance 而关闭式拒绝。当前在线精确计量片段是单产品
QueryPlan 的 projection/filter/order/limit 与 `COUNT/SUM/MIN/MAX` 聚合；Join
和 Union 已在可执行代数中实现，但尚未接入在线 compiler。

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
make eval-exposure
make eval-smoke
make paper
make logs
docker compose down
```

`make verify` 会执行格式检查、`go vet`、真实 PostgreSQL `go test -race ./...`、镜像构建和隔离的 Compose 端到端验收。

`make eval-exposure` 运行可审计的 ground truth、1,024 对等价改写、
anti-arbitrage cases、计费基线与双预算 planner oracle。它不会把尚未执行的
外部 PostgreSQL 开销实验伪装成结果。`make paper` 构建新的 TKDE 工作稿；
旧的安全网关稿仍可用 `make paper-tdsc` 构建。

设置 `GATEWAY_RESULT_RETENTION_TTL` 会让 Gateway 定期删除超过保留期的结果密文，同时保留查询记录、回执和审计证据。每个结果密文还绑定 `GATEWAY_DATA_KEY_ID` 并登记在 `result_encryption_keys`；带 `GATEWAY_ADMIN_TOKEN` 的管理员可以擦除 key ID，使对应密文保留但后续读取 fail closed。该 Demo 不销毁外部 KMS 中的真实 key material，生产环境需要把 key ID 擦除接入 KMS/HSM/Secret Manager。设置 `GATEWAY_ADMIN_TOKEN` 也会启用本机管理员接口，用于手动 purge 以及设置/释放 legal hold；active hold 会阻止对应任务的结果密文被清理。

查询回执验证方可读取 `/.well-known/taskgate/query-receipt-keyring.json`，获得 `taskgate-query-receipt-keyring/v1` 公钥 Bundle。Bundle 包含 active Gateway Key ID、历史验签公钥以及 `valid_from`/`retired_at` 窗口，不包含私钥材料。

设置 `GATEWAY_AUDIT_ANCHOR_URL` 会让 Gateway 定期把当前审计 Hash Chain checkpoint 签名为 `taskgate-audit-checkpoint-anchor/v1` 并 POST 到外部日志或 WORM 服务。该外部服务的保留和不可篡改性由部署环境保证。

`docker compose down` 保留 `control-pg-data` 与 `business-pg-data`；`docker compose down --volumes` 会删除当前 Compose 项目的这两个 Volume。旧版本的 `gateway-data` Volume 已不再挂载，也不会被本次改造自动删除，可按需手工备份或清理。

## 文档

- [架构与安全边界](docs/architecture.md)
- [任务级 Exposure 语义、在线算法与支持边界](docs/exposure-accounting.md)
- [Compose 启动、Navicat 与 MCP 演示](docs/getting-started.md)
- [Catalog 编写指南](docs/catalog-guide.md)
- [OA 与数据源适配器接口](docs/adapters.md)
- [SQL 与 QueryPlan 安全规则](docs/sql-security.md)
- [威胁模型与生产化差距](docs/threat-model.md)
