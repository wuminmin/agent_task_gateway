# TaskGate TKDE manuscript

This directory contains the current database-paper manuscript, *Task-Scoped
Accounting of Cumulative Query Exposure over Versioned Reporting Snapshots*.
The paper presents one coherent TaskGate design: typed release, dependency,
and Outcome exposure; immutable publication dictionaries; streamed compressed
bitmaps; an exact Merkle-radix Outcome set; atomic root-family settlement; and
signed publication receipts. Internal artifact identifiers are not manuscript
module names.

The manuscript targets governed reporting replicas refreshed by scheduled
ETL/synchronization, not mutable OLTP primaries or continuous CDC. Deployment
aliases such as `latest` resolve to a concrete Catalog publication during
approval. Active roots stay bound to that publication and route to its retained
deployment until completion.

The online join boundary is:

```text
SQL AST -> canonical equi-join graph -> JoinMany -> deterministic algebra/effect fold
```

It accepts connected graphs of 2–16 distinct Catalog stable roles, arbitrary
accepted INNER JOIN parenthesization, and multiple typed equality predicates
per edge. Disconnected graphs, self joins, outer/cross joins, and non-equality
join predicates fail closed. The complete typed-algebra normal form contributes
to replay and Outcome identity; authorization and settlement additionally bind
the plan, grant, policy, publication, and effect context.

## Build and validation

Build both the paper and supplement from the repository root:

```sh
make paper
```

Validate source-controlled evidence and regenerate the LaTeX evidence macros:

```sh
make paper-evidence
```

The evidence generator validates the following current claims before writing
`generated/evidence.tex`:

- RQ1 independent-oracle memberships;
- RQ2 closed-language FactSet and charge invariance;
- RQ3 deterministic conservation, integration, delegation, and race cases;
- ordinal/bitmap acceptance, layout, contention, and million-Fact oracle gates;
- exact Outcome-set permutation, novelty, replay, tamper, and touched-branch
  radix behavior;
- daily publication build, verification, activation, retained routing, and
  rollover conditions;
- the finite-state ledger, bitmap-refinement, Outcome-set, and recoverable
  artifact-publication checks.

The manuscript keeps three assurance layers separate:

1. paper theorems for the algebra and representation-independent properties;
2. bounded TLA+ safety models for settlement, concurrency, and publication
   recovery after SQL-to-provenance compilation;
3. executable Go/PostgreSQL differential, concurrency, tamper, and
   fault-injection evidence for implementation correspondence.

The models abstract the parser, complete AST, PostgreSQL rows, concrete hashes
and signatures, network, and ciphertext. Radix partition/object reuse is tested
in executable regressions. The action/transaction/test map is an audit artifact,
not a mechanized refinement proof; the paper does not claim a formally verified
system.

The build compiles `main.tex` and `supplement.tex` in the pinned Docker TeX
environment. A syntax-oriented entry point is available at `build.sh`. The
supplement contains complete inference rules, proofs, implementation coverage,
radix/bitmap details, receipt persistence semantics, publication-routing
measurements, and broad SQL result-equivalence stress tests. Superseded module
walkthroughs and implementations are intentionally excluded.

The reported experiments are controlled, single-host evidence. They do not
claim second-engine, multi-node, arbitrary-SQL, mutable-source, production-SLO,
or live-agent generality. The final publication-scale WSL2 campaign remains the
single full experiment to run after the manuscript and protocol are frozen.

## Related-manuscript disclosure

SessionBound and this TKDE manuscript have the same author. SessionBound is an
unsubmitted working manuscript available as a preliminary arXiv preprint; it
has not been submitted to or accepted by TDSC. The Introduction states that
status explicitly. Submission materials must disclose the relationship and the
database-specific contributions of this manuscript; the paper does not organize
its technical content around that evolution history.

## Submission-length check

The pinned-template build is currently 13 main-paper pages plus a separate
supplement after adding the expanded DFC/Passant comparison.
Before upload, the corresponding author must confirm the current TKDE category,
initial-submission limit, supplementary-material rules, and any overlength
acknowledgement in the submission portal. A successful local build alone does
not establish submission compliance.
