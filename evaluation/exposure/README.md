# Exposure evaluation

This suite is the deterministic evidence layer for the TKDE-oriented TaskGate
V2 model. The source-controlled corpus drives `taskgate-exposure-v2` typed
FactID, witness, NULL, Join, Group, Page, and Observation semantics; it
does not reuse the older gateway latency measurements as exposure evidence.

Run it with:

```sh
make eval-exposure
```

The command builds a disposable PostgreSQL 16 instance, emits one JSON report
to standard output, and refreshes `results.json`. The report records the
SHA-256 of `corpus.json`, the independent-oracle fixture digest, the exact
PostgreSQL version, and the fixed rewrite seed. A nonzero exit means a
ground-truth count, rewrite comparison, anti-arbitrage case, or planner oracle
did not match.

## Research-question coverage

- **RQ1:** five manually enumerated V2 SPJA/aggregate/join ground-truth cases check
  counts and committed SHA-256 digests of the exact release and source-influence
  fact sets.
- **RQ2:** a fixture evaluator that imports none of TaskGate's exposure,
  query-plan, or SQL-policy code supplies expected rows. A real PostgreSQL 16
  server executes 1,024 unique instantiated rewrites from eight templates:
  predicate reorder, derived-table projection, CTE pushdown, De Morgan,
  correlated `EXISTS`, one-row `VALUES` join, and complete offset-page
  partitions of sizes two and three. The report separately records generated
  attempts, unique rewrites, templates, differential checks, metamorphic
  checks, executed PostgreSQL statements, and mismatches.
- **RQ3:** deterministic split/merge, overlapping pagination, retry, join
  multiplicity, and snapshot-update cases run here. Task-family delegation and
  concurrent settlement are identified separately and run by the PostgreSQL
  race suite in `internal/control` and `internal/gateway`.
- **RQ4:** the report contrasts query-count, row-count, serialized-byte,
  weighted-cell, no-history provenance, and full history-aware charges. Runtime
  overhead is deliberately `not_measured` until a fresh external PostgreSQL
  campaign is recorded. `../exposure-performance/` now provides the real
  public-MCP path, five-stage ablation, ledger/lock/history instrumentation,
  and a local Compose smoke; the bundled demo result remains engineering
  evidence and is not promoted to a publication claim. Go benchmarks are
  development diagnostics, not paper measurements.
- **RQ5:** exact dual-budget optimization is checked against declared expected
  choices and a deterministic utility-greedy baseline using answer completeness
  and query coverage only.

`corpus.json` and the PostgreSQL oracle fixture are intentionally small enough
to audit. Publication-scale TPC-H, TPC-DS, second-engine, agent-task, and
overhead campaigns remain external experiments and must retain their own data,
environment, and run provenance.
