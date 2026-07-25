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
to standard output, and refreshes `results.json`. It also retains the RQ3
`go test -race -json` log and a digest-bound integration artifact. The report
records the SHA-256 of `corpus.json`, both independent-oracle identities, the
normalized rewrite-pair-set digest, exact PostgreSQL version, and campaign seeds
where randomized or generated trials are used.
A nonzero exit means a ground-truth set, rewrite comparison, integration pass,
anti-arbitrage case, or planner oracle did not match.

## Research-question coverage

- **RQ1:** an independent reference package that imports none of the production
  exposure, query-plan, gateway, or policy code derives complete semantic Fact
  objects, witnesses, and hashes for 14 V2 cases over 12 expense and four
  department rows. Projection, positive and empty selection, NULL, group/global
  aggregation, join, and pagination compare 109 release and 261 influence
  case-level fact memberships; per-case set digests are retained.
- **RQ2:** a fixture evaluator that imports none of TaskGate's exposure,
  query-plan, or SQL-policy code supplies expected rows. A real PostgreSQL 16
  server executes 1,024 unique normalized baseline/rewrite SQL pairs from eight
  templates and 128 predicates. Generated attempts, unique pairs, executed
  unique pairs, duplicates, the normalization rule, and the pair-set digest are
  separate result fields. The templates are:
  predicate reorder, derived-table projection, CTE pushdown, De Morgan,
  correlated `EXISTS`, one-row `VALUES` join, and complete offset-page
  partitions of sizes two and three. Differential checks, metamorphic checks,
  executed PostgreSQL statements, and mismatches are also reported separately.
- **RQ3:** deterministic split/merge, overlapping pagination, retry, join
  multiplicity, and snapshot-update cases run here. Two named PostgreSQL control
  store tests cover task-family delegation and concurrent settlement. The
  publication result is accepted only if both test-level pass events and the
  package-level pass occur in a SHA-256-bound raw `go test -race -json` log.
- **RQ4:** `../exposure-performance/results.json` records three fresh isolated
  PostgreSQL deployments, 31,296 operations, five-stage ablation,
  latency/throughput, component timing, ledger growth, memory, contention, and
  history-hit metrics. The result is explicitly a controlled local fixture
  campaign rather than a TPC or production-scale measurement.
- **RQ5:** exact set-valued dual-budget optimization is checked on the
  two-requirement/shared-fact/budget-one counterexample and 500 brute-force
  instances. `../agenttasks/` separately replays
  120 four-part database-agent traces and scores selected answer tokens against
  gold assertions without consulting planner utility values.

`corpus.json`, the PostgreSQL oracle fixture, and the agent-task generator are
intentionally small enough to audit. The 1,024 count denotes unique normalized
SQL pairs, not independent datasets or workloads. TPC-scale, second-engine,
multi-node, and live-LLM generalization remain outside the reported scope.
