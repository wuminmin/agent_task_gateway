# TaskGate TKDE working draft

This is a separate database-paper project derived from the implementation and
the historical gap analysis in [../../tkde.md](../../tkde.md). Its current
title is *Task-Scoped Accounting of Cumulative Query Exposure over Versioned
Reporting Snapshots*. It is not a template
conversion of the TDSC security-gateway draft.

## Versioning and submission status

SessionBound, arXiv:2607.00751v1, is a preliminary preprint version that
introduced the task-scoped authorization substrate. The associated TDSC
working draft was never submitted to a journal or conference and is not under
review. The present TKDE manuscript is the substantially revised article; it
develops the database-specific accounting semantics, implementation, formal
analysis, and evaluation.

The manuscript cites and identifies the preliminary preprint once in the
Introduction. It does not treat SessionBound as an independently published
system or repeatedly compare TaskGate against it in Related Work. At submission
time, the version relationship and material differences must be disclosed in
the cover letter and in the arXiv comments. A later arXiv upload will be a new
version and will not remove v1 from the version history.

The manuscript targets governed reporting replicas refreshed by scheduled
daily ETL/synchronization, not mutable OLTP primaries or continuous CDC. Any
deployment alias such as `latest` is resolved to a concrete Catalog/publication
in the authorization snapshot presented for approval; approval confirms that
binding rather than rewriting it. Active roots remain bound to their approved
publication and are version-routed until completion.

The online join boundary is
`SQL AST -> canonical equi-join graph -> JoinMany -> deterministic binary algebra/effect fold`.
It accepts connected graphs of
2–16 distinct Catalog stable roles, arbitrary graph topology and INNER JOIN
parenthesization that lower to the same graph, and multiple typed equality
predicates per edge. The 16-source ceiling is an operational complexity guard.
Disconnected graphs, self-joins, outer joins, cross joins, and non-equality
join predicates remain fail-closed. A graph-only structural digest is for
tests and diagnostics. The complete typed-algebra normal-form digest
remains the query-semantic component of replay and outcome identity;
authorization and settlement additionally bind the existing canonical plan,
grant/policy, and derived-effect context.

Build from the repository root:

```sh
make paper
```

Validate all source-controlled evidence and deterministically regenerate only
the LaTeX evidence macros with:

```sh
make paper-evidence
```

The build first runs `make eval-exposure`, which refreshes the deterministic
exposure report. `generate_evidence.py` then verifies:

- the report's `corpus.json` SHA-256 and fixed rewrite seed;
- RQ2's independent-oracle fixture digest, real PostgreSQL 16 version,
  generated/unique rewrite counts, check counts, and zero mismatches;
- complete RQ1/RQ2/RQ3 oracle and integration results;
- all raw files for the three-trial RQ4 campaign, including 7,896 full-path and
  23,400 ablation samples, reconstructed summaries, environment digest, and
  the digest of the archived, historically tested gateway/benchmark source;
- the three-scale representative two-source PostgreSQL 16 Join--Group report,
  its 27 direct/novel/replay points, exact million-fact accounting,
  raw-artifact digest, implementation digest, and service memory peaks;
- the source-controlled V4 acceptance manifest, archived tested-source digest,
  current-tree divergence metadata, fixed environment and verification
  receipts, all 30 implemented gates and all 560 measured operations,
  including exact overlap, replay identity,
  latency, cgroup-memory, network, WAL, offline-build, artifact, activation,
  storage, and small-query-regression checks;
- the V4 small-query candidate's 832 raw operation samples and Docker-memory
  observations, from which its five benchmark cells are reconstructed before
  comparison with the digest-bound legacy baseline;
- the V4 supplemental layout, same-root concurrency, and million-Fact oracle
  pack, whose four source bindings are reconstructed from the retained
  `fede479...` archive while later source changes remain disclosure metadata;
- the production Control PostgreSQL storage curve and budget-boundary trials;
- compact source snapshots for the legacy performance, storage, and scale
  campaigns, each uniquely bound to commit `38a35d7...` and re-hashed from
  exact archived bytes instead of being compared with the newer V4 tree;
- the sealed RQ5 offline pack, reconstructing four 345,000-row publications,
  twelve measured build/strict-verify/receipt-bound-activation cycles, phase
  timings, direct-builder VmHWM, and artifact descriptors;
- the compact RQ5 online descriptor pack and its three retained-version
  transitions, independently re-hashing Catalogs, approved inputs, bundle
  manifests, dataset bindings, timings, and all five correctness conditions;
- the model, configuration, and raw-log hashes in
  `formal/results/exposure_ledger.json` and
  `formal/results/exposure_bitmap_refinement.json`;
- the V5 Outcome bounded production/test source manifest, source-set digest,
  base implementation commit, raw `go test -json` execution receipt, exact
  radix counters, and full-rebuild-reference match;
- the model, configuration, and raw-log hashes for the abstract V5 Outcome-set
  settlement and recoverable Artifact Publication safety models.

It then emits `generated/evidence.tex` and compiles both `main.tex` and the
separate `supplement.tex` with a pinned Docker TeX environment. The broad 1,024
pair PostgreSQL result-equivalence stress test is confined to the supplement;
the main paper's RQ2 evidence is closed-language FactSet/charge invariance.
Complete typing/big-step rules, output-key cases, and the detailed operator
coverage matrix are also in the supplement; the main paper retains the effect
summary and safety properties. Detailed legacy materialized-ledger timing,
storage, and Join--Group diagnostics are likewise in the supplement; the main
paper retains one scoped Legacy--V4 contrast. A local TeX syntax build is
available through `paper/tkde/build.sh`, but it still uses Docker for the
exposure evaluation.

The draft reports controlled single-host evidence, including a TPC-H-derived
(not official TPC-H) workload up to 225,000 joined members and 1,035,000
dependency facts. The V4 bundle covers one fresh deployment with warm verified
indexes plus separately scoped bitmap-layout, same-root contention, and
million-fact offline-oracle studies. It does not claim second-engine,
multi-node, arbitrary-SQL, mutable-source, production-SLO, or live-LLM
generality. RQ5 executes a four-publication daily-refresh campaign with three
measured builds per publication and a separate retained-route campaign over
the same 345,000-row fixture. Its five cross-publication correctness conditions
all pass, while the paper preserves the experiment-router, warm-cache,
direct-child-memory, and omitted-payload boundaries. The unsubmitted TDSC
working draft remains available through `make paper-tdsc`; its results are not
relabeled as exposure evidence. The source-controlled semantic, performance,
storage, and scale reports are all consumed by the paper build.

## Submission-length check

Checked 2026-07-27: the IEEE Computer Society's public author guidance defines
12 formatted pages for a regular Transactions paper, including references and
author biographies, and the [2026 IEEE charge
list](https://magazines.ieeeauthorcenter.ieee.org/wp-content/uploads/sites/10/IEEE-Article-Processing-Charges-List.pdf)
lists TKDE at 12 pages before mandatory overlength charges of USD 220 per page.
The [Computer Society author
guidance](https://www.computer.org/publications/author-resources) warns that a
journal's submission-page limit can differ from its production overlength
threshold. Therefore the corresponding author must still confirm the selected
TKDE manuscript category, initial-submission limit, treatment of supplementary
material, and current charge acknowledgement in ScholarOne/Author Portal
immediately before submission. Do not infer acceptability from a successful
local PDF build alone. The 2026-08-01 pinned-template syntax build is 14
main-paper pages plus a separate 12-page supplement. This is inside the working
12--14-page reduction target but remains two pages above the cited production
threshold; the initial-submission and supplement rules still require
confirmation before freezing the submission.
