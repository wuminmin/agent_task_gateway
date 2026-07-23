# Exposure evaluation

This suite is the deterministic evidence layer for the TKDE-oriented TaskGate
model. The source-controlled corpus drives the executable exposure algebra; it
does not reuse the older gateway latency measurements as exposure evidence.

Run it with:

```sh
make eval-exposure
```

The command emits one JSON report to standard output and refreshes
`results.json`. The report records the SHA-256 of `corpus.json` and the fixed
rewrite seed. A nonzero exit means a ground-truth count, rewrite pair,
anti-arbitrage case, or planner oracle did not match.

## Research-question coverage

- **RQ1:** five manually enumerated SPJA/aggregate/join ground-truth cases check
  counts and committed SHA-256 digests of the exact release and source-influence
  fact sets.
- **RQ2:** a fixed seed generates 1,024 equivalent projection/selection and
  pagination pairs. The report counts mismatches.
- **RQ3:** deterministic split/merge, overlapping pagination, retry, join
  multiplicity, and snapshot-update cases run here. Task-family delegation and
  concurrent settlement are identified separately and run by the PostgreSQL
  race suite in `internal/control` and `internal/gateway`.
- **RQ4:** the report contrasts query-count, row-count, serialized-byte,
  weighted-cell, no-history provenance, and full history-aware charges. Runtime
  overhead is deliberately `not_measured` until a fresh external PostgreSQL
  campaign is recorded. Go benchmarks are development diagnostics, not paper
  measurements.
- **RQ5:** exact dual-budget optimization is checked against declared expected
  choices and a deterministic utility-greedy baseline using answer completeness
  and query coverage only.

`corpus.json` is intentionally small enough to audit. Publication-scale TPC-H,
TPC-DS, second-engine, agent-task, and overhead campaigns remain external
experiments and must retain their own data, environment, and run provenance.
