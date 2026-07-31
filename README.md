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

V4 把 canonical FactID 语义精确编码为冻结 snapshot 的 ordinal bitmap；少量派生 release/outcome 使用动态字典。在线路径是 `reserve -> replay lookup / execute+stream -> derive bitmap -> stage encrypted Parquet -> three-head CAS -> canonical promotion`。
可见结果与 ordinal provenance companion 在同一个只读 `REPEATABLE READ` 事务执行。Gateway 在对象存储客户端侧加密 Parquet；Control PostgreSQL 只提交账本、artifact 元数据、审计和 V6 签名回执，不保存 Parquet 或结果行。确定性的 canonical 对象创建成功即记为 `consumed/AVAILABLE`，随后普通查询只向 Agent 返回 `result_id` 与摘要。超出任一 exposure 上限的结果不会产生 canonical 对象。

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
 commit PENDING artifact metadata/audit/V6 receipt
             ▼
 S3/MinIO: promote encrypted Parquet canonical object
             ▼
       consumed/AVAILABLE → result_id + summary
```

两个 PostgreSQL 使用独立容器、账号和 Volume；S3 兼容对象存储使用独立的内部网络、bucket-scoped Gateway 凭据和持久 Volume。result bucket 必须私有且禁用 versioning，以保证 TTL/purge 删除的是实际 bytes，而不只是 delete marker。所有子 Agent 解析到同一个 root head；CAS 冲突会重读并重新计算三个维度，不能分维提交。Gateway 仍按单实例部署，本版本不提供多 Gateway 执行租约协议。

## 快速启动

宿主机只需要 Docker Engine 与 Docker Compose v2：

```bash
cp .env.example .env
# 按 docs/getting-started.md 生成加密密钥和两个独立 Ed25519 密钥
# 同时替换 .env 中的全部 Token、共享密钥、对象存储和数据库密码
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
| Parquet 对象存储 | 仅 `result-storage` 内部网络 / `taskgate-results` |

如需本机数据库客户端调试，可显式启用非论文部署覆盖：

```bash
docker compose -f compose.yaml -f compose.debug.yaml up --build -d --wait
```

Navicat 的用户名和密码对应关系见[本地启动与数据库调试](docs/getting-started.md#navicat-连接参数)。Business PostgreSQL 与 MinIO API/Console 仅在显式启用调试覆盖时绑定宿主机回环地址。

## MCP 2.0 工作流

1. 调用 `list_data_products`、`describe_data_product` 和 `get_sql_capabilities`，获取逻辑产品、字段、稳定角色、Scope 以及 `taskgate-reporting-sql-v1` 能力边界。
2. 调用 `request_data_task`，显式提交非空 `objective`、`data_products`、每个产品的非空 `columns` 及 `scopes`。Gateway 按最高敏感级别选择 Catalog Profile，并把完整 Profile 写入审批 Manifest；Agent 不选择或优化预算。
3. 在 OA 提交并完成人工审批。正常批准把整个签名额度交给 Agent；系统不奖励未使用额度，安全边界按额度全部耗尽计算。
4. ACTIVE 后使用 `query_sql(task_id, request_id, sql)`。对启用 exposure 的 Grant，Gateway 只接受能无损转换为 canonical QueryPlan 的报表 SQL；成功后只执行从该计划重新生成的 visible SQL 和 provenance companion。
5. 生成计划继续使用同快照 ordinal provenance、V4 FactID/三维 bitmap 结算、结果 withholding、semantic replay 和 V6 receipt 链路。子 Agent 任务通过 `parent_task_id` 和 `delegate_principal_id` 创建，所有授权维度只能收缩，且共享根账本。

`request_id` 由客户端生成并在一个任务内保持唯一。相同 ID 和相同请求只返回首次持久化结果/状态；相同 ID 搭配不同请求会关闭式拒绝，重试不会产生第二次执行或预算消费。

`query_sql` 的最小 SQL：

```sql
SELECT month, SUM(total_amount) AS amount
FROM expense_summary
GROUP BY month
ORDER BY month ASC
LIMIT 20;
```

普通 Agent 的 `tools/list` 不默认列出 `execute_plan`。该入口仍保留给 SDK、内部测试、基准、调试和确定性工作流；它与 SQL lowering 共用唯一 QueryPlan 编译/记账边界，不是策略旁路。

成功响应包含 `exposure` 与 `exposure_budget`：前者区分本次 actual facts 和
相对根任务的 charged facts，后者给出共享账本的上限、已用和剩余值。相同
`request_id` 只观察首次终态；新的 request ID 重放同一规范化命题和结果时三维
增量均为零，不同命题即使得到相同空/零结果也会新增 outcome 费用。

`query_sql`、`execute_plan` 和 `get_query_result` 的普通响应不包含 `rows` 或对象键，而是返回 `result_id`、列定义、`row_count`、`column_count`、`expires_at` 与计费摘要。需要检查少量数据时调用 `preview_result(result_id, offset, limit)`，其中 `limit` 最大为 100；需要完整文件时调用 `deliver_result(result_id, format="parquet")` 获取默认 5 分钟的短期下载 URL。下载是否发生、下载次数和 Agent Host 的临时副本都不会改变预算、`consumed` 状态或 Receipt；消费边界已经是 canonical Parquet 对象创建成功。非 loopback 的 `GATEWAY_PUBLIC_BASE_URL` 必须使用 HTTPS，日志/APM 必须脱敏 capability URL 中的 `token`。

`taskgate-reporting-sql-v1` 支持单产品
projection/filter/order/limit/offset 和 `COUNT(*)/COUNT(column)/SUM/MIN/MAX`，以及 2–16 个不同 Catalog 稳定角色组成的任意 connected INNER equi-join graph 形状；每条 edge 可包含一个或多个 column-to-column equality predicate。Lowering 先将 SQL alias 解析为 Catalog 稳定角色，再对 graph 的 nodes、edges 和 predicates 规范排序并转换为现有 `join_many`；下游将其 deterministic binary fold 为现有二元 Join 代数。16-source 上限是限制生成 SQL 宽度、provenance 行和 PostgreSQL planning work 的 operational complexity/DoS ceiling，不限制上述 graph 形状，并支持 10 表 Join；请求还受 1 MiB MCP 请求体、PostgreSQL AST 解析/白名单校验和现有资源预算与超时/行数上限约束。Self-join、outer/cross/non-equality join、断开的 join graph、子查询、CTE、set operation、窗口、`HAVING` 和多输入分页都在 SQL profile 外，Gateway 不会把 `LEFT JOIN` 静默改成 `INNER JOIN`。Resource-only Grant 继续使用现有安全 SQL policy，但不会因此扩大 exposure SQL profile。

SQL 语法、授权或 lowering 失败在业务数据库执行和正式 reservation 之前返回结构化、可修复的错误，不扣查询、DBMS、release、dependency 或 outcome 预算。原始 SQL 派生的摘要只用于审计和 `(task_id, request_id)` 幂等；FactID、OutcomeFact 和 semantic replay 始终使用 canonical QueryPlan/`plan_digest`，不使用原始 SQL 文本哈希。

SQL alias 只是输入语法；lowering 会把它映射到 Catalog 稳定角色。高级 `execute_plan` 的 Join 字段直接使用该稳定角色：

```json
{
  "from": {
    "join_many": {
      "sources": [
        {"product": "expense_detail", "role": "expense_detail"},
        {"product": "expense_summary", "role": "expense_summary"}
      ],
      "on": [{"left": "expense_detail.department", "right": "expense_summary.department"}]
    }
  },
  "columns": ["expense_detail.receipt_no", "expense_summary.total_amount"]
}
```

`union_distinct` 仅是高级 QueryPlan 入口的现有能力，不属于 `taskgate-reporting-sql-v1` lowering。其 `columns` 是完整去重 tuple；即使最终 `columns` 隐藏其中字段，这些字段仍参与 dependency：

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
| Alice | MCP | 申请任务、查询自己的 ACTIVE 任务、读取结果元数据、有界预览、短期交付与凭证 | `TASKBOUND_ALICE_TOKEN` |
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
未投稿的旧安全网关工作稿仍可用 `make paper-tdsc` 构建。

`evaluation/v4-acceptance/config.example.json` 同样只演示本仓库十行冻结
Catalog 的 case schema 与四种 plan shape，不包含 12 Release / 1,035,000
Influence maximum point。用它运行时相应 SLO gate 必须保持 `unmeasured`；论文
验收必须换成独立冻结的大规模 publication、真实 ACTIVE task IDs 与校准后的
0/50/90/100% overlap cases。

新查询的 `result_artifacts` 行只保存 `result_id`、对象键、schema、行列数、ACL、TTL、明文/密文哈希、key ID 和生命周期状态；客户端侧加密的 Parquet 原件保存在 S3/MinIO。Control 事务先登记 `PENDING`，Gateway 再把 staging 对象幂等提升为 deterministic canonical 对象并标记 `AVAILABLE/consumed`；启动和后台恢复只在 staging 与已提交证据一致时继续 PENDING promotion，不重执行 SQL 或重复收费。staging 丢失或 canonical 证据冲突时 readiness fail closed，需先恢复对象证据或受审修复；当前不会自动放弃/退款。

当前研究原型的 Connector/可见结果到 Parquet 部分路径仍会在内存持有完整结果，`preview_result` 默认又对大于 64 MiB 的 artifact 关闭。百万行生产使用前仍需要有界流式 Parquet writer/reader 与容量基准；`GATEWAY_CONNECTOR_MAX_ROWS` 的高上限本身不是有界内存的证明。

设置 `GATEWAY_RESULT_RETENTION_TTL` 会让 Gateway 定期先删除超过保留期的 canonical 对象，再把 Control 元数据置为删除终态，同时保留查询记录、回执和审计证据。每个 artifact 绑定 `GATEWAY_DATA_KEY_ID` 并登记在 `result_encryption_keys`；带 `GATEWAY_ADMIN_TOKEN` 的管理员可以擦除 key ID，使保留对象后续读取 fail closed。该 Demo 不销毁外部 KMS 中的真实 key material，生产环境需要把 key ID 擦除接入 KMS/HSM/Secret Manager。管理员接口也支持手动 purge 以及设置/释放 legal hold；active hold 会阻止对应任务的对象被 TTL 或手动 retention 清理，但不会延长 artifact `expires_at` 或继续开放读取，也不会自动阻止独立的 key-erasure 审批流程。

查询回执验证方可读取 `/.well-known/taskgate/query-receipt-keyring.json`，获得 `taskgate-query-receipt-keyring/v1` 公钥 Bundle。Bundle 包含 active Gateway Key ID、历史验签公钥以及 `valid_from`/`retired_at` 窗口，不包含私钥材料。

设置 `GATEWAY_AUDIT_ANCHOR_URL` 会让 Gateway 定期把当前审计 Hash Chain checkpoint 签名为 `taskgate-audit-checkpoint-anchor/v1` 并 POST 到外部日志或 WORM 服务。该外部服务的保留和不可篡改性由部署环境保证。

`docker compose down` 保留 `control-pg-data`、`business-pg-data`、不可变
`snapshot-index-artifacts`、仅保存认证密文临时文件的 `gateway-encrypted-spool` 和保存规范加密 Parquet 的 `result-object-data`；`docker compose down --volumes` 会删除当前 Compose 项目的这五个 Volume。旧版本的 `gateway-data` Volume 已不再挂载，也不会被本次改造自动删除，可按需手工备份或清理。

## 文档

- [TaskGate V4：Snapshot-Indexed Hybrid Bitmap Ledger](docs/exposure-v4.md)
- [架构与安全边界](docs/architecture.md)
- [任务级 Exposure 语义、在线算法与支持边界](docs/exposure-accounting.md)
- [Compose 启动、Navicat 与 MCP 演示](docs/getting-started.md)
- [Catalog 编写指南](docs/catalog-guide.md)
- [OA 与数据源适配器接口](docs/adapters.md)
- [SQL 与 QueryPlan 安全规则](docs/sql-security.md)
- [威胁模型与生产化差距](docs/threat-model.md)
