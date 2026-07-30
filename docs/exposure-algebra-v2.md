# Exposure Algebra V2：正式语义

本文给出 `taskgate-exposure-v2` 的规范性定义。关键词“必须”“不得”和“关闭式失败”具有规范含义；代码注释或论文中的非正式说明与本文冲突时，以本文和对应版本的可执行测试为准。

## 1. 语义环境与适用域

一个语义环境记为

```text
Π = (pgMajor, typeDomain, timezone, collations, snapshots, catalogHash)
```

只有满足以下条件的环境才是 admissible：

1. `pgMajor`、Catalog、reporting view schema/definition 和 datasource identity 已由 Connector 证明；每个字符串字段还固定确定性 PostgreSQL collation 的精确名称和实际版本。
2. `timezone = UTC`。查询不含 volatile/stable-but-time-dependent 表达式、窗口、子查询、递归、`UNION ALL` 或未列入本规范的运算。
3. 每个 Scan namespace 绑定一个不可变的 Catalog snapshot；同一 namespace 在一棵表达式中不得绑定两个 snapshot。
4. 每个基础关系的 entity key 唯一；每个 self-join 输入有不同的稳定 Catalog role。字段 ID 在 Join 后全局唯一，不使用 SQL alias 充当语义 ID。
5. 类型属于第 6 节的 V2 值域。`time with time zone` 和文本保真但无 equality 的 `json` 均不可接受；结构化等价使用 `jsonb`。
6. Group 的输出值来自同一 PostgreSQL snapshot 中实际执行的 `COUNT|SUM|MIN|MAX`；输出 oracle 不得遗漏、重复或虚构正向 group。
7. Page 的输入已经按“用户 order key + Catalog stable entity/group key”构成的全序排列。没有可证明全序时关闭式失败。
8. PostgreSQL cast、算术和 aggregate 必须正常完成；SQL overflow、typed range、witness `uint64` multiplicity 或 canonical encoding 失败都使求值无定义，不能生成、结算或释放 Effect。

这些条件是定理的前提，不是隐藏假设。在线编译器在 Catalog 验证、schema/collation 证明、结构化计划检查、同一 `REPEATABLE READ` snapshot 和 provenance 完整性检查中执行它们。

本规范的第二账本是 **conservative positive-output dependency footprint**：
对每个实际交付输出行和单元格，按以下归纳规则计入参与成功推导的基础事实。
`Influence` 仅是兼容字段名。该足迹既不是最小 causal provenance，也不是
PostgreSQL 的完整 physical read set；“精确”仅指相对于本规范规则精确且不漏计。

## 2. 正式语法与类型规则

令 `N` 为 source namespace，`s` 为 snapshot，`r` 为稳定 relation role，`f` 为稳定字段 ID，`τ` 为规范 SQL 类型，`κ` 为精确 collation `(name,version,deterministic=true)`。

```text
Γ ::= ∅ | Γ, f : τ [collate κ]

e ::= Scan[N,s,r,Γ]
    | Select[p; deps](e)
    | Project[F](e)
    | Join[Θ](e,e)
    | UnionDistinct(e,e)
    | Group[G; A](e)
    | Page[O; stable; offset; limit](e)

p ::= atom | p AND atom
atom ::= f op literal
op ::= = | <> | < | <= | > | >= | LIKE | IN | NOT IN

Θ ::= f = f | Θ AND f = f
A ::= COUNT(*) | COUNT(f) | SUM(f) | MIN(f) | MAX(f)
O ::= f ASC | f DESC | O, f ASC | O, f DESC
```

`deps` 是谓词实际读取的字段集合，不包括常量。在线 QueryPlan 的多个 filter 是一个纯、无副作用的合取。

类型判断写作 `Π; Γ ⊢ e : Γ'`：

```text
(T-Scan)     all fields/types/collations are canonical
             -----------------------------------------
             Π; ∅ ⊢ Scan[N,s,r,Γ] : Γ

(T-Select)   Π; Γ ⊢ e : Γ       deps ⊆ dom(Γ)       p : SQLTruth
             ---------------------------------------------------
             Π; Γ ⊢ Select[p;deps](e) : Γ

(T-Project)  Π; Γ ⊢ e : Γ       ∅ ≠ F ⊆ dom(Γ)
             -----------------------------------
             Π; Γ ⊢ Project[F](e) : Γ|F

(T-Join)     Π; ΓL ⊢ l : ΓL     Π; ΓR ⊢ r : ΓR
             dom(ΓL) ∩ dom(ΓR)=∅
             Θ 非空且每个等式两端具有相同规范类型与 collation
             ---------------------------------------------------
             Π ⊢ Join[Θ](l,r) : ΓL ∪ ΓR

(T-Union)    Π ⊢ l : Γ          Π ⊢ r : Γ
             --------------------------------
             Π ⊢ UnionDistinct(l,r) : Γ

(T-Group)    Π; Γ ⊢ e : Γ       G ⊆ dom(Γ)
             A 的参数存在且 PostgreSQL 输出类型正确
             ---------------------------------------------------
             Π ⊢ Group[G;A](e) : Γ|G ∪ type(A)

(T-Page)     Π; Γ ⊢ e : Γ       offset,limit ≥ 0
             O ++ stable 是 Γ 上经证明的全序
             ---------------------------------------------
             Π ⊢ Page[O;stable;offset;limit](e) : Γ
```

规范化器实现同一套静态检查。缺字段、重复字段、错误 aggregate output type、空 equijoin、非确定性/未固定 collation、不同 Union schema、`UNION ALL` 或未知 operator 均被拒绝。

## 3. FactID 与规范编码

V2 Fact 的数学 payload 为：

```text
BR(N,s,k)
BC(N,s,k,f,τ,Cτ(v))
DV(B,kout,x,τ,Cτ(v),CW(W))
```

其中：

- `BR` 是基础行存在事实；
- `BC` 是基础单元格事实；
- `DV` 是派生输出单元格事实；
- `B` 是按 namespace 排序且 namespace 唯一的 `(namespace,snapshot)` bundle；
- `kout` 是输出行键；
- `x` 是删除 alias 后的规范表达式；
- `Cτ` 是第 6 节的类型化值规范函数；
- `W : BaseFact ⇀ ℕ+` 是 finite-support witness multiset；空 support 合法。

每个字符串编码为 `uint64_be(byte_length) || UTF8_bytes`，列表先编码 `uint64_be(count)`，再编码元素。V2 payload 先编码 `kind` 和字面 profile `taskgate-exposure-v2`，然后按上面的字段顺序编码。Snapshot bundle 在编码前按 `(namespace,snapshot)` 排序。

Witness commitment 的精确定义为：

```text
pairs(W) = concat over ascending FactHash h of [h, zeroPad20(W[h])]
CW(W)    = ComposeCanonicalKeyV2("witness-multiset", pairs(W))
FactHash = SHA256("TASKGATE-FACT-V2\0" || CanonicalPayload)
```

空 witness 使用单个 token `empty`。Legacy V2/V3 的 `exposure_facts` 同时保存 hash 和完整 canonical payload；V4 将同样的 collision check 移到 immutable/dynamic dictionary，并用 ordinal bitmap 表示集合。两种表示都在同一 hash 遇到不同 payload 时关闭式失败，因此安全性不以“不会发生 SHA-256 collision”为数据库一致性假设。

### 3.1 产品、快照与逐算子输出实体键

派生 Fact 没有调用者可填写的 `product` 字符串。其产品身份严格定义为 snapshot bundle `B` 的 namespace 投影，快照身份是完整、按 namespace 排序的 `(namespace,snapshot)` bundle。令：

```text
RID(R,t) = H("relation-row", normalizedSchema(R), snapshotBundle(R), t.key)
```

各运算的 `DV.outputRowKey` 完整定义如下：

| 输出 | Snapshot/product bundle | `outputRowKey` |
|---|---|---|
| Scan | `[(N,s)]` | Catalog stable entity key `e`；在线 lowering 将键字段编码为 `H("base-entity",N,(field,type,Cτ(value))*)` |
| Select/Project/Page | child bundle | 保留 child key |
| Join pair `(l,r)` | 两侧 bundle 的 namespace-wise merge | `H("join-row", sort(RID(L,l),RID(R,r)))` |
| Union-Distinct class | 两侧 bundle 的 namespace-wise merge | `H("union-distinct-row", normalizedSchema, sorted(field,type,Cτ(value)) tuple)` |
| Keyed Group | child bundle | `H("group-row", sort(expression,type,Cτ(groupValue))*)` |
| Global aggregate | child bundle | `H("group-row","global")` |

因此 global aggregate 的常量 row key 并不丢失产品/快照：`DV` 仍绑定 bundle、aggregate expression、typed output value 和 witness。Group 同时绑定 group tuple 和 bundle/witness；Join pair key 递归绑定两个 immediate child 的 schema、bundle 与 entity key；Union 的 row key 绑定 typed dedup tuple，而最终 `DV` 还绑定 merged bundle、canonical union expression 和 alternative witness。若同一 namespace 在一个表达式内对应两个 snapshot，bundle merge 失败。

## 4. 标注关系

一个单元格标注为：

```text
c = ⟨v, τ, S, W, x, optionalReleaseFact⟩
```

一行标注为：

```text
t = ⟨k, cells, Sr, Wr, origins⟩
```

关系 schema 还为每个字段携带与数据行无关的规范 lineage expression `exprR(f)`。Scan 令其为 `namespace.fieldID`；Project、Select、Join、Page 传播它；Group 构造 `group(exprR(f))`/`fn(exprR(f))`；Union 对相同表达式保持原值，否则取排序后的 `union(exprL(f),exprR(f))`。因此空关系以及交换 Union operand 都不会通过“第一行”影响后续 derived FactID。

关系的值载体是由唯一 row key 实现的有限 PostgreSQL bag：两个 value tuple 即使逐字段相等，只要 row key 不同就仍是两个 occurrence。Select、Project、Join 和 Page 保持 bag multiplicity；只有 Union-Distinct 和 Group 按 typed equivalence class 产生一个输出 occurrence。Effect 最终是 Fact 集合，但中间 witness multiset 保留 join fanout 与 aggregate multiplicity。

`S`、`Sr` 是 V2 base Fact 的有限集合；`W`、`Wr` 是正整数 multiset，并始终满足：

```text
support(W) = S
support(Wr) = Sr
```

集合合并写作 `⊔ = ∪`；同一证明路径的 multiset 合并写作 `⊕`（逐键加法）；Union-Distinct 的替代证明合并写作 `⊔max`（逐键最大值）。`⊔max` 保留某一分支内部已有的 Join fanout multiplicity，同时满足 `W ⊔max W = W`。

`ValidateRelationV2` 在每个运算边界检查：规范 schema/snapshot bundle、唯一行键、完整 cell shape、类型和值可规范化、Fact hash/payload 一致、support/witness 等支撑、以及全部基础事实均被当前 snapshot bundle 覆盖。预置 base-cell release fact 还必须属于该 cell support；预置 derived release fact 的 expression 和 witness commitment 必须与该 cell 完全一致。因而“同值、同类型但不同血缘”的 Fact 不能被伪装成合法 release identity。

## 5. 求值推导规则

求值判断写作 `Π;D ⊢ e ⇓ R`。以下规则省略不变字段的逐项复制。

### 5.1 Scan

对基础 tuple `(entity=k, values=v)`：

```text
br = BR(N,s,k)
cell_f = ⟨v[f], τf, {BC(N,s,k,f,τf,Cτf(v[f]))},
          [BC(...) ↦ 1], N.f, BC(...)⟩
row = ⟨k, cells, {br}, [br ↦ 1], {(r,N,k)}⟩
```

所有基础 entity key 必须唯一。

### 5.2 Selection

PostgreSQL predicate 的结果域为 `{TRUE,FALSE,UNKNOWN}`，仅 `TRUE` 保留行。对保留行：

```text
Sr' = Sr ⊔ (⊔ f∈deps . Sf)
Wr' = Wr ⊕ (⊕ f∈deps . Wf)
cells' = cells
```

因此 predicate 字段进入保留行的 positive-output dependency，即使之后未投影；
FALSE/UNKNOWN 行不产生依赖事实。这是明确的 negative-information boundary。

### 5.3 Projection

```text
Project[F](⟨k,cells,Sr,Wr,O⟩)
  = ⟨k,cells|F,Sr,Wr,O⟩
```

字段的展示顺序不属于 Effect 身份。

### 5.4 Conjunctive equijoin

对左右行 `l,r`，若 `Θ` 中每个 typed equality 都是 SQL `TRUE`，产生一行；任一比较为 `FALSE` 或 `UNKNOWN` 就不产生行。每个输入行的闭合身份为：

```text
RID(R,t) = H("relation-row", normalizedSchema(R), snapshotBundle(R), t.key)
kjoin    = H("join-row", sort([RID(L,l),RID(R,r)]))
```

输出标注为：

```text
Sr = l.Sr ⊔ r.Sr ⊔ supports(all Θ key cells)
Wr = l.Wr ⊕ r.Wr ⊕ witnesses(all Θ key cells)
cells = l.cells ∪ r.cells
```

直接输入身份而不是仅用 Scan origin 构造 `kjoin`，所以 Group、Union、Page 或嵌套 Join 的结果仍可再次 Join，代数对 Join 闭合。

### 5.5 Union-Distinct

两行在每个同名 typed field 上满足 PostgreSQL `IS NOT DISTINCT FROM` 时属于同一 equivalence class；两个 NULL 相同，单边 NULL 不同。输出 row key 是规范 schema 和规范 typed tuple 的 hash。

对完整 typed tuple 已无重复的 set-valued `R`，Normalizer 与可执行代数可按
幂等律短路完全相同的已注解分支 `UnionDistinct(R,R)=R`；这保证合法的
duplicate-branch collapse 在下游 Group/Join 中也不改变 Effect。普通 Scan
仍按 PostgreSQL bag 处理：若其完整 tuple 有重复，`R UNION DISTINCT R` 等于
`Distinct(R)` 而不是 `R`，不能折叠。以下 class 规则应用于未被折叠的
operands。

对一个 class `Q`：

```text
member-row(t) = (t.Sr,t.Wr) ⊕ (⊕ f∈Γ . (t.cell[f].S,t.cell[f].W))
(Sr(Q),Wr(Q)) = ⊔max t∈Q . member-row(t)
Sf(Q) = ⊔ t∈Q . t.cell[f].S
Wf(Q) = ⊔max t∈Q . t.cell[f].W
```

若该 class 中所有 `cell[f]` materialize 为同一个 FactID，则保留该 FactID；否则清除预置 release fact，并令表达式为 schema 中排序、幂等的 `union(exprL(f),exprR(f))`，由新 row key、值和 `Wf(Q)` 构造 `DV`。这条规则同时保证所有不同候选的 dependency 进入足迹，并保证 `UnionDistinct(A,A)` 不制造新的 release identity 或 multiplicity。

`member-row` 将一个候选成员的 row dependency 与全部 schema equivalence
字段作为同一证明组合；不同 alternative members 再用幂等 `⊔max`。因此即使
最终可见字段 `V` 隐藏某个去重字段，该字段仍属于 distinct class 的依赖足迹。

### 5.6 Group 与 aggregate

Group key 使用逐字段 `IS NOT DISTINCT FROM`；NULL 因而形成一个普通 group。对成员集 `M`：

```text
kgroup = H("group-row", sort([expr(f), τf, Cτf(valuef)] for f∈G))  when G≠∅
kgroup = H("group-row", "global")                                 when G=∅
(Sr,Wr) = ⊕ m∈M . ((m.Sr,m.Wr) ⊕ (⊕ g∈G . (m.cell[g].S,m.cell[g].W)))

group-cell f:
  Sf = ⊔ m∈M . m.cell[f].S
  Wf = ⊕ m∈M . m.cell[f].W

COUNT(*) cell:
  S = ⊔ m∈M . m.Sr
  W = ⊕ m∈M . m.Wr

COUNT(f), SUM(f), MIN(f), MAX(f) cell:
  S = ⊔ m∈M . m.cell[f].S
  W = ⊕ m∈M . m.cell[f].W
```

表达式分别为 `count(*)` 或 `fn(input-expression)`。无 `GROUP BY` 的 PostgreSQL aggregate 必须恰有一行，包括空输入；有 key 的 Group 必须与输入的全部正向 group 一一对应。Aggregate 值本身由满足第 1 节条件的 PostgreSQL oracle 计算，而不是在 Go 中近似重算。

所以每个实际输出 group 的全部 group-key source facts 都在行级 dependency 中，
即使 key 不属于最终可见字段 `V`。`COUNT(f)` 的 NULL 参数 fact，以及
`MIN/MAX` 的非极值参数 fact，都按完整 operator-input 集合计入；PostgreSQL
计算标量时仍按自身规则忽略 NULL。这是保守依赖声明，不是反事实因果声明。

### 5.7 Stable Page

语义上先用固定 PostgreSQL comparator 排序。Comparator 对每个字段使用 Catalog 证明的 exact collation name/version；未显式指定时，`ASC` 使用 `NULLS LAST`、`DESC` 使用 `NULLS FIRST`。编译器在用户 order 后追加 Catalog entity key 或全部 group key（ASC），直到证明 lexicographic total order；global aggregate 至多一行。在线 SQL 执行相同 order 后才将关系标记为 `CanonicalOrder=true`。

若 `R.CanonicalOrder=true`：

```text
Page(R,o,l) = R.rows[min(o,n) : min(n, o+l)]   when l>0
Page(R,o,0) = R.rows[min(o,n) : n]
```

这里 `limit=0` 是结构化 QueryPlan 的“未设置上限”哨兵，不是 SQL `LIMIT 0`。
标注逐项不变。负边界或未证明全序时失败。当前正向 profile 不把未输出行、
排序字段、rank 或 absence 加入 dependency；这不是完整 top-k/read dependency，
而是明示的 negative/order-information boundary，不是 noninterference 保证。

### 5.8 Observation

对可见字段集 `V`：

```text
Release(R,V)   = { materialize(t, t.cell[f]) | t∈R, f∈V }
Dependency(R,V) = (⊔ t∈R . t.Sr) ⊔ (⊔ t∈R,f∈V . t.cell[f].S)
Effect(R,V)     = normalize(Release, Dependency)
```

`materialize` 优先复用合法的 base/derived `optionalReleaseFact`；否则产生 `DV(snapshotBundle,rowKey,expression,type,value,CW(W))`。两类集合分别规范化、排序和持久化，不相互折叠。

## 6. PostgreSQL 值语义条件

`CanonicalSQLTypeV2` 消除 `int2/int4/int8`、`decimal`、`float4/float8`、`bool`、`char/varchar`、`timestamp/timestamptz` 等别名。规范类型和值如下：

| 类型 | `Cτ(v)` |
|---|---|
| `smallint/integer/bigint` | 先检查 PostgreSQL 16/32/64-bit 范围，再编码为规范十进制整数 |
| `numeric` | exact rational `p` 或 `p/q`；支持 PostgreSQL NaN/±Infinity；拒绝 binary float 输入 |
| `real/double precision` | 对应 IEEE 位宽的规范 hexadecimal float；NaN、±Infinity 和 `-0=0` 规范化 |
| `boolean` | `true/false` |
| `bytea` | lowercase hexadecimal bytes |
| `date` | `YYYY-MM-DD` 或 ±Infinity |
| `time without time zone` | 从午夜起的微秒数；`time with time zone` 被拒绝 |
| `timestamp without time zone` | 无时区的 PostgreSQL 微秒精度 civil time |
| `timestamp with time zone` | UTC PostgreSQL 微秒精度 instant 或 ±Infinity |
| `uuid` | lowercase `8-4-4-4-12` |
| `text/character varying` | 原 UTF-8 字符串；相等与排序使用被证明的确定性 collation |
| `character` | 去除 PostgreSQL `bpchar` equality 忽略的尾部 ASCII space 后的字符串 |
| `jsonb` | 类型化结构编码：对象键排序、数组保序、number exact；会保留空白、键顺序、重复键和数值词法形式的 `json` 不在 V2 值域 |
| SQL NULL | 独立 token `null`，不与字符串 `"null"` 混同 |

`SQLValueEqualV2(τ,a,b)` 实现 SQL `=`：任一侧 NULL 得 `UNKNOWN`；否则比较 `Cτ`。`SQLValueNotDistinctV2` 实现 Group/Distinct 等价：两个 NULL 相等。字符串 equality 的这一步要求 collation 为 deterministic；Page 和有序 predicate 仍由固定名称及版本的 PostgreSQL collation 执行。Connector 每次证明都核对该名称、实际版本及 `collisdeterministic=true`。

合取遵循 PostgreSQL 三值逻辑：`FALSE AND x=FALSE`、`TRUE AND x=x`、`UNKNOWN AND TRUE/UNKNOWN=UNKNOWN`；Selection 和 Join 只保留 `TRUE`。`IN/NOT IN`、比较与 `LIKE` 的 NULL/collation 行为由固定 PostgreSQL profile 执行。

整数输入先检查 16/32/64-bit PostgreSQL 范围。Aggregate 输出类型固定为：`COUNT→bigint`、`SUM(smallint/integer)→bigint`、`SUM(bigint)→numeric`、`SUM(numeric/real/double precision)→` 同族、`MIN/MAX→` 输入类型。Aggregate value 来自 PostgreSQL，不在 Go 中重算；任何数据库 overflow/error 都没有求值结论。无 key 的空输入严格产生一行：`COUNT(*)=0`、`COUNT(f)=0`、`SUM/MIN/MAX=NULL`；有 key 的空输入产生零行。所有 aggregate argument cell（包括 NULL 与 MIN/MAX 非极值 fact）进入 profile-defined positive-output dependency，而 scalar 仍按 PostgreSQL 规则忽略 NULL 并只返回极值。

## 7. Query Normal Form

`taskgate-query-normal-form-v3` 是 typed、profile-bound 的 syntactic normal form，不是 SQL equivalence oracle。它绑定 namespace、snapshot、lineage digest、SQL type、精确 collation name/version、UTC、exact numeric、bag/NULL profile，并执行：

- 删除输出 alias，规范 aggregate 名称和 PostgreSQL 输出类型；
- 字段使用 namespace/role-qualified ID；
- 对 conjunction、projection、group key、aggregate 集排序；
- 对 Join predicate 内的两个字段及 Join operands 排序；
- 对 Union operands 排序，并将相同分支的 Union-Distinct 约为该分支；
- 保留用户 `ORDER BY` 的顺序，并按 Catalog 顺序追加 stable entity key；Group key suffix 使用字段 ID 的规范顺序；
- 拒绝不能静态证明 well-typed 的节点。

规范化不交换非合取 predicate，不重写 outer join，不做算术恒等变换，也不推断任意 SQL 等价。

## 8. 定理

下面所有定理都量化于同一个 admissible `Π`、同一个 snapshot bundle `D` 和通过第 2 节类型判断的有限表达式。

### 定理 1（表示不变量与闭包）

求值是偏函数：若 cast/overflow、collation、schema、bundle、oracle、key collision 或 representation premise 失败，则没有 `R`、Effect、settlement 或 release。若 `Π;D ⊢ e ⇓ R`，则该 `R` 唯一且 `ValidateRelationV2(R)=OK`；而且 `R` 可作为任意满足类型规则的父 operator 输入，包括 Group/Union 后再次 Join。

证明：对 `e` 的结构归纳。Scan 直接构造唯一 row key、singleton support/witness 和规范 cell。Select、Project、Page 只复制或以 `∪/⊕` 合并已验证标注。Join 的 schema disjointness 和 `RID` 构造给出闭合行身份；Union 的 typed tuple class 给出唯一输出键，`∪/max` 保持等支撑；Group 的 group equivalence class 给出唯一键，`∪/⊕` 保持等支撑。每个实现规则返回前再次执行完整 validator。证毕。

### 定理 2（Positive-output dependency 确定性）

固定 `Π,D,e,V` 后，`Dependency(Eval(e,D),V)` 唯一，因此完整 `Effect` 唯一。

证明：基础值和 PostgreSQL oracle 在固定 snapshot 下唯一。`Cτ`、typed truth、row-key hash、set union、multiset addition/max、canonical sorting 和 Fact materialization 都是确定函数。对语法树结构归纳即可；Page 的全序前提排除了 tie 的非确定选择。证毕。

### 性质 1（Group-key completeness）

对每个交付 group `Q` 和每个 `g∈G`，所有成员的 `cell[g].S` 都属于该输出行
的 dependency，因此在 `g∉V` 时也不会被 projection 丢失。该性质直接来自
第 5.6 节的行级 same-proof composition。

### 性质 2（Distinct-dependency completeness）

对每个交付的 union-distinct class `Q`，每个候选成员和每个 `f∈Γ` 的
`cell[f].S` 都属于该输出行 dependency，即使 `f∉V`。不同 members 的
alternative max composition 不改变最终 support union。

### 性质 3（Conservative aggregate-input）

每个交付 aggregate cell 的完整逻辑参数输入都属于 dependency：`COUNT(*)`
包含全部成员 row dependency，其他已支持 aggregate 包含全部参数 cell facts。
NULL 和 MIN/MAX 非极值均不得遗漏；该性质不声称这些输入是最小 causal
provenance。

### 定理 3（正规形可靠性，而非完备性）

对本规范列出的 rewrite closure：

```text
NFΠ(e1) = NFΠ(e2)  ⇒  EffectΠ(e1,D) = EffectΠ(e2,D).
```

证明：Normalizer 可能消除的差异只有 alias、集合型字段/合取顺序、Join operand/predicate 顺序、Union operand 顺序、已证明完整 tuple 无重复的同一 Union 分支，以及固定 stable suffix 的拼写。Scan 不携带该证明；Union-distinct 与 Group 的结果携带，Select/Page 可保留，Project 仅在 schema 不变时保留。Alias 不进入 `DV.expression`；集合规范化消除遍历顺序；Join 使用排序后的 immediate row identities 且 `∪/⊕` 可交换；Union class、relation-level expression、`∪/⊔max` 均按规范顺序构造，且 `W⊔maxW=W` 并保留相同 materialized Fact，故合法的 Union commutativity/idempotence 在包括下游 Group 的上下文中成立；Group key 和 witness 构造先排序；Page 的实际 SQL 和 NF 使用同一 canonical suffix。各原子 rewrite 在任意 well-typed context 下保持 Effect，由 context 结构归纳得到 closure。证毕。

逆命题不成立：两个 NF 不同的计划仍可能在某个特定数据集上产生同一 Effect。该定理也不覆盖任意 SQL optimizer rewrite。

### 定理 4（分割、重放与无重复收费）

若字段分割 `V=V1∪V2`，或一个 canonical-order 关系被不重叠 Page 完整分割为 `R1..Rn`，则：

```text
Effect(R,V) = merge(Effect(R,V1),Effect(R,V2))
Effect(R,V) = merge_i Effect(Ri,V)
```

任一 Observation 重复任意次数，其归一化 Effect 不变。

证明：Observation 只做有限集合 union。Projection/Page 不改所选 cell 的 Fact identity 或 annotation；完整分割恰好覆盖原索引集。集合 union 满足结合、交换和幂等律。证毕。

### 定理 5（精确增量计费）

令根任务历史为 `K=(Kr,Ki)`，候选联合 Effect 为 `E=(Er,Ei)`。成功 settlement 的唯一新增费用是：

```text
Δr = Er \ Kr
Δi = Ei \ Ki
charge = (|Δr|, |Δi|)
K' = (Kr ∪ Er, Ki ∪ Ei)
```

所以同一根任务族通过 retry、委托或等 Effect rewrite 重复观察时增量为零；若任一 `|K|+|Δ|` 超限，任何结果都不能释放。

证明与物理表示无关。Legacy Control PG 以 `(root,kind,fact_hash)` 唯一键实现集合差；V4 对 immutable FactID↔ordinal 双射执行 exact bitmap `ANDNOT/OR/popcount`，其 decode 分别等价于差/并/基数。V4 一次 root-head epoch CAS 原子发布所有维度，冲突后按新 head 重算；任一边界失败整笔回滚，release 只发生在 commit 后。因此两条实现都模拟上述集合转移。`ExposureLedger.tla` 给出抽象有限状态机，`ExposureBitmapRefinement.tla` 给出 bounded ordinal/CAS refinement obligation。证毕。

## 9. 实现对应与声明边界

| 规范对象 | 实现 |
|---|---|
| Typed value、FactID、canonical payload | `internal/exposure/fact.go` |
| 推导规则、validator、Observe | `internal/exposure/algebra_v2.go` |
| Snapshot dictionary、exact bitmap、weighted witness | `internal/ordinal` |
| 三维 bitmap root-head settlement | `internal/control/ordinal_exposure.go` |
| Typed NF 与静态语法检查 | `internal/queryplan/normalform.go` |
| 受限 Join/Union 双 SQL lowering | `internal/queryplan/relational.go` |
| Catalog collation/type/profile 条件 | `internal/catalog/validate.go` |
| PostgreSQL name/version/determinism 证明 | `internal/dataconnector/postgres.go` |
| 在线 paired execution/provenance | `internal/gateway/exposure.go`, `internal/gateway/relational_exposure.go` |
| 原子 ledger 结算 | `internal/control` |

“完整 Exposure Algebra”指本文件语法内的可执行、闭合代数及其 Observation/FactID/NF 语义。公网 `execute_plan` 除单产品 Scan/Select/Project/Group/Page 外，还 lowering 两个 Scan 叶子的不同 Catalog 角色 INNER equijoin，以及同产品双过滤分支 Union-Distinct；二者可继续 Group。该在线子集拒绝嵌套关系树、self-join、Union-All 和多输入 Page。本文不声称：任意 SQL provenance、outer join、`AVG`、negative information、排序位置泄露、差分隐私、跨引擎等价，或 Go 对本文数学模型的 mechanized refinement proof。
