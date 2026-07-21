# 威胁模型与生产化差距

## 范围与资产

要保护的资产包括：

- `legacy.*` 业务数据及 Reporting View 返回的明细/汇总结果。
- Alice/Carol MCP Token、OA 服务 Token、回调共享密钥、会话密钥、数据库密码和 DeepSeek Key。
- TaskGrant、审批凭证、预算账本、查询凭证和审计 Hash Chain。
- AES-256-GCM 数据密钥及 SQLite 中的加密查询结果。

本模型把 Agent 输入、模型输出、Agent SQL、浏览器请求、网络回调和业务数据都视为不可信。Catalog、Gateway 二进制、Docker 宿主机和可读取 `.env`/Volume 的管理员属于可信运维域；若该域失陷，Demo 无法保证机密性或审计不可否认性。

## 主要威胁与现有控制

| 威胁 | 当前控制 | 残余风险 |
|---|---|---|
| Agent 越权读取其他人的任务/结果 | MCP Bearer Token 映射固定 Principal；Alice 的任务查询同时校验 `principal_id`、`task_id`、`query_id` 与 actor；越权时按不存在返回 | Token 是长期静态共享秘密，无设备/会话绑定、撤销列表或细粒度组织权限 |
| Carol 读取敏感原始结果 | Carol 只注册 `list_audit_events`、`get_audit_receipt`；凭证不含结果行 | 拥有宿主机/SQLite/密钥权限的管理员仍可解密；审计事件可能含任务目标和 OA 元数据 |
| Prompt Injection 让模型扩权 | 模型仅产生候选严格 JSON；Catalog 决定敏感级别、Scope、审批、预算；规范化字段/范围/预算由 OA 页面展示并绑定快照 Hash；QueryPlan 再由 Go 编译和 AST 策略校验 | 用户目标与问题会发送给第三方 DeepSeek；审批人仍须核对页面中的授权快照，不应只阅读自然语言标题 |
| SQL 注入、注释绕过或写操作 | PostgreSQL AST 单语句白名单、逻辑产品/字段/函数/运算符限制、Scope CTE、外层行限制 | Parser/策略缺陷仍可能存在；需要持续模糊测试、攻击语料和依赖升级 |
| 策略失误后修改数据库 | 专用 `gateway_reader`、只读角色、只读连接和事务；仅 Reporting View `SELECT` | 数据库管理员误授权、View 误暴露列或数据库漏洞可绕过应用层意图 |
| OA 回调伪造/重放/乱序 | HMAC-SHA256 原始 Body 签名、Header/Body ID、双时间窗口、草稿/版本/context/actor/state/授权快照 Hash 校验、SQLite 幂等事务 | 共享密钥无轮换/Key ID；Demo OA Outbox 不持久，最终失败只写日志 |
| 并发查询超预算 | SQLite 事务先预留查询/行/DB 时间，同一任务只允许一个查询；结算释放未用部分，硬上限自动归档 | SQLite 设计仅支持单 Gateway 实例；多实例会需要集中式事务数据库和分布式租约 |
| Gateway 崩溃或 SQLite 短暂故障遗留预算/回调 | 运行期结算后台幂等重试并使 readiness 失败；启动恢复把 RESERVED 查询按完整预留量保守计费并标记 INTERRUPTED，把 PROCESSING 回调置为可重试 | 保守计费可能高于实际消耗，需要运营对账/申诉；进程在 PostgreSQL 执行期间崩溃时，取消仍依赖连接断开 |
| 磁盘上的结果泄露/篡改 | AES-256-GCM 随机 Nonce；AAD 绑定 task/query；保存明文 SHA-256 并在读取时验证 | 只有查询结果加密，任务目标、Scope、Grant、预算与审计元数据仍明文；密钥放在容器环境变量，无 KMS/轮换/版本 |
| 审计记录被改写 | SQLite UPDATE/DELETE Trigger、逐事件 SHA-256 链、Carol 可读取 previous/current Hash | Hash Chain 没有外部时间戳/签名锚；拥有 DB 文件和代码执行权限的管理员可重建整条链；启动/读取不自动强制验证全链 |
| 网络窃听或本机恶意进程 | Gateway/OA 端口只绑定 `127.0.0.1`；PostgreSQL 不发布宿主机端口 | MCP/OA Demo 使用明文 HTTP；同一宿主机上的恶意进程或代理仍可能截获 Token/数据 |
| 浏览器会话伪造与 CSRF | 签名 Session Cookie、bcrypt 密码 Hash、`HttpOnly`、`SameSite=Lax`、CSRF Token、CSP、`nosniff`、无 Referrer | 本地 HTTP 下 Cookie 没有 `Secure`；没有 MFA、锁定、审计登录或企业 SSO |
| DoS 与成本滥用 | 请求体大小、HTTP Timeout、数据库 Timeout、连接池、任务预算和结果行上限 | MCP 初始化/目录/任务申请没有速率限制或租户配额；可消耗模型和 OA 资源 |
| 容器逃逸或文件系统写入 | Gateway/OA 非 root、只读根文件系统、`no-new-privileges`、仅 `/data` Volume 与 `/tmp` 可写 | 未配置 seccomp/AppArmor 定制、Capability 显式 drop、镜像签名或供应链证明 |

## 数据最小化与外发

DeepSeek 路径：

- 任务申请发送 Alice 的自然语言目标和脱敏逻辑目录。逻辑目录含产品、字段、描述、敏感级别、Scope 定义和允许聚合，不含 Source、物理 View、DSN 或密码。
- 自然语言查询发送问题和仅限当前 Grant 的逻辑产品/字段/聚合。
- 不发送 PostgreSQL 查询结果、SQLite 内容、OA Token、数据库密码或数据密钥。
- 直接 `query_sql` 不调用 DeepSeek。

查询结果会以明文返回给已授权 Alice 的 MCP 客户端；加密只保护 SQLite 静态存储，不是端到端显示控制。Codex、终端历史、截图、Skill 或用户后续复制均在 Gateway 边界之外。

## Demo 已实现但需运营配合的边界

- Catalog 管理员必须保证逻辑字段与 Reporting View 一致，且 View 不包含未发布敏感列。
- `.env` 必须保持 Git 忽略、文件权限收紧并安全备份数据密钥；修改密钥会使历史结果无法读取。
- Catalog 升级前应排空或重新申请 ACTIVE 任务，因为版本不一致会拒绝查询。
- 应监控 `MODEL_UNAVAILABLE`、回调重试失败、SQLite 恢复事件、数据库 Timeout、预算耗尽和审计链校验结果。
- Named Volume 备份必须与数据密钥一起管理；只有数据库文件没有密钥不能恢复结果，只有密钥没有完整 SQLite 也不能恢复审计链。

## 生产化差距

按优先级建议：

### 身份、网络与秘密

1. 用 OIDC/OAuth 2.0、短期 Token、Audience/Scope、组织/租户绑定和集中撤销替代静态 Token。
2. Gateway、OA、模型出口和数据库全部启用 TLS/mTLS；放入受控网络和反向代理，实施来源限制与 Egress Policy。
3. 使用 KMS/HSM 或 Secret Manager，给结果密文保存 Key ID，支持 Envelope Encryption、在线轮换和旧密钥受控解密。
4. 使用 Docker/Kubernetes Secret 或 Workload Identity，避免把秘密长期放在 `.env` 和进程环境。

### 持久化与高可用

1. 将 OA 草稿、用户、Outbox 和审批历史持久化；对 Gateway/OA 做双向对账和死信队列。
2. 将单实例 SQLite 控制面迁移到支持多实例事务、行锁和备份恢复演练的控制数据库；实现每任务分布式串行租约。
3. 明确定义任务、结果、凭证和审计的保留/删除/法务冻结策略；当前没有清理任务。
4. 为结果加密、预算账本和回调状态实施定期备份、恢复演练与一致性扫描。

### 审计与合规

1. 把 Audit Head 定期签名并锚定到独立 WORM/日志服务或可信时间戳，防止管理员重建链。
2. 启动和周期任务自动验证 Hash Chain；验证失败应停止敏感查询并告警。
3. 对管理员 Catalog 变更、密钥操作、Token 生命周期、登录、审批和结果读取建立独立审计。
4. 对敏感结果实施下载控制、水印、DLP、用途复核和最小聚合阈值；当前没有防推断/差分隐私控制。

### 策略与适配器

1. 对 Catalog 与实际 View 做发布时 Schema/类型校验，并把数据库迁移、角色 Grant 和 Catalog 作为一个受审发布单元。
2. 对 SQL Parser、渲染器和 Scope 注入做持续 Fuzz、属性测试、差异测试及 PostgreSQL 版本兼容测试。
3. 多 Source 支持需实现 `secretRef` 解析、Connector Registry、每方言 AST 策略和禁止跨源意外 Join；当前 `POSTGRES_DSN` 只有一个实例。
4. 增加管理员撤销/暂停任务、紧急 Kill Query、按主体/产品的全局预算和速率限制。
5. 明确 DeepSeek 数据处理协议、区域、保留策略、模型版本锁定和无模型降级工作流。

### 平台加固与可观测性

1. 增加结构化指标、分布式 Trace、告警、SLO、容量测试和真实外部模型的受控预发布验证；当前 Compose 自动化已覆盖完整 Alice/Bob 生命周期、重启、权限与降级，但使用本地确定性模型替身，不调用真实 DeepSeek。
2. 固定镜像 Digest，生成 SBOM、签名与漏洞扫描；实施依赖升级流程。
3. 显式 drop Linux Capabilities，配置 seccomp/AppArmor、资源配额、只读 Secret Mount 和 Network Policy。
4. 对 MCP 与 OA 增加速率限制、并发限制、Body/响应配额、异常检测和防暴力登录。

## 上线门槛

在接入真实企业数据前，至少应完成：企业身份与 TLS、集中密钥管理、持久 OA/Outbox、多实例控制库方案、外部审计锚、Catalog/View 联合发布、全量 E2E/攻击测试、备份恢复演练、数据保留策略以及安全评审。未完成这些项目时，本仓库应仅用于本机确定性 Demo 和接口验证。
