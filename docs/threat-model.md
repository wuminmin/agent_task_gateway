# 威胁模型与生产化差距

## 范围与资产

要保护的资产包括：

- `legacy.*` 业务数据及 Reporting View 返回的明细和汇总结果。
- Alice/Carol MCP Token、OA 服务 Token、回调/会话密钥和两个 PostgreSQL 的密码。
- TaskGrant、审批凭证、资源预算、根任务双 exposure 账本、不可变 FactID、查询凭证与审计 Hash Chain。
- AES-256-GCM 数据密钥及控制 PostgreSQL 中的加密查询结果。

Agent 提交的结构化申请、QueryPlan、SQL、浏览器请求、网络回调和业务数据都视为不可信。Catalog、Gateway 二进制、Docker 宿主机以及可读取 `.env`/Volume 的管理员属于可信运维域；该域失陷后，本 Demo 无法保证机密性或不可否认性。

## 主要威胁与现有控制

| 威胁 | 当前控制 | 残余风险 |
|---|---|---|
| Agent 越权读取其他主体任务/结果 | Bearer Token 映射固定 Principal；任务和结果同时校验 principal、task、query 与 actor；越权按不存在返回；Principal 禁用后 Gateway 不再列出或执行工具 | 长期静态 Token 没有设备绑定、短期会话、组织级权限或外部会话吊销发布 |
| Carol 读取敏感原始结果 | Carol 只注册两个审计工具，凭证不含结果行 | 拥有控制库和数据密钥的管理员可以解密；审计元数据可能包含目标和 OA 信息 |
| 客户端伪造授权字段 | 身份字段由 Gateway 生成；产品、字段和 Scope 必须显式且非空；OA 展示并绑定 RFC 8785 `AuthorizationManifestV1` 摘要 | 不验证自然语言目标与查询语义；审批人仍须核对结构化 Manifest |
| 恶意 QueryPlan | 本地编译器验证产品、字段、聚合、过滤、排序、literal 与 Limit，随后进入完整 SQL AST 策略 | 编译器或策略实现缺陷仍需模糊测试和攻击语料覆盖 |
| 通过拆分查询、重叠分页或新 request ID 重复提取数据 | release/influence 以根任务已知 FactID 集合结算；唯一键只计 novel facts；终态 request replay 不重执行 | 只覆盖已定义 FactID 与正向 provenance；不限制输出顺序、空结果等负信息推断 |
| 聚合把百万来源伪装成一个返回行 | release 与 source influence 分账；聚合输出只占少量 release，但所有正向贡献来源进入 influence | 当前 lineage 物化有高空间和延迟成本；尚无 sketch、压缩或近似模式 |
| 子 Agent 通过委托重置预算 | 签名 root/parent lineage、逐维 Grant narrowing、每次执行验证祖先 ACTIVE；整个任务族共享 root ledger | 默认 Demo 只有一个 query Principal；更大委托 DAG 只有有限模型与实现测试，尚无大规模实验 |
| 并发子任务同时花费最后额度 | 根 exposure ledger `FOR UPDATE` 串行结算；FactID 唯一键、双上限条件更新、结果持久化处于同一事务 | 单一根 ledger 会成为高并发热点；当前没有分片或分布式 settlement 协议 |
| 可见结果与 provenance 观察不同数据版本 | 两条策略 SQL 在一个只读 `REPEATABLE READ` 事务执行；证据缺失或截断时结果不释放 | Catalog snapshot 是管理员版本标签，未接 CDC；错误 entity key 或漏升 snapshot 会破坏跨查询语义 |
| 通过直接 SQL 绕开 exposure compiler | 启用 exposure 的 Grant 要求结构化 `execute_plan` 和完整 observation；`query_sql` 关闭式拒绝 | 在线 compiler 尚不支持 Join、Union、AVG、窗口函数或任意 SQL provenance |
| SQL 注入、注释绕过或写操作 | PostgreSQL AST 单语句白名单、逻辑产品/字段/函数/运算符限制、Scope CTE、外层行限制 | Parser/策略缺陷和依赖漏洞仍可能存在 |
| 策略失误后修改业务库 | 非 owner、非 superuser、无 `BYPASSRLS` 的独立 `gateway_reader`；角色/连接/事务只读；仅 Datasource Attestation 表与 Reporting View `SELECT`；Schema Attestation | DBA 误授权、View 内容语义错误或数据库漏洞可绕过应用意图 |
| OA 回调伪造、重放或乱序 | HMAC-SHA256 认证原始 Body；Event ID/状态/context/actor 校验；独立 OA Ed25519 回执绑定 Manifest 与最终 Grant；Gateway 可配置多把 OA 验签公钥及有效/退役窗口；事务幂等 | Demo OA Outbox 不持久；KMS、密钥分发和吊销发布仍需外部运维 |
| 并发/重试查询超资源预算 | 控制 PG 对任务和资源预算加行锁；`(task_id, request_id)` 唯一；预留与结算在事务中；同一任务一个在途查询 | 单 Gateway 没有跨实例执行租约，不能安全横向扩容 |
| Gateway 崩溃遗留预算/回调 | 启动恢复将 RESERVED 查询按资源预留保守计费并标记 `INDETERMINATE`，释放未结算 exposure reservation，写入查询回执，并禁止同 request ID 重执行；结果只在结算提交后释放 | 资源计费可能高于实际消耗；不声称立即取消已在途查询；瞬态缓冲不具备跨进程恢复 |
| 查询回执伪造或历史密钥混淆 | V4/V5 成功回执绑定 exposure observation/charge、V2 planner evidence、`signed_at`、Gateway Key ID 和终态审计位置；无 exposure evidence 的兼容终态使用 V3；Gateway 发布带有效/退役窗口的公钥 Bundle | Keyring 发布仍依赖 Gateway 配置和部署重启；没有集中 KMS/HSM、透明日志或外部撤销服务 |
| 并发审计链分叉或序列空洞 | 单行 `audit_chain_head` 通过 `SELECT ... FOR UPDATE` 串行化；连续序号、事件和链头在同一事务提交；可配置 `taskgate-audit-checkpoint-anchor/v1` 签名 checkpoint POST 到外部日志/WORM 服务 | 外部 Anchor 服务本身的保留、不可篡改性和时间戳可信度依赖部署；高写入量时链头是串行瓶颈 |
| Grant/审计记录被应用修改 | PostgreSQL Trigger 禁止 UPDATE/DELETE，逐事件 SHA-256 链可验证 | 超级用户可禁用 Trigger 并重建整条链 |
| 磁盘结果泄露或篡改 | AES-256-GCM 随机 Nonce，AAD 绑定 task/query，读取时校验明文 SHA-256；每个结果行保存 `key_id` 并登记在 `result_encryption_keys`；结果保留清理可按 TTL 调度或管理员截止时间删除密文并保留查询/回执/审计证据；管理员 key ID 擦除会把 key 标记为 `ERASED`、追加审计事件，并让现有密文读取 fail closed；active legal hold 阻止密文清理 | 任务、Scope、Grant、预算与审计元数据未加密；本 Demo 的 AES key 仍位于环境变量和进程内存；实际 key material 销毁、轮换、集中 KMS/HSM custody 和撤销透明性仍需外部平台 |
| 本机网络窃听 | Gateway、OA 和 Control PG 绑定 `127.0.0.1`；Business PG 默认仅内部网络 | 本地仍使用明文 HTTP/PG；同机恶意进程可尝试连接或截获凭据 |
| 浏览器会话伪造与 CSRF | 签名 Cookie、bcrypt、`HttpOnly`、`SameSite=Lax`、CSRF Token、CSP | 本地 HTTP Cookie 无 `Secure`；无 MFA、锁定、SSO 与登录审计 |
| DoS 与成本滥用 | HTTP Body/Timeout、连接池、数据库 Timeout、任务预算和结果行上限 | 目录和任务申请缺少租户配额、速率限制和异常检测 |
| 容器逃逸或供应链攻击 | 非 root、只读根文件系统、`no-new-privileges` | 未固定所有镜像 Digest，缺少定制 seccomp/AppArmor、签名和 SBOM 门禁 |

## 数据最小化与边界

Gateway 不向外部模型或翻译服务发送目标、Catalog、SQL 或结果。所有任务申请和 QueryPlan 校验均在本地确定性代码中完成。

查询结果会以明文返回给获批 Alice 的 MCP 客户端；数据库加密只保护控制库中的静态结果，不是端到端显示控制。终端历史、截图、Agent 工具或用户后续复制均在 Gateway 边界之外。

`exposure_facts.identity_json` 保存产品、snapshot、Hash 化 entity key、字段名和
value-version Hash，不保存原始值；但它仍是敏感访问元数据。事实账本在当前
原型中不可删除，未与用户删除请求、保留期限或跨 snapshot 压缩策略集成。

默认论文部署不向宿主机发布 Business PostgreSQL 端口；只有 Gateway 所在内部网络可达。`compose.debug.yaml` 是明确标记的非论文调试覆盖，可将业务库绑定到宿主机回环地址。控制库与业务库使用不同数据库、角色、密码和 Volume，业务只读账号无法访问控制库。

## 需要运营配合的边界

- Catalog 管理员必须保证字段的业务语义正确、`entity_key` 在 snapshot 内稳定唯一、数据更新时提升 snapshot，且 View 不包含未发布敏感列；自动 Attestation 只校验列顺序与 PostgreSQL 类型，不能证明这些语义。
- `.env` 应保持 Git 忽略并收紧文件权限；数据密钥需与控制库备份一起安全管理。
- Catalog 升级前应排空或重新申请 ACTIVE 任务，因为版本不一致会关闭式拒绝查询。
- 应监控回调重试、启动恢复、数据库 Timeout、预算耗尽、结算重试、readiness 和审计链验证。
- 两个 PG Volume 必须分别备份和恢复演练，避免误把系统控制数据与业务数据混作同一生命周期。

## 生产化差距

### 身份、网络与秘密

1. 用 OIDC/OAuth 2.0、短期 Token、Audience/Scope、租户绑定和集中撤销替代静态 Token。
2. Gateway、OA 与数据库全部启用 TLS/mTLS，并置于受控网络与反向代理后。
3. 使用 KMS/HSM 或 Secret Manager 做 Envelope Encryption，保存 Key ID，支持轮换，并把 key ID 擦除操作绑定到真实 key material 销毁与透明撤销日志。
4. 使用工作负载身份或编排平台 Secret，避免秘密长期存在于 `.env` 和进程环境。

### 持久化与高可用

1. 持久化 OA 草稿、用户、Outbox 和审批历史，提供双向对账与死信队列。
2. 在横向扩容 Gateway 前实现跨实例、可续租且可恢复的每任务执行租约；只有数据库行锁不足以覆盖业务查询执行窗口。
3. 将结果保留清理的 TTL、管理员操作和 legal hold 接入组织级工单、审批和告警流程。
4. 对控制库和业务库分别建立最小权限、备份、时间点恢复和一致性演练。

### 审计与策略

1. 将签名 Audit Head Anchor 接入企业级 WORM/透明日志、可信时间戳和告警流程，并监控 Anchor 失败。
2. 启动及周期性验证 Hash Chain，失败时停止敏感查询并告警。
3. 将 Catalog、Reporting View、角色 Grant 和数据库迁移作为同一个受审发布单元，并做 Schema/类型校验。
4. 对 QueryPlan 编译器、SQL Parser、Scope 注入和渲染器持续做 Fuzz、属性测试及 PostgreSQL 版本兼容测试。
5. 增加管理员撤销/暂停、紧急 Kill Query、主体/产品级全局预算和速率限制。
6. 将 snapshot 发布接入 CDC/版本流水线，为 FactID 元数据建立保留、压缩和受审删除策略。
7. 扩展并验证 Join、Union、窗口和多引擎 provenance；在此之前继续关闭式拒绝。
8. 如需防止空结果、顺序、相关查询或外部知识产生的推断，应叠加差分隐私、查询审计或领域专用推断控制；当前 exposure ledger 不提供这些保证。

### 平台加固与可观测性

1. 增加结构化指标、分布式 Trace、告警、SLO、容量测试和故障演练。
2. 固定镜像 Digest，生成 SBOM、签名并接入漏洞扫描。
3. 显式 drop Linux Capabilities，配置 seccomp/AppArmor、资源配额和网络策略。
4. 为 MCP 与 OA 增加速率/并发限制、异常检测、Body/响应配额和防暴力登录。

## 上线门槛

接入真实企业数据前，至少应完成企业身份与 TLS、集中密钥管理、持久 OA/Outbox、多 Gateway 租约、外部审计锚、Catalog/View 联合发布、攻击测试、双库备份恢复演练、数据保留策略和独立安全评审。
