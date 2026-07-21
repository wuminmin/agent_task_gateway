# TaskGate：IEEE TDSC 可执行任务计划

## 1. 固定研究路线

- 论文题目暂定：**TaskGate: Human-Approved, Budget-Constrained Authorization for Autonomous Database Agents**。
- 保留 **Go 外置 Gateway + Control PostgreSQL + Business PostgreSQL + OA 审批端**。
- 仅支持 PostgreSQL 16 的受限只读 `SELECT`；不做写操作、支付、PG Extension、通用 PG Wire Proxy 或 LLM 语义授权。
- 核心主张：人类批准的是一组未来查询及其累计执行预算；不可信 Agent 提交的每条查询都必须满足授权包络：
  \[
  q_i\in L(G),\qquad used_j+reserved_j\le limit_j
  \]
- 不称为“新认证框架”，统一使用“任务绑定授权与持续使用控制”。

## 2. 协议与实现任务

### 授权对象

新增版本化 `AuthorizationManifestV1`：

```text
version, task_id, human_subject, agent_id, declared_objective,
products, approved_columns, mandatory_scope, sensitivity,
budget {
  max_queries, max_result_rows, max_db_ms,
  per_query_timeout_ms, task_ttl_ms
},
catalog_version, catalog_sha256, callback_context, nonce
```

- 身份字段由 Gateway 从认证主体生成，Agent 不得自报。
- 使用 RFC 8785 规范化 JSON，摘要为：
  `SHA256("TASKGATE-MANIFEST-V1\0" || canonical_json)`。
- 增加固定测试向量，保证 Go、OA 和论文脚本得到相同摘要。
- 将 Grant 分成可收缩的 `TaskGrantCoreV1` 和附带审批证明的最终 `TaskGrantV1`。
- OA 允许 `approve/reject/narrow`；缩小产品、列、范围、期限和预算，禁止任何扩权。
- 把现有 `CheckNarrowing` 接入真实审批回调路径。

### 审批与回执

新增 OA Ed25519 签名的 `ApprovalReceiptV1`：

```text
version, receipt_id, task_id, decision,
manifest_digest, approved_grant_digest,
approver_id, issued_at, key_id, signature
```

- HMAC 回调继续负责传输认证和防重放；Ed25519 回执负责持久绑定审批结果。
- OA 签名代表“可信审批服务记录了已认证人类的决定”，不宣称个人法律不可否认性。
- 审批页面必须显示目标、数据产品、列、强制范围、敏感级别、各项预算、有效期和 Agent 身份。

### 查询执行

- `query_sql` 和 `execute_plan` 增加必填 `request_id`。
- Control PG 对 `(task_id, request_id)` 建立唯一约束：
  - 相同 ID、相同请求摘要：返回首次持久化结果或状态。
  - 相同 ID、不同摘要：关闭式拒绝。
  - 不得因客户端重试产生第二次预算消费。
- 固定执行顺序：身份与任务状态 → Grant/TTL/Catalog → SQL AST → 产品/列/函数/操作符 → Scope/Projection/Limit 重写 → 预算预留 → 只读事务执行 → 结算与回执。
- 每个任务只允许一个在途查询；不同任务可以并发。
- 新增 `revoke_task`：阻止新查询；已在途查询仅受超时和 Grant 到期约束，不宣称立即取消。
- 崩溃语义：
  - Connector 调用前确定失败：释放预留。
  - 已执行且结果确定：按实际用量结算。
  - 是否执行无法确定：按预留上限保守计费，标记 `INDETERMINATE`，禁止自动重执行。

### 查询回执与部署边界

新增 Gateway Ed25519 签名的 `QueryReceiptV1`，绑定：

```text
task_id, query_id, request_id,
manifest/grant/catalog digest,
request/sql fingerprint, policy decision,
budget before/reserved/charged/after,
row_count, db_ms, result_hash,
status/error, timestamps,
audit_sequence, previous_audit_hash,
gateway_key_id, signature
```

- Hash Chain 只声明可检测 Gateway 日志修改，不声明 WORM 或外部不可篡改。
- Catalog 增加预期 Reporting View 列和 PostgreSQL 类型；启动时对真实数据库做 Schema Attestation，漂移时 readiness 失败。
- Agent 永不持有 Business PG 凭据。
- Business PG 默认不发布宿主机端口；仅 Gateway 网络和最小权限、非 owner、非 superuser、无 `BYPASSRLS` 的只读角色可访问。
- 如需调试端口，放入明确标记为非论文部署的 Compose override。

## 3. 形式化、测试与评估

### 形式化模型

在 `formal/` 建立 TLA+ 模型，覆盖任务状态、审批回放、Grant 收缩、预算预留/结算、崩溃恢复、撤销、到期和 Catalog 漂移。

验证以下性质：

- 无有效审批不得执行。
- 每条执行查询的关系、列和范围均属于 Grant。
- Grant 只能单调收缩。
- `used + reserved <= approved limit` 始终成立。
- 重放和重复 `request_id` 不产生第二次执行。
- 撤销、过期和终态禁止新查询。
- Catalog/Schema 不一致时关闭式拒绝。
- TLC 有限状态模型不得出现不变量违反。

### 安全测试

- 保留并扩展现有 Docker 测试。
- 覆盖多语句、CTE、子查询、别名、集合操作、注释、危险函数、Scope 绕过、列扩张和 Parser 边界。
- 覆盖 Manifest、Grant、审批回执、查询回执的篡改、重放、乱序和错误 Key ID。
- 在 reserve、执行前、执行后、settle 前后分别注入 Gateway/Control PG 故障。
- 增加 Go fuzz、变形测试和真实 PostgreSQL 差分解析/执行测试。
- 增加直接连接 Business PG、底表访问、错误凭据和 Prompt Injection Agent 的负向测试。
- 最终 fuzz 累计不少于 24 CPU 小时；保存 seed、版本和完整日志。
- 验收要求：定义范围内零次未授权执行、预算不变量始终成立、无 panic、错误码稳定。

### 性能评估

使用 TPC-H/TPC-DS 的 SF1 和 SF10 数据，比较：

1. Direct PostgreSQL。
2. Native View/RLS。
3. AST-only Gateway。
4. 完整 TaskGate。

- 并发度为 1、8、32，均使用独立任务，避免违反单任务串行假设。
- 预热后每组至少 30 次测量。
- 报告吞吐量、p50/p95/p99、CPU、内存、Control PG 事务、回执存储和各组件耗时。
- 所有表格和图片必须由原始 JSON/CSV 自动生成，不手填数字。
- 初稿不进行真人研究，也不声称降低认知负担或审批疲劳；仅报告可计算的审批次数。后续只有获得伦理审批和真实参与者后才能加入人因结论。

## 4. 论文与交付验收

- 在 `paper/tdsc/` 编写 IEEE LaTeX 论文，结构为：Introduction、Related Work、Threat Model、Authorization Model、Design、Formal Analysis、Implementation、Evaluation、Limitations、Conclusion。
- Related Work 必须正面对比 OAuth RAR、TBAC/UCON、Blockaid、PAuth、Task Shield、AP2 和 bounded capability receipts；不得使用未经证明的 “first” 表述。
- 明确限制：不验证自然语言目标语义、不提供隐私预算、不支持写操作、任意 SQL、多 Gateway、立即撤销在途查询或法律不可否认性。
- 在 `evaluation/` 保存工作负载、攻击语料、实验驱动器、环境清单、原始结果和绘图脚本。
- 提供 Docker 化命令：
  - `make verify`：全部功能与安全测试。
  - `make formal`：TLC 验证。
  - `make eval-smoke`：小规模四基线实验。
  - `make eval-full`：完整可复现实验。
  - `make paper`：从原始结果生成图表并编译论文。
- 最终验收：
  - 干净环境可按文档复现上述命令。
  - 论文中的每个实验数字能追溯到原始结果。
  - 代码、Threat Model、形式化性质和论文声明完全一致。
  - 不伪造实验、人类参与者或引用结果。
  - 未经用户明确要求，不提交论文、不推送远端、不创建发布版本。

## 默认假设

- 使用 Docker 执行 Go、PostgreSQL、TLA+ 和 LaTeX 工具，避免依赖宿主机环境。
- 论文系统限定单 Gateway 实例、每任务单在途查询。
- OAuth/OIDC 或企业身份系统负责基础身份认证；TaskGate 研究的是认证后的任务授权。
- 现有用户改动必须保留，实施前先记录 Git 状态和可复现基线。
