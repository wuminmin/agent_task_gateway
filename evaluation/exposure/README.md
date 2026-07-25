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
- **RQ4:** `../exposure-performance/results.json` records three fresh isolated
  PostgreSQL deployments, 31,296 operations, five-stage ablation,
  latency/throughput, component timing, ledger growth, memory, contention, and
  history-hit metrics. The result is explicitly a controlled local fixture
  campaign rather than a TPC or production-scale measurement.
- **RQ5:** exact dual-budget optimization is checked against declared expected
  choices and 500 brute-force instances. `../agenttasks/` separately replays
  120 four-part database-agent traces and scores selected answer tokens against
  gold assertions without consulting planner utility values.

`corpus.json`, the PostgreSQL oracle fixture, and the agent-task generator are
intentionally small enough to audit. TPC-scale, second-engine, multi-node, and
live-LLM generalization remain outside the reported scope.
