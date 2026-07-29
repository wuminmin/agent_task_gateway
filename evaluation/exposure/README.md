# Exposure evaluation

This suite is the deterministic evidence layer for the TKDE-oriented TaskGate
V2 model. The source-controlled corpus drives `taskgate-exposure-v2` typed
FactID, witness, NULL, Join, Group, Page, and Observation semantics; it
does not reuse the older gateway latency measurements as exposure evidence.
The report field `influence` is retained for compatibility and denotes the
contract-defined conservative positive-output dependency footprint, not causal
influence or PostgreSQL's physical read set.

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
or anti-arbitrage case did not match.

## Research-question coverage

- **RQ1:** an independent reference package that imports none of the production
  exposure, query-plan, gateway, or policy code derives complete semantic Fact
  objects, witnesses, and hashes for every source-controlled V2 case. The
  counterexamples include hidden group keys, hidden union-distinct fields,
  `COUNT(a)` NULL inputs, `MIN/MAX` non-extrema, TRUE/FALSE/UNKNOWN selection,
  and page/order boundaries; exact totals and per-case set digests are generated
  in `results.json`.
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
  A second closed-language stream checks release/dependency FactSet and
  incremental-charge invariance for hidden group-key ordering, union operand
  exchange, idempotence, and duplicate-branch collapse in addition to the
  existing projection, selection, join, and group rewrites.
- **RQ3:** deterministic split/merge, overlapping pagination, retry, join
  multiplicity, and snapshot-update cases run here. Two named PostgreSQL control
  store tests cover task-family delegation and concurrent settlement. The
  publication result is accepted only if both test-level pass events and the
  package-level pass occur in a SHA-256-bound raw `go test -race -json` log.
- **RQ4:** `../exposure-performance/results.json` records three fresh isolated
  PostgreSQL deployments, 7,896 full-path operations and 23,400 ablation
  operations (31,296 total), five-stage decomposition,
  latency/throughput, component timing, ledger growth, memory, contention, and
  history-hit metrics. Its published raw JSONL is rebuilt and source-digest
  checked during paper generation. The result is explicitly a controlled local
  fixture campaign rather than a TPC or production-scale measurement.
`corpus.json` and the PostgreSQL oracle fixture are
intentionally small enough to audit. The 1,024 count denotes unique normalized
SQL pairs, not independent datasets or workloads. TPC-scale, second-engine,
multi-node, and live-LLM generalization remain outside the reported scope.
