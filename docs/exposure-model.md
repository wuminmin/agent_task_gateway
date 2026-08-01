# TaskGate Exposure Model

TaskGate controls the cumulative data exposure of an approved autonomous-agent
task. The accounting subject is not one SQL statement, connection, or agent
process: it is the approved root task and every delegated agent execution in
that root task family. Individually authorized queries can therefore still be
denied when their combined exposure would exceed the family's approved
capacity.

This document gives the compact set model. The complete operator semantics are
specified in [Exposure Algebra V2](exposure-algebra-v2.md), and the physical V4
bitmap representation is described in [TaskGate V4](exposure-v4.md).

## Exposure space

At the paper-level abstraction, within a publication-owned fact namespace, a
database fact has the identity

\[
F=(\mathit{snapshot},\mathit{entity},\mathit{attribute},\mathit{version}).
\]

- `snapshot` identifies the immutable reporting publication in which the fact
  is interpreted. It is a Catalog-managed version, not a PostgreSQL MVCC
  transaction ID.
- `entity` is a canonical stable entity key. A derived result uses a canonical
  output-row key instead.
- `attribute` is a stable field identifier, the row-existence marker, or a
  canonical derived expression.
- `version` distinguishes the typed value and, where required, the provenance
  witness or query outcome that gives the fact its meaning.

The namespace is implicit in the four-tuple but is part of global identity. It
prevents equal entity and field names in different governed products from
colliding. A snapshot change also creates a different identity even if an
entity's displayed value is unchanged. This is intentional: facts from two
publication contracts must never be silently merged.

The four-tuple is an abstraction, not a claim that every persisted Go `FactID`
has exactly four JSON fields. The compatibility V1 payload is
`(product, snapshot, entity_key, field, value_version)`. V2/V3 use disjoint,
tagged canonical payloads:

- a base-row fact binds source namespace, snapshot, and entity key;
- a base-cell fact additionally binds stable field ID, canonical SQL type, and
  typed value;
- a derived fact binds a canonical snapshot bundle, output-row key,
  normalized expression, typed value, and witness commitment;
- an outcome fact binds the server-generated query normal form, release-set
  digest, and visible row count.

Canonical FactIDs are hashed for durable lookup. V4 assigns immutable
`uint32` ordinals to published base facts and represents their sets with exact
compressed bitmaps; derived release facts and outcome facts remain in an exact
dynamic dictionary. Hashes and bitmaps are storage representations, not Bloom
filters or probabilistic estimates.

## Task exposure ledger

Let \(T\) denote an approved root task family. TaskGate maintains three
separate, monotone ledgers:

\[
\operatorname{Ledger}(T)=
\bigl(L_{\mathrm{release}}(T),
      L_{\mathrm{dependency}}(T),
      L_{\mathrm{outcome}}(T)\bigr).
\]

Their meanings are:

- **\(L_{\mathrm{release}}\)** records base or derived facts placed in
  successful visible result artifacts.
- **\(L_{\mathrm{dependency}}\)** records base row/cell facts in the
  positive-output dependency footprint of successful results. The current API,
  database schema, and receipts retain the compatibility label `influence` for
  this dimension; it does not mean causal influence.
- **\(L_{\mathrm{outcome}}\)** records successful normalized query
  propositions and their outcomes. An empty result or zero-valued aggregate is
  therefore not automatically free, while a replay of the same proposition
  and result can deduplicate.

All descendants created by delegation resolve to the same root ledger. A
delegated grant can only narrow permissions and absolute limits; it cannot
create a private ledger or switch publication. Separately approved root tasks
have separate ledgers.

## Query effect and positive delta

After authorization and deterministic compilation, a query \(q\) produces a
candidate exposure vector while its result remains withheld:

\[
E(T,q)=
\bigl(E_{\mathrm{release}},
      E_{\mathrm{dependency}},
      E_{\mathrm{outcome}}\bigr).
\]

The notation in the high-level design,

\[
\Delta(T,q)=\operatorname{Exposure}(q)-\operatorname{Ledger}(T),
\]

means componentwise set difference, not signed arithmetic. Precisely, for
\(j\in\{\mathrm{release},\mathrm{dependency},\mathrm{outcome}\}\),

\[
\Delta_j(T,q)=E_j(T,q)\setminus L_j(T).
\]

The charged budget is the vector of positive novelty:

\[
\operatorname{charge}(T,q)=
\bigl(|\Delta_{\mathrm{release}}|,
      |\Delta_{\mathrm{dependency}}|,
      |\Delta_{\mathrm{outcome}}|\bigr).
\]

Only these new set members consume exposure capacity. A previously accounted
fact contributes zero rather than a negative credit; no query can refund or
trade facts between dimensions. "Positive" here refers both to set novelty
and to dependencies of produced outputs, not to a positive numeric result.

For an approved capacity vector
\(B=(B_{\mathrm{release}},B_{\mathrm{dependency}},B_{\mathrm{outcome}})\),
admission requires, componentwise,

\[
|L_j(T)|+|\Delta_j(T,q)|
=|L_j(T)\cup E_j(T,q)|
\le B_j.
\]

If every dimension is within its limit, all three ledgers advance atomically:

\[
L'_j(T)=L_j(T)\cup E_j(T,q)=L_j(T)\cup\Delta_j(T,q).
\]

If any dimension exceeds its limit, or if evidence validation or persistence
fails, none of the three ledger heads advances and the result is not released.

### Example

Suppose the current ledger and a candidate query contain:

| Dimension | Current ledger | Candidate effect | Positive delta |
|---|---|---|---|
| Release | `{r1, r2}` | `{r2, r3}` | `{r3}` |
| Dependency | `{d1, d2}` | `{d2, d3, d4}` | `{d3, d4}` |
| Outcome | `{o1}` | `{o2}` | `{o2}` |

The query is charged `(1, 2, 1)`, not the candidate cardinalities `(2, 3, 1)`.
After settlement, retrying the same normalized query and result has exposure
delta `(0, 0, 0)`. Ordinary query, row, and database-time accounting may still
apply to that retry.

## What each dimension means

Accounted result exposure (wire label `release`) measures result values selected
for publication and charged by successful settlement. It upper-bounds the
facts in an `AVAILABLE` canonical artifact and does not claim the artifact was
downloaded or observed. Equal display values derived from different snapshot
bundles, expressions, or witnesses need not have the same FactID.

The dependency footprint is derived by the closed relational algebra. It
includes, as applicable, retained-row existence, projected cells, predicates
on retained rows, join and group keys, DISTINCT fields, and declared aggregate
inputs. Multiplicity is used while constructing a witness, but the settled
ledger is a set.

Outcome exposure adds one fact for a successful V3/V4 query. Its identity
binds the typed canonical query proposition and the observed release digest and
row count. This prevents a sequence of different empty or zero-result probes
from being completely unaccounted, but it does not estimate how many bits each
answer reveals.

## Enforcement mapping

The current V4 implementation realizes the set transition as follows:

1. The TaskGate Enforcement Layer validates the task, signed Catalog and View bindings, controlled
   analytical SQL profile, and exposure profile.
2. Visible output and its provenance companion are evaluated in one read-only
   `REPEATABLE READ` transaction; the result is staged privately and withheld.
3. Base-fact novelty is exact bitmap `ANDNOT` followed by `popcount`; dynamic
   facts use exact hash/payload identity. Ledger union is bitmap `OR` plus
   dynamic-set union.
4. Release, compatibility-named influence, and outcome heads are published by
   one root-epoch compare-and-swap. A conflict rereads the committed head and
   recomputes all three deltas.
5. Settlement, artifact intent, audit record, and receipt commit together.
   Canonical encrypted-object creation is the implemented consumption boundary;
   only an available artifact can be presented to the agent.

An interrupted post-commit artifact promotion is recovered without rerunning
the query or refunding exposure. If the committed object evidence cannot be
reconciled, readiness fails closed.

## Scope and limitations

This is deterministic, profile-relative accounting of explicit release,
positive-output dependency, and normalized outcome facts. It is not
differential privacy, mutual-information accounting, a complete model of an
agent's knowledge, or a DBMS physical-read audit. In particular, the current
dependency rules do not charge failed predicate rows, absent groups, page-excluded
rows, ordering-only facts, timing signals, or arbitrary background inference.

The guarantee is scoped to one approved root family and one immutable
publication epoch. A separately approved task or a task on a later publication
starts a new ledger; TaskGate currently provides no principal-wide exposure
ledger across publications. Mutable OLTP/CDC serving and arbitrary SQL are
outside the implemented V4 boundary. See
[Versioned Publication](versioned-publication.md) for how changing business
data is incorporated without rebinding an active task.
