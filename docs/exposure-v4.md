# TaskGate V4：Snapshot-Indexed Hybrid Bitmap Ledger

V4 不改变 FactID 的业务含义，也不改变安全公式；它只替换百万级事实的在线表示和结算方式。人类审批的是 Catalog 预定义的完整预算，Agent 可以使用其中全部容量。系统不自动寻找“最小预算”，唯一的 admission 条件是三个 root-family ledger 在提交后仍不越界。

\[
\Delta_d=E_d\setminus K_d,\qquad K'_d=K_d\cup E_d,\qquad |K'_d|\le B_d^{Catalog}
\]

## 架构

```mermaid
flowchart LR
    C[冻结的业务快照<br/>Candidate Catalog] --> IC[Snapshot Index Compiler]
    IC --> S[Business PG immutable sidecar<br/>entity key → row_handle]
    IC --> D[FactID ↔ uint32 ordinal<br/>hot hashes / cold payload chunks]
    D --> M[publication manifest digest]
    S --> Q[visible SQL + ordinal companion<br/>one REPEATABLE READ snapshot]
    M --> A[authorize + bind publication]
    A --> H{committed semantic replay?}
    H -- miss --> Q
    H -- hit --> O[committed observation reference]
    Q --> E[streamed OrdinalEngine<br/>weighted bitmap effect]
    E --> X[R/I/O ANDNOT + popcount]
    O --> X
    X --> CAS[one root-head epoch CAS<br/>publish all 3 dimensions]
    Q --> P[Parquet + chunked AES-GCM<br/>private staging object]
    CAS --> T[PENDING artifact metadata + audit + V6 receipt<br/>same Control PG transaction]
    P --> T
    T --> K[commit, then idempotent<br/>canonical object creation]
    K --> R[consumed / AVAILABLE<br/>result_id + summary]
```

可以把 V3 想成：每次过收费站，都把一百万张货物清单打印成纸，逐张拿去档案室查重并归档；即使是同一车货重放，也重新打印一遍。V4 是给冻结仓库中的每件货物预先贴不可变编号。在线只携带精确的“编号打孔卡”（Roaring bitmap），收费站用集合差和计数判断有没有越过人类批准的仓库边界。少数查询临时生成的聚合结果和 outcome 没有预编号，放进一个小型动态字典。打孔卡是压缩表示，不是 Bloom filter，因此没有假阳性，也不会少收费。

## 可信发布物

一个 `snapshot_publication` 同时绑定：

- datasource、schema digest、冻结 snapshot ID；
- immutable ordinal sidecar 及其 digest；
- base-row 和每个 canonical base-cell field 的 dictionary segments；
- `ordinal → 32-byte FactHash` 热数组；
- sealed、content-addressed canonical payload 冷工件；Control PG 存储接口可进一步切分并用 ZSTD 压缩；
- compiler/profile/collation/type 版本和完整 manifest digest。

每个 segment 内的 FactID 以完整 SHA-256 排序并分配 `uint32` ordinal。达到 \(2^{32}\) 前按确定性 hash prefix 分片。构建器拒绝重复 entity key、非规范类型或 collation、hash/payload collision、计数差异和 artifact digest 不一致。Catalog Product 必须引用 publication；任一绑定缺失时 V4 查询关闭式失败。

热索引不保留 canonical payload。Gateway 日常只需要 row handle 到 row/cell ordinal 的映射、ordinal hash 数组、segment bounds 和 manifest。审计导出时才从冷块按需展开 payload，并重新校验 hash。

### 离线编译、发布和启动

`cmd/snapshot-index` 接受严格的 `taskgate-snapshot-index-input-v1` JSON。输入必须声明受限的 `source_relation: reporting.<name>`，以及 Catalog source、publication、sidecar、按顺序排列的 entity-key fields、字段 SQL type/collation 和 candidate rows；publication CLI 不允许退回独立 JSON rows。构建器要求 `SNAPSHOT_POSTGRES_DSN`，以非管理、只读角色开启一个 `REPEATABLE READ` 事务，要求 relation 是 populated materialized view、owner 为 NOLOGIN 且不在 reader 的角色继承链中，并逐一核对所有唯一物理字段的 canonical PostgreSQL type、collation、collation version 和 deterministic 属性。随后它扫描数据库字段并完全覆盖 JSON rows，expected digests 才是人类审核过的值级发布断言；JSON 不能伪造 snapshot cells。库内直接调用 `snapshotbundle.Compile` 的 JSON-only 路径只保留给确定性单元测试，不是发布接口。

无论 rows 来自哪条路径，构建器都不会信任调用者提供的 entity key：它使用与在线 V2 相同的 `base-entity` typed canonical encoding 重新计算，若可选断言不一致、规范化后键重复、字段集合不同或 expected digest 不同，构建立即失败。

示例输入见 [`config/snapshot-index.example.json`](../config/snapshot-index.example.json)。在主机有 Go 工具链时执行：

```bash
SNAPSHOT_POSTGRES_DSN='postgres://gateway_reader:...@127.0.0.1:25434/travel_demo?sslmode=disable' \
go run ./cmd/snapshot-index \
  -input config/snapshot-index.example.json \
  -output-dir artifacts/snapshot-index
```

也可以只使用 Docker：

```bash
docker build --build-arg TARGET=snapshot-index -t taskgate-snapshot-index .
docker run --rm --user "$(id -u):$(id -g)" \
  --network taskbound-agent-data-gateway_business-data \
  -e SNAPSHOT_POSTGRES_DSN='postgres://gateway_reader:...@business-postgres:5432/travel_demo?sslmode=disable' \
  -v "$PWD:/work" -w /work taskgate-snapshot-index \
  -input config/snapshot-index.example.json \
  -output-dir artifacts/snapshot-index
```

每个 publication 生成一个不可覆盖的目录，包含：

- `*.hot.tgord`：Gateway 激活的 hash/ordinal/row-handle 索引；
- `*.cold.tgord`：离线审计使用的完整、密封且内容寻址的 canonical FactID/payload；当前 standalone compiler 输出不压缩，Control PG artifact/chunk 接口可选择 `ZSTD`；
- `*.sidecar.ndjson`：header、按 handle 稠密排序的 typed entity keys、footer；
- `*.bundle.json`：dictionary manifest、manifest digest 和三个文件的 byte digest/size；该文件最后写入，是发布完成标记。

CLI 落盘前会调用 `ParseHotDictionary`，并用不物化第二份 FactID 对象的 `VerifyColdDictionary` 逐项重算 canonical payload、segment roots、dictionary/cold digest；随后流式重验 sidecar 的每个 handle、typed key 和 HOT 映射。它在 stdout 输出需要进入 Candidate Catalog 的 `manifest_digest`、`dictionary_digest`、`sidecar_digest`、`cold_payload_digest` 和 `hot_index_digest`；人类审批的是带这些计算结果的 Catalog，而不是事先编造 digest。若要重建已经发布的名字，必须由操作员显式移走旧目录；CLI 不覆盖它。

默认 Compose 会在 Gateway 启动前以一次性服务编译
`config/snapshots/expense-*-v1.json`。builder 只连接内部 `business-data` 网络，
等待 Business PostgreSQL healthy，并使用 `gateway_reader` DSN 扫描实际冻结
materialized view；它不能连接 control-plane 或 public-edge。发布物写入共享 named
volume，重复启动只接受字节完全相同的 publication。直接运行 Gateway 二进制时，
则把 Catalog 中每个 `snapshot_publication` 目录放入同一个 base directory，并将
`GATEWAY_SNAPSHOT_ARTIFACT_DIR` 指向它。

Catalog 只要声明了 `snapshot_publications`，`GATEWAY_SNAPSHOT_ARTIFACT_DIR` 就是必需启动配置，而不是可选的运行时降级。Catalog validation 要求一个 V4 deployment 的所有可达 approval route 都使用 V4，禁止与 V2/V3 或 resource-only route 混合。Gateway 验证完全部 publication 后、注册任何 metadata 前，在 Control PG 原子写入不可变的 V4 deployment marker；该事务与 legacy grant/query/ledger 写入使用同一 advisory-lock 域，任一旧 ledger 非空、legacy resource-only query 尚未完成或并发旧写入都会令激活失败。Catalog publication 必须全部且仅各出现一次，HOT 总量不得超过 160 MiB，目录/文件不得是 symlink，Catalog、datasource、schema、snapshot、dictionary、manifest 和 sidecar digest 必须全部一致。HOT 必须携带 entity keys；Gateway 还会流式读取整个 NDJSON，以 typed key 重算 entity key 并逐 handle 对照 HOT，而不是只相信文件描述符。验证后才注册 `(catalog digest, publication name)`，并把 dictionary/segment metadata 幂等登记到 Control PG。每次 V4 编译都绑定同一个 Catalog-wide dictionary universe，使一个 root-family 能先后查询不同的已审批 Product；具体 bitmap 仍只包含本次实际事实。该 universe 在首个授权查询时才登记，启动器不会提前创建查询状态。

COLD 的逐 Fact canonical payload、FactHash、segment roots、dictionary digest 和 cold root 在 builder 发布前完整重算；审计消费者也必须以 Catalog `manifest_digest` 调用 `ParseColdDictionary` 或 bounded reader verifier。为了不把已测约 16.7 秒的百万 Fact JSON/canonicalization 离线工作重新塞进 `activation ≤ 2 s`，普通 Gateway 在生产只读 artifact mount 上对 COLD 做固定 64 KiB 缓冲的全文件 envelope 验证：嵌入 manifest 必须等于 Catalog `manifest_digest`，并核对 domain seal、EOF、长度和完整文件 SHA-256。bundle descriptor 只是 transport check，本身不是语义信任根；启动安全性依赖“builder 已完成语义验证 + 原子发布 + Gateway `:ro` 挂载”的 immutable-publication 边界。若部署不能提供只读不可变目录，就必须在上线前运行同一个 full COLD reader verifier，不能把普通启动的 envelope 检查当成 builder attestation。该过程不把 COLD canonical payload 留在 Gateway 热堆。不含 `snapshot_publications` 的旧 Catalog 无需该环境变量；已声明 publication 却未配置 artifact directory 时，Gateway 直接拒绝启动。

## 在线派生

`QueryPairStream` 在同一个只读 `REPEATABLE READ` 事务中缓冲 visible result，同时逐行发送 provenance companion。companion 必须带 Catalog publication 对应的 `row_handle`，并按 canonical output group 和稳定 entity key 排序。当前 Connector/可见结果到 Parquet 的部分路径仍会在内存中持有完整结果，不应把 companion 的流式派生误读为全链路有界内存。生产级百万行结果还需要有界流式 Parquet writer/reader 与容量验收。

`OrdinalEngine` 不再执行 `scanRelationV2 → JoinOnV2 → GroupV2 → Observation.Normalize` 的全量关系物化。它同时只保留当前 group 的：

- 精确 support bitmap；
- multiplicity 大于 1 的稀疏 extras；
- 查询级 Influence bitmap。

Witness 的同一证明使用加法，`UNION DISTINCT` alternative proof 使用逐事实最大值。提交 witness 时，Engine 在各 hash-sorted segment 上做 k-way merge，流式生成与 V2 oracle 完全相同的 witness commitment 和 derived FactID。未知 handle、越界 ordinal、publication/manifest 不匹配、非规范 bitmap、multiplicity overflow 或 companion 截断都会在结果释放前失败。

三维 Effect 表示为：

- Release：base-cell bitmap + 小型 derived-release 动态事实；
- Influence：base-row/base-cell bitmap；
- Outcome：一个动态 outcome 事实。

## 原子结算和 replay

Control PG 将 bitmap 按 `(dictionary, segment, ordinal>>16)` 分成不可变、content-addressed containers。Novelty 是精确 `ANDNOT + popcount`，root 更新是 `OR`。一个 root head 同时指向 Release、Influence、Outcome 三个 set manifest 和单一 epoch；所有子 Agent 都解析到同一个 root head。

结算事务读取并校验 root epoch，计算三个维度，再以带 epoch 条件的一次乐观 CAS 发布三个新 head；它不依赖一把覆盖整个派生期的 root 行锁。CAS 冲突会回滚本次事务、重新读取 head 并重算三维 novelty，最多重试 16 次，绝不会分别提交 R/I/O。任何维度超预算、重试耗尽或持久化错误都回滚 bitmap、artifact intent、observation、audit、materialization 和 receipt，并且不会创建 canonical 结果对象。

可见结果在结算前编码为 Parquet，并由 Gateway 以分块 AES-256-GCM 客户端侧加密为 TaskGate 私有 S3/MinIO bucket 中的 staging 对象。Control PG 事务只提交 `PENDING` artifact 元数据（`result_id`、对象位置、schema、行列数、ACL、TTL、双哈希、ETag、key ID 和状态），不保存 Parquet bytes 或结果行。commit 后 Gateway 将 staging 幂等提升为确定性 canonical key；canonical 对象成功创建就是完整结果的消费/释放边界，随后状态记为 `AVAILABLE/consumed`。

普通 `query_sql`/`execute_plan`/`get_query_result` 响应只包含 `result_id`、schema、行列数、`expires_at` 和计费摘要，不包含全量 `rows` 或对象键。`preview_result` 每次最多返回 100 行，且默认受 64 MiB artifact 读取上限保护；`deliver_result` 返回绑定 result/task/expiry 的 Gateway 签名短 TTL capability URL。preview、URL 生成、实际下载、未下载或重复下载都不改写预算、exposure、`consumed_at` 或 Receipt。

promotion 中断时，启动与后台 sweep 只在 staging 与已提交哈希/ETag 证据一致时恢复 `PENDING`；不重执行 Business SQL、不退款、不写第二份 Receipt。staging 丢失或 canonical 证据冲突会让 readiness fail closed，直到对象证据被恢复或受审修复；当前没有自动放弃/退款。artifact 到达自身 `expires_at` 后 metadata/preview/delivery 关闭；TTL 或管理员 purge 先删 canonical bytes 再 tombstone 元数据，active legal hold 阻止 retention 清理但不延长 `expires_at`。bucket 必须私有且禁用 versioning；非 loopback 交付基址必须为 HTTPS，日志/APM 必须脱敏 URL 中的 `token`。

成功结算会保存 `(root_task_id, observation_digest)`。同一 root 再次引用完全相同、已提交的 observation 时，Control 可直接确认 zero novelty，无需读取其百万位 bitmap。

distinct-request semantic replay 的 key 至少绑定：task、grant/scope digest、typed normal form（含全部 literals/parameters）、Catalog/schema/dictionary-set digest，以及 profile/compiler/order/page/result-encoding 版本。命中仍然执行授权、普通查询/行资源结算、审计和新 V6 receipt；Business/provenance SQL 不执行。缓存结果必须为新 query AAD 重新加密。缓存缺失、过期、跨 grant、密钥擦除、密文清理或 digest 不一致都按 miss 处理，回到 novel path。对同一完整 binding，发现密文已清理、过期或 key 已擦除时，Control 会在 miss 事务中原子清理 unusable materialization 并留下 eviction audit，避免僵尸 cache key 阻断随后重新发布；binding 冲突仍关闭式失败。

## 切换和边界

V4 是清库切换，不双写、不静默删除历史账本。迁移和首次 V4 activation 都检查旧 ledger；非空则拒绝启动/激活，并要求显式离线重建。激活 marker 是单向的：后续 V4 Catalog 可以升级，但换回 legacy Catalog 会在启动时被拒绝；数据库触发器同时拒绝新的非 V4 grant、旧任务 query、`exposure_ledgers`、`query_exposure_reservations`、`exposure_facts` 和 `query_exposure_facts` 写入，因此替换 Catalog 或旧 Gateway binary 不能恢复逐 Fact 热路径。

为保留历史 receipt verifier 和 V2/V3 测试 oracle，本实现没有物理 `DROP` legacy 表；这些表只可保存切换前证据，V4 激活后被数据库门禁逻辑封存。也就是说，用户方案中“删除运行时逐事实逻辑”已经满足，但“物理删表”刻意没有执行；若合规要求真正移除表，必须另做显式、可审计的离线归档/重建，而不能放进自动迁移。旧 receipt verifier 保留，V6 使用独立 digest domain，绑定 dictionary set、三维 effect digest、actual/charged counts、root epoch 和 result digest。

SLO 仅适用于：已验证并 warm 的冻结 snapshot index、受支持的闭合 canonical QueryPlan（来自无损 SQL lowering 或高级计划入口）、单路高基数 derivation，且不含离线构建和排队。它不适用于任意 SQL、可变数据源或未发布快照。高基数使用单路 semaphore，小查询走独立池；工作集超过配置阈值时进入查询私有、先加密后落盘的 spool。默认 Compose 为该密文 spool 使用独立磁盘 volume，而不是 16 MiB `/tmp` tmpfs；Gateway 的 512 MiB cgroup 上限保持不变。

## 实验状态与硬门槛

现有 [scale 结果](../evaluation/exposure-scale/results.json) 是 legacy 全量 FactSet/逐事实 PostgreSQL 路径的瓶颈诊断：最大点 12 Release + 1,035,000 Influence，direct SQL 约 33.9 ms，而 novel/replay 为 169.3/154.1 s，Gateway 峰值 9.8 GiB。它证明普通 SQL 不是主瓶颈，也证明旧表示不能服务交互工作负载；它不是 V4 bitmap 的性能结果。

V4 重跑前固定以下在线验收门槛，不能提前写成已实现 SLO：

- warm verified index、高基数并发 1：novel P50 ≤ 3 s、P95 ≤ 4 s；
- distinct-request semantic replay P50 ≤ 100 ms、P95 ≤ 150 ms，且不执行 Business/provenance SQL；
- Gateway cgroup peak memory ≤ 512 MiB，包含被触达的 mmap pages；
- 小查询延迟和吞吐相对 legacy regression 均不得超过 10%。

离线门槛是 index build ≤ 10 分钟、builder RSS ≤ 4 GiB、总 artifact ≤ 2 GiB、Gateway HOT ≤ 160 MiB、activation ≤ 2 s。报告必须把离线成本与在线成本分开，并分别列出 direct SQL、ordinal stream、bitmap derivation、settlement、replay、RSS、network、WAL、build time，以及按 1/10/100 个 root 摊销后的存储。

复测矩阵包括 0/50/90/100% overlap、dense/clustered/random sparse、Scan/Join–Group/Union/Page、同根 CAS 并发与小查询回归。正确性门槛先于性能：最大点 decoded FactHash、canonical payload、witness multiplicity 以及 novel/replay observation 必须与旧 oracle 完全相同。

### Ordinal derivation 工程测量

仓库提供一个显式启用的确定性 derivation-kernel 测量。默认 fixture 是 345,000 行、12 个 group；每行恰好贡献 base-row、group cell 和 aggregate-input cell 三个 Influence，因此测试会在输出报告前强制验证 **1,035,000 Influence + 12 derived Release**：

```bash
docker run --rm \
  -e TASKGATE_RUN_ORDINAL_SCALE=1 \
  -v "$PWD:/src" -w /src golang:1.25-bookworm \
  go test -count=1 -timeout=30m \
    -run '^TestOrdinalDerivationMillionInfluenceEvaluation$' -v ./internal/gateway
```

输出中的 `TASKGATE_ORDINAL_DERIVATION_REPORT` JSON 分开记录 fixture 生成、离线 index build、HOT activation、handle 准备和在线 bitmap derivation 时间；并记录 derivation 前后 Go heap、Linux `/proc/self/status` 的 RSS/HWM、每 4,096 行采样得到的在线 RSS 峰值，以及可用时的 cgroup v2 `memory.peak`。同进程 build+derive 模式的 `status` 是 `engineering_measurement_only`；用 `TASKGATE_ORDINAL_SCALE_EXPORT_HOT` 与 `TASKGATE_ORDINAL_SCALE_IMPORT_HOT` 分离 builder/Gateway 后，状态是 `engineering_measurement_online_process`。两者都不执行 Business SQL、connector I/O、Control PG settlement 或 replay，不能据此宣称端到端 SLO 已满足。

只验证 runner 时可以额外传入 `TASKGATE_ORDINAL_SCALE_ROWS=120` 和 `TASKGATE_ORDINAL_SCALE_GROUPS=12`；这种缩小运行同样不能作为百万点性能结果。

完整生产 publication 编译器另有 4 GiB opt-in gate；它覆盖输入规范化、ordinal compile、HOT/COLD 序列化与重验、sidecar 序列化与重验：

```bash
docker run --rm --memory=4g --memory-swap=4g \
  -e TASKGATE_RUN_SNAPSHOT_SCALE=1 -e GOMAXPROCS=4 \
  -v "$PWD:/src" -w /src golang:1.25-bookworm \
  go test ./internal/snapshotbundle \
    -run '^TestSnapshotPublicationScaleEvaluation$' -count=1 -timeout=15m -v
```

2026-07-30 的受限工程复测见 [`evaluation/v4-kernel/results.json`](../evaluation/v4-kernel/results.json)：离线 production compile+verify 为 16.684 s，process HWM 3,616,878,592 bytes，完整 artifact 587,211,769 bytes，HOT 64,861,634 bytes。五个全新 512 MiB cgroup 进程加载另一份同规模 HOT 并派生精确的 1,035,000 Influence + 12 Release，bitmap kernel 的 type-7 P50/P95 为 1.272/1.497 s，HOT activation 为 0.441/0.587 s，最大 cgroup `memory.peak` 为 291,278,848 bytes。该记录通过离线门槛和 warm-HOT 内核门槛，但不包含公开 `query_sql` lowering（或高级 `execute_plan`）的 SQL、结算和 replay，完整 V4 acceptance 仍是 pending。

## 正确性接口

V4 的实现和形式化 refinement 检查以下边界：

1. 每个 immutable segment 是 canonical FactID 与 ordinal 的双射；
2. bitmap `OR`、`ANDNOT` 和 `popcount` 分别 refinement FactSet 的并、差和精确基数；
3. 对声明的闭合关系代数，decoded ordinal effect 等于旧 V2/V3 oracle effect；
4. 三维 root-head CAS 模拟原 TLA root-family settlement，因此 Catalog 边界定理与表示无关。

有限 refinement 模型位于 `formal/ExposureBitmapRefinement.tla`。V2/V3 通用代数继续作为测试 oracle，而不再处于 V4 生产热路径。
