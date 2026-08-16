# 9/9 所需的 Catalog 改动（一次性设计，2026-08-16）

作者 2026-08-16 定：正式 campaign 要跑，路线走**出路 1「按 profile 分部署」**。
本文把达成 9/9 所需的 Catalog／Product／路由改动一次性设计完，理由见第三节。

## 一、为什么必须一次改完

今天已经踩过一次：为让 Baseline S1/S2 有审批路由而单独改主 Catalog（`7ff48f2`），
结果 `config/profiles/registry.json` 重新生成、摘要变更，连带打断三份保留证据
（`activation-support-v1.json`、`outside-product-route-matrix-v1.json`、
`semantic-cache-isolation-evidence-v1.json`）与九个测试，最终整体还原（`06d78da`）。

**任何改变 Catalog 里 Product 或路由集合的改动，都会改变注册表摘要，从而作废这三份
实跑证据。** 它们只能重测、不能改摘要重贴标签。所以正确做法是：把 9/9 需要的全部
Catalog 变更**一次做完**，然后**只付一次**重测代价。

## 二、每个未完成家族的实查需求

| 家族 | 缺格 | 绑定的 Product | 需要的额度 | 现状 |
|---|---:|---|---|---|
| Baseline S1 | 10 | `provsql_orders` | 5,000 / 50,000 行 | profile Catalog 只投影出 `summary-manual-v5`（500 行），不够 |
| Baseline S2 | 10 | `provsql_orders` + `provsql_lineitem` | 3 行，双 Product 精确集合 | 同上，且双 Product 闭包无法配路由 |
| Baseline S3 | 15 | `final_v5_exposure_scale` | 1,000 / 10,000 / 45,000 行 | 该 Product 路由 `max_rows: 16`，不够 |
| Baseline S4 | 3 | `final_v5_analytics_depth4` | 3 行 | **该 Product 在活体 Catalog 里不存在** |
| Baseline S5 | 8 | `provsql_orders` | 5,000 / 50,000 行，双参数、`execute_plan` | 同 S1，另需新执行路径 |
| Baseline S6 | 0 ✅ | `final_v5_result_heavy` | 100,000 行 / 1,600,000 释放 Fact | 已实现，额度正好装下 |
| Scale dependency-e2e | 24 | `final_v5_exposure_scale` | 16 行 / 1,035,000 影响 Fact | 路由已就位，未实测 |
| Scale outcome-merkle | 36 | **无 Product** | — | 纯内核，不需部署 |
| Scale extreme | 2 | **无 Product** | — | 纯内核，不需部署 |

**关键收获**：scale 的 62 格里 **38 格根本不需要 Gateway**，只有 24 格要部署。

## 三、约束：为什么不能简单加路由

两条源码级硬约束（今天实查）：

- `internal/catalog/resolve.go:72-99`：产品级路由是**精确集合匹配**。请求集合与某条
  产品级路由有交集但不相等，即 fail-closed，不会退回敏感度默认路由。
- `internal/catalog/validate.go:246-268`：**一个 Product 最多属于一条产品级路由**。

推论：`[orders]`（S1/S5）、`[orders, lineitem]`（S2）、`[lineitem, nonce, orders]`
（ProvSQL）三个重叠闭包，**在同一份 Catalog 里无法各配一条路由**。而 profile Catalog
是主 Catalog 的**投影**，只能复制主 Catalog 已有的路由，无法凭空生成，所以"分部署"
本身并不能绕开这条——被投影下去的仍然是主 Catalog 里那条不匹配的路由。

**这个架构对"某族需要自己的额度"的标准答案是：给它自己的 Product。**
`final_v5_attack_expense_detail`、`final_v5_rls_unlimited_expense_detail`、
`final_v5_rls_bounded_expense_detail`、`final_v5_concurrency_expense_detail`、
`final_v5_result_heavy`、`final_v5_exposure_scale` 六个都是这么来的——都是同一批底层
数据上的**独立物化视图**，各自持有独占路由。Baseline S1/S2/S5 复用别族 Product
（契约写的 `EXISTING_PRODUCT_REUSED`）是全仓唯一的例外，也正是它卡住的原因。

新增 Product 的实际代价（照 `final_v5_concurrency_expense_detail` 的既有做法实查）：
`db/init/00-schema.sql` 里一条物化视图（约 3 行 SQL）、`db/init/10-reader.sh` 的读授权、
`db/init/07-freeze-publications.sql` 的冻结断言、Catalog 一个 Product 条目。
**复用既有 snapshot publication，因此不需要生成新的 sidecar／dictionary／manifest 摘要。**

## 四、Catalog 变更清单（一次性）

### 4.1 新增 Product（3 个物化视图）

| 新 Product | 视图来源 | 复用 publication | 服务 |
|---|---|---|---|
| `final_v5_baseline_orders` | `reporting.provsql_orders` | `provsql-orders-v1` | S1、S5 |
| `final_v5_baseline_join_orders` | `reporting.provsql_orders` | `provsql-orders-v1` | S2 |
| `final_v5_baseline_join_lineitem` | `reporting.provsql_lineitem` | `provsql-lineitem-v1` | S2 |

S1/S5 与 S2 必须用**不同的** orders Product：否则 `[orders]` 与 `[orders, lineitem]`
两个闭包会让同一个 Product 落入两条产品级路由，违反排他约束。

S4 的 `final_v5_analytics_depth4` 是第 4 个新 Product，但它是**深度 4 的语义视图**，
不是简单投影，成本高于上述三个，排在最后做。

### 4.2 新增路由与预算档

| 路由（精确集合） | 预算档 | 关键额度 | 依据 |
|---|---|---|---|
| `[final_v5_baseline_orders]` | `final-v5-baseline-orders-v1` | `max_rows 400000` | S1/SF10 每 Task 四次受治理查询 × 50,000 行（行账本累计，见 `internal/control/budget.go:421-442`） |
| `[final_v5_baseline_join_lineitem, final_v5_baseline_join_orders]` | `final-v5-baseline-join-v1` | `max_influence_facts 8000000` | S2/SF10 连接 50,000 orders 与 250,000 lineitem |

### 4.3 需要放宽的既有路由

`final-v5-exposure-scale-v1` 现为 `max_rows: 16`，而 Baseline S3 在同一个 Product 上
要 45,000 行。二者共用一个 Product，而一个 Product 只能有一条路由，**故该路由的额度
必须取两族需求的上界**。`capability_test.go` 里逐字节断言该预算档的守卫需相应重述，
并写清放宽的是哪一项、为谁放宽。

## 五、执行顺序（先攒后测，只付一次重测代价）

1. **四节全部 Catalog／Product／路由改动一次落地**（含 S4 的 depth-4 视图）
2. **契约修订**：`baseline-v1.json` 的 S1/S2/S5 Product 绑定由 `EXISTING_PRODUCT_REUSED`
   改为新 Product；发新契约版本，重算契约索引摘要
3. **重新生成 profile**：闭包变化 → 新 profile ID／别名 → 新注册表
4. **一次性重测证据链**（实跑）：逐 profile 激活 → 路由矩阵 → 激活支持清单 → 缓存隔离
5. **重新资格认证 artifact 定向绑定**（对新注册表）
6. **建分部署 campaign runner**
7. **实现剩余格**：S3 15、S5 8、S4 3、scale dependency-e2e 24（另 38 格纯内核）
8. **在 profile 路径上重跑 baseline 定向轮**，恢复从 HEAD 的可复现性

## 六、明确未决、需作者裁决的一项

第 2 步是**契约修订**：把 Baseline S1/S2/S5 的 Product 绑定从"复用 ProvSQL 关系"
改为"Baseline 专属 Product"。契约的 `research_design_status` 是
`AUTHOR_APPROVED_FOR_IMPLEMENTATION`，本改动**不改变任何指标口径、也不改变论文主张**
（查询字节、期望行列、模式集合全不变，只换 Product 名与其独占额度），但它确实动了
一份已批准的研究设计绑定。已按双通道上报作者，等待裁决后再执行第 2 步；
第 1、7 步中与契约无关的部分可先行。
