# TaskGate: Task-Bound Query-Outcome and Exposure Accounting for Database Agents

TaskGate 是一个数据库研究原型：它把累计数据暴露绑定到人类授权的根任务，使自适应查询、重试、分页和子 Agent 共享同一知识账本。Agent 必须先提交明确的数据产品、字段、Scope 和目的；Gateway 从 Catalog 绑定完整预算 Profile，经 OA 审批后才允许查询只读数据产品。Gateway 不包含模型层；授权、provenance、计量和结算均由确定性代码完成。

人类审批的是 Catalog 预定义的完整三维容量，正常批准后全部交给 Agent。系统不自动校准“最小预算”，也不把未使用额度视为优化目标；唯一的 exposure admission 条件是提交后的共享 root-family ledger 不超过人类签名边界。

> 当前仓库是单实例 Demo，不是可直接上线的生产网关。生产差距见[威胁模型与生产化差距](docs/threat-model.md)。

## 核心模型

每个事实至少绑定 `(product, snapshot, entity key, field, value version)`。
根任务维护三个集合账本：实际交付的 `release exposure`、保守的
`positive-output dependency footprint`。后者在 API、数据库和回执中继续使用
兼容标签 `influence`，但表示按 V2 规则参与已交付正向输出推导的 row/cell
facts，不是最小 causal influence，也不是完整 physical read set；第三个
`query outcome` 把规范化 QueryPlan 命题绑定到发布结果摘要，因此两个不同
阈值即使都回答空集或 `0`，仍会分别收费。一次查询只
支付相对根任务已计量集合的新事实：

```text
delta(T, q) = (|F_release(q) - K_release(T)|,
               |F_influence(q) - K_influence(T)|,
               |F_outcome(q) - K_outcome(T)|)
```

V4 把 canonical FactID 语义精确编码为冻结 snapshot 的 ordinal bitmap；少量派生 release/outcome 使用动态字典。在线路径是 `reserve -> replay lookup / execute+stream -> derive bitmap -> three-head CAS -> release`。
可见结果与 ordinal provenance companion 在同一个只读 `REPEATABLE READ` 事务执行；只有三维账本、加密结果、审计和 V6 签名回执原子提交后，结果才会释放。
超出任一 exposure 上限的结果不会交付或保存。

实现范围和非目标见[任务级数据暴露记账](docs/exposure-accounting.md)。

## 架构概览

```text
冻结 Reporting Snapshot + Candidate Catalog
                 │ offline compile
                 ├── Business PG ordinal sidecar
                 └── HOT hash/ordinal + COLD payload ── publication manifest
                                                        │
Agent ──► Gateway ── authorize / semantic replay ───────┤
             │ miss: visible SQL + streamed ordinal companion
             ▼
       exact weighted bitmap effect
             ▼
Control PG: ANDNOT + popcount + one R/I/O root-head CAS
             ▼
       commit result/audit/V6 receipt, then release
```

两个 PostgreSQL 使用独立容器、账号和 Volume。所有子 Agent 解析到同一个 root head；CAS 冲突会重读并重新计算三个维度，不能分维提交。Gateway 仍按单实例部署，本版本不提供多 Gateway 执行租约协议。

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

Compose 和 Gateway 二进制默认把 `GATEWAY_CONNECTOR_MAX_ROWS` 设为
`1200000`，使 345,000 行 provenance 的 V4 maximum-point workload 不会被
connector ceiling 截断。降低这个值是显式的部署取舍；超限时系统会关闭式拒绝，
不会释放部分结果。

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
2. 调用 `request_data_task`，显式提交非空 `objective`、`data_products`、每个产品的非空 `columns` 及 `scopes`。Gateway 按最高敏感级别选择 Catalog Profile，并把完整 Profile 写入审批 Manifest；Agent 不选择或优化预算。
3. 在 OA 提交并完成人工审批。正常批准把整个签名额度交给 Agent；系统不奖励未使用额度，安全边界按额度全部耗尽计算。
4. ACTIVE 后使用 `execute_plan(task_id, request_id, plan)`。默认 Catalog 已启用 V4：Product 绑定已验证的 `snapshot_publication`，路径生成同快照 ordinal provenance、构造 OutcomeFact 并进行精确 bitmap 三维结算。
5. 子 Agent 任务通过 `parent_task_id` 和 `delegate_principal_id` 创建，所有授权维度只能收缩，且共享根账本。每次查询仍提交一个确定的 QueryPlan。

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
`request_id` 只观察首次终态；新的 request ID 重放同一规范化命题和结果时三维
增量均为零，不同命题即使得到相同空/零结果也会新增 outcome 费用。

`query_sql` 仍用于 resource-only 兼容 Grant；对启用 exposure 的任务，它会因
无法构造完整 provenance 而关闭式拒绝。在线精确计量片段除单产品
projection/filter/order/limit/offset 和 `COUNT(*)/COUNT(column)/SUM/MIN/MAX`
外，还支持两种受限 `from`：两个不同 Catalog 稳定角色之间的 INNER
equijoin，以及同一产品两个过滤分支的 `union_distinct`。二者都能继续分组与
聚合；嵌套 Join/Union、self-join、`UNION ALL` 和多输入分页关闭式拒绝。

Join 字段使用 Catalog 稳定角色限定；`role` 不是任意 SQL alias：

```json
{
  "from": {
    "join": {
      "left": {"product": "expense_detail", "role": "expense_detail"},
      "right": {"product": "expense_summary", "role": "expense_summary"},
      "on": [{"left": "expense_detail.department", "right": "expense_summary.department"}]
    }
  },
  "columns": ["expense_detail.receipt_no", "expense_summary.total_amount"]
}
```

`union_distinct.columns` 是完整去重 tuple；即使最终 `columns` 隐藏其中字段，
这些字段仍参与 dependency：

```json
{
  "from": {
    "union_distinct": {
      "role": "expense_summary",
      "columns": ["department", "month"],
      "left": {"product": "expense_summary", "role": "left_branch", "filters": [{"column": "expense_type", "op": "=", "value": "机票"}]},
      "right": {"product": "expense_summary", "role": "right_branch", "filters": [{"column": "expense_type", "op": "=", "value": "酒店"}]}
    }
  },
  "columns": ["expense_summary.department"]
}
```

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

`make eval-exposure` 运行可审计的 ground truth、由独立 oracle 校验的
1,024 个唯一 PostgreSQL 结果等价改写（补充性压力测试，不作为 exposure invariance 证据）、
anti-arbitrage cases 和计费基线。`evaluation/exposure-performance/results.json` 保存三次独立
PostgreSQL 全路径 trial 的 31,296 个 RQ4 观测；该结果限定为本地十行 fixture，
不冒充 TPC 或生产规模。`make paper` 构建新的 TKDE 工作稿；
旧的安全网关稿仍可用 `make paper-tdsc` 构建。

`evaluation/v4-acceptance/config.example.json` 同样只演示本仓库十行冻结
Catalog 的 case schema 与四种 plan shape，不包含 12 Release / 1,035,000
Influence maximum point。用它运行时相应 SLO gate 必须保持 `unmeasured`；论文
验收必须换成独立冻结的大规模 publication、真实 ACTIVE task IDs 与校准后的
0/50/90/100% overlap cases。

设置 `GATEWAY_RESULT_RETENTION_TTL` 会让 Gateway 定期删除超过保留期的结果密文，同时保留查询记录、回执和审计证据。每个结果密文还绑定 `GATEWAY_DATA_KEY_ID` 并登记在 `result_encryption_keys`；带 `GATEWAY_ADMIN_TOKEN` 的管理员可以擦除 key ID，使对应密文保留但后续读取 fail closed。该 Demo 不销毁外部 KMS 中的真实 key material，生产环境需要把 key ID 擦除接入 KMS/HSM/Secret Manager。设置 `GATEWAY_ADMIN_TOKEN` 也会启用本机管理员接口，用于手动 purge 以及设置/释放 legal hold；active hold 会阻止对应任务的结果密文被清理。

查询回执验证方可读取 `/.well-known/taskgate/query-receipt-keyring.json`，获得 `taskgate-query-receipt-keyring/v1` 公钥 Bundle。Bundle 包含 active Gateway Key ID、历史验签公钥以及 `valid_from`/`retired_at` 窗口，不包含私钥材料。

设置 `GATEWAY_AUDIT_ANCHOR_URL` 会让 Gateway 定期把当前审计 Hash Chain checkpoint 签名为 `taskgate-audit-checkpoint-anchor/v1` 并 POST 到外部日志或 WORM 服务。该外部服务的保留和不可篡改性由部署环境保证。

`docker compose down` 保留 `control-pg-data`、`business-pg-data` 与不可变
`snapshot-index-artifacts` 和仅保存认证密文临时文件的 `gateway-encrypted-spool`；`docker compose down --volumes` 会删除当前 Compose 项目的这四个 Volume。旧版本的 `gateway-data` Volume 已不再挂载，也不会被本次改造自动删除，可按需手工备份或清理。

## 文档

- [TaskGate V4：Snapshot-Indexed Hybrid Bitmap Ledger](docs/exposure-v4.md)
- [架构与安全边界](docs/architecture.md)
- [任务级 Exposure 语义、在线算法与支持边界](docs/exposure-accounting.md)
- [Compose 启动、Navicat 与 MCP 演示](docs/getting-started.md)
- [Catalog 编写指南](docs/catalog-guide.md)
- [OA 与数据源适配器接口](docs/adapters.md)
- [SQL 与 QueryPlan 安全规则](docs/sql-security.md)
- [威胁模型与生产化差距](docs/threat-model.md)
