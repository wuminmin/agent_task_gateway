# TaskGate Formal Model (TKDE Revision)

This document states the compact model used by the TKDE revision. The
normative operator rules, typed FactID encoding, and normal-form rewrite
closure are specified in [Exposure Algebra V2](exposure-algebra-v2.md). The
model below abstracts their physical representation: a set may be stored as
validated FactID rows or as an exact ordinal bitmap without changing the
settlement rule.

## 1. Scope and notation

Let the three exposure dimensions be

\[
\mathcal J=\{R,D,O\},
\]

where `R` is release exposure, `D` is the positive-output dependency
footprint, and `O` is query-outcome exposure. The implementation retains the
field name `influence` for compatibility; throughout this model that field
means `D`, not causal influence.

For vectors of sets, union and difference are componentwise. For a vector of
finite sets \(X\), write

\[
|X|=(|X_R|,|X_D|,|X_O|),
\]

and compare its cardinalities componentwise with a numeric budget vector.
Every FactID is a versioned, typed, canonical semantic identity. Hashes and V4
ordinals are storage keys for those identities, not probabilistic
approximations.

The claims below assume an *admissible environment*: an attested Catalog and
schema, immutable reporting publications, deterministic PostgreSQL type and
collation semantics, unique stable entity keys, a well-typed plan in the
closed supported query language, and complete visible-result/provenance
execution in one read-only `REPEATABLE READ` snapshot. A failed premise makes
evaluation undefined and causes fail-closed rejection before result release.

## 2. Approved task and mutable state

An approved root task is

\[
T=(P,S,B,C).
\]

- \(P\) is the approved policy/grant: the principal and the allowed data
  products, fields, and mandatory scopes.
- \(S\) is the immutable snapshot/publication binding, including the
  datasource, schema, Catalog, snapshot, and dictionary needed to identify the
  execution epoch.
- \(B=(B_R,B_D,B_O)\in\mathbb N^3\) is the approved capacity vector for the
  three exposure dimensions.
- \(C\) is the signed set of semantic and execution constraints: the closed
  QueryPlan profile, type/collation and normal-form versions, bound View
  contracts, delegation attenuation, and result-release rules.

The approved tuple is immutable. In particular, it does **not** contain the
mutable exposure history. That history is a separate monotone root-family
ledger

\[
K(T)=(K_R(T),K_D(T),K_O(T)),\qquad K_j(T)\subseteq\mathcal F_j(S),
\]

initialized to three empty sets. Every delegated descendant resolves to the
same root ledger. A descendant may narrow policy or limits, but cannot create
a second ledger or enlarge \(P\), \(S\), \(B\), or \(C\). Unless stated
otherwise, \(T\) below denotes the approved root that owns this shared state.

## 3. Per-query and cumulative exposure

Let \(q\) be a query accepted by \(P\) and \(C\), and let \(I_S\) be the
database instance fixed by \(S\). Evaluation first remains internal to the
gateway and produces a finite candidate effect

\[
E(T,q)=\bigl(E_R(T,q),E_D(T,q),E_O(T,q)\bigr).
\]

The three components are different semantic objects and are never collapsed
into one weighted score.

### 3.1 Release exposure

\(E_R(T,q)\) contains the base or derived cell FactIDs that would appear in
the visible result artifact. A derived value identity binds its source/snapshot
bundle, canonical output key and expression, typed value, and witness
commitment. Consequently, equal display values from different semantic
derivations need not be the same release fact.

### 3.2 Positive-output dependency footprint

\(E_D(T,q)\) contains the base row/cell FactIDs selected by the inductive
operator rules as supporting successfully produced visible rows and cells.
Selection predicates on retained rows, join keys, group keys, distinctness
fields, and the complete declared aggregate inputs are included as specified
by the algebra.

This is a conservative, profile-relative positive-output dependency
footprint. It is neither minimal causal provenance nor the DBMS physical read
set. Failed predicate rows, absent groups, rows outside a page, order/rank-only
facts, and other negative information are outside the V2 footprint.

### 3.3 Query-outcome exposure

For a successful V3/V4 evaluation,

\[
E_O(T,q)=\{o_q\},
\]

where \(o_q\) binds the server-generated typed normal-form version and digest,
the normalized release-set digest, and visible row count. Thus a successful
empty result or zero aggregate still has an outcome fact. Different normalized
propositions do not collapse merely because they return the same value; a
replay with the same proposition and release result does collapse.

For V5, the outcome candidate is instead

\[
E_O(T,q)=A_{ctx}(q)\cup\{c_q\},
\]

where `A` is the exact set of normalized caller-controlled predicate atoms and
`c` is the unique composite outcome. The composite commits the atom-set digest
and cardinality, so \(|E_O|=|A|+1\). Atoms mean “tested”, not a per-condition
truth value. The signed constraint set \(C\) includes the atomizer version and
raw-literal, unique-atom, per-atom payload, and total-payload limits.
Both `A` and `c` are semantic Fact objects. The physical Merkle membership is
\(\{\operatorname{FactHash}(f):f\in E_O\}\); Control PostgreSQL retains each
canonical payload and rejects same-hash/different-payload collisions.

### 3.4 A query sequence

For a sequence of successfully settled queries
\(Q=\langle q_1,\ldots,q_n\rangle\), cumulative exposure is set union:

\[
E(T,Q)=\bigsqcup_{i=1}^{n}E(T,q_i)
=\left(\bigcup_iE_R(T,q_i),
       \bigcup_iE_D(T,q_i),
       \bigcup_iE_O(T,q_i)\right).
\]

Multiplicity matters while deriving witnesses, but the final ledgers are
sets. Repeating an already accounted FactID therefore does not consume another
unit. A candidate query rejected at settlement is not a member of \(Q\).

## 4. Delta, admission, and ledger transition

Given current root state \(K=K(T)\), TaskGate computes exact novelty

\[
\Delta_j(T,q\mid K)=E_j(T,q)\setminus K_j,
\qquad j\in\mathcal J,
\]

and the charged vector

\[
\operatorname{charge}(T,q\mid K)
=\bigl(|\Delta_R|,|\Delta_D|,|\Delta_O|\bigr).
\]

Write \(\operatorname{Valid}_{P,S,C}(q)\) for successful authorization,
binding validation, compilation, same-snapshot execution, and effect
validation. The exposure admission predicate is

\[
\operatorname{Admit}_K(T,q)
\iff
\operatorname{Valid}_{P,S,C}(q)
\land
\bigwedge_{j\in\mathcal J}|K_j\cup E_j(T,q)|\le B_j.
\]

If admission succeeds, the only legal transition is

\[
K'_j=K_j\cup E_j(T,q)=K_j\cup\Delta_j,
\qquad j\in\mathcal J.
\]

All three heads and their counts are published as one logical settlement. V4
uses an epoch-checked root-head compare-and-swap; a conflict rereads the new
head and recomputes all three differences. The legacy representation uses a
root-scoped database lock and unique FactID rows. In either representation,
an over-budget or persistence failure leaves all three \(K_j\) unchanged.

The visible result is staged privately before this transition. Settlement
atomically commits the three ledgers, terminal evidence, V7 settlement receipt,
and a PENDING artifact intent. Canonical promotion and AVAILABLE/consumption
audit are subsequent and recoverable. The legal ordering is
`Observed ⊆ Available ⊆ Accounted`; downloads (`Observed`) are intentionally
not tracked. Artifact promotion recovery does not rerun the business query or
refund an already committed settlement.

## 5. Properties

### 5.1 Budget safety

**Theorem (atomic root-family budget safety).** For every prefix of every
committed settlement history of root task \(T\),

\[
K_j(T)=\bigcup_{q\in\operatorname{settled}(T)}E_j(T,q)
\quad\text{and}\quad
|K_j(T)|\le B_j
\qquad (j\in\mathcal J).
\]

**Assumptions.** The ledger starts within its approved bounds; every writer
uses the settlement transition; FactID/ordinal decoding is exact; and the
database transaction or root-head CAS is atomic. Every descendant uses the
same root head.

**Proof sketch.** Induct on committed transitions. The base ledger is empty.
A rejected transition leaves it unchanged. An admitted transition replaces
each component by \(K_j\cup E_j\), and the admission predicate proves the new
cardinality is at most \(B_j\). Atomic three-dimensional publication rules out
a state containing only some components. Concurrent candidates linearize at a
successful epoch/lock; a loser recomputes its novelty from that committed
state. Set union establishes the history equality and its idempotence prevents
retry, overlap, or delegation from charging one FactID twice.

This theorem is scoped to one approved root family and one publication epoch.
It does not prevent a separately approved root or a later publication from
receiving a new ledger.

### 5.2 Canonical replay consistency

Let \(NF_C(q)\) be the typed normal form under the versions fixed by \(C\).
Let \(\chi_T(q)\) contain the full replay binding: issuing task and root
family, Grant/authorization digest, \(S\), Catalog/schema/dictionary set, View
binding, exposure profile, compiler, ordering/pagination, and result-encoding
versions. Define

\[
q_1 \equiv_T q_2
\iff
\chi_T(q_1)=\chi_T(q_2)
\land NF_C(q_1)=NF_C(q_2).
\]

**Theorem (canonical replay consistency).** For queries in the supported
normalizer rewrite closure over the same immutable instance,

\[
q_1 \equiv_T q_2 \Longrightarrow
E(T,q_1)=E(T,q_2).
\]

It follows that the two candidates have identical novelty from the same
ledger. Once one effect has committed, any later equivalent effect has
zero exposure charge in every dimension.

**Proof sketch.** The accepted normalizer removes only transformations proved
sound for the closed algebra: aliases and defined set-like orderings, canonical
INNER-equijoin graph traversal, and the explicitly supported union/page
rewrites. Structural determinism over a fixed snapshot gives identical
annotated results, release identities, and dependency sets. The outcome
identity binds the equal normal form and equal release/cardinality digest.
Componentwise set difference then gives equal deltas and, after the first
union, three empty deltas.

This is a soundness result, not a complete SQL-equivalence oracle. Different
normal forms may coincide on one dataset, and algebraic identities outside the
declared rewrite closure receive no guarantee. Zero *exposure* novelty also
does not waive ordinary query/row/DB-time accounting, reauthorization, audit,
or receipt generation.

### 5.3 View expansion preservation

Let \(G_S\) be the schema-qualified, attested relation registry fixed by \(S\).
For an accepted semantic View root \(v\), the partial compiler

\[
\operatorname{Expand}_{G_S,C}(v)=(e,\mu,\beta_v)
\]

recursively replaces ordinary View nodes with the corresponding operators in
the closed core algebra. Governed materialized Views remain opaque terminal
Products. The map \(\mu\) binds the public root interface to stable terminal
field IDs. The binding \(\beta_v\) retains root Product/field identity,
resolved expressions, the signed View contract, and predicate context before
the caller's allowed outer projection is composed.

**Theorem (view expansion preservation).** If expansion is defined, then for
the same public projection and immutable input instance:

\[
\begin{aligned}
\operatorname{Values}_{view}(v,I_S)
  &=\operatorname{Values}_{core}(e,I_S),\\
\operatorname{Annotations}_{view}(v,I_S)
  &=\operatorname{Annotations}_{core}(e,I_S),\\
E(T,q[v])&=E(T,q[e]^{\beta_v}).
\end{aligned}
\]

The first equality is PostgreSQL bag/result preservation, the second preserves
stable row/cell identity and positive-output annotations, and the third
preserves all three exposure dimensions only for the internal lowered query
that retains \(\beta_v\). An ordinary public query directly against a terminal
Product can have equal Release/dependency effects but different V5 Outcome
FactIDs because it represents a different public-product contract.

**Proof sketch.** Induct over the acyclic View dependency DAG. A terminal is
the same governed Scan. Direct projection/rename, conjunctive literal filter,
INNER equijoin, grouping, and each admitted aggregate map to the corresponding
typed core rule; the induction hypothesis supplies equal child bags and
annotations. Stable Catalog semantic identifiers rather than SQL aliases
determine the normalized fields and annotations from which FactIDs are
materialized. Canonical composition therefore yields the same annotated core
plan, while \(\beta_v\) preserves the V5 predicate bindings. Exact definition, transitive-dependency, canonical-plan, and
ordered-interface digests bind the induction inputs, and query-time
rediscovery detects drift before execution.

The result covers only the compiler's bounded fragment. Cycles, unsupported
relations, repeated stable roles, outer/cross/non-equality joins, arbitrary
subqueries, windows, set operations, and an invalid aggregate barrier fail
closed. It is not a theorem about arbitrary PostgreSQL View rewriting.

## 6. Claim boundary and evidence level

- Exposure is deterministic explicit-disclosure accounting, not differential
  privacy, mutual information, a complete knowledge state, or
  noninterference. A rejection may reveal an uncharged budget-threshold bit.
- The dependency dimension covers positive outputs under the declared rules;
  it does not cover all negative, order, timing, background-knowledge, or
  physical-read information.
- Snapshot immutability and publication integrity are trusted premises. The
  model does not cover mutable OLTP/CDC input or migration of an active ledger
  across publication epochs.
- Hash/payload disagreement, noncanonical bitmap encoding, an invalid ordinal,
  or incomplete provenance evidence fails closed; safety does not rely on a
  Bloom filter or silent hash-collision assumption.
- The proof sketches are mathematical arguments for the specified abstraction.
  [ExposureLedger.tla](../formal/ExposureLedger.tla),
  [ArtifactPublication.tla](../formal/ArtifactPublication.tla),
  [ExposureBitmapRefinement.tla](../formal/ExposureBitmapRefinement.tla), and
  [OutcomeHashSetRefinement.tla](../formal/OutcomeHashSetRefinement.tla) add
  finite-state model checks, while [REFINEMENT.md](../formal/REFINEMENT.md)
  maps abstract actions to code and tests. They are not a mechanized proof of
  the Go implementation or of arbitrary SQL semantics.

Implementation terminology and current operational boundaries are documented
in [exposure-accounting.md](exposure-accounting.md),
[exposure-v4.md](exposure-v4.md), and [architecture.md](architecture.md).
