# TKDE experiment execution guide

This guide defines the six experiments required for the TKDE revision. It is
an execution protocol, not a report of completed experiments. Every result
cell below is intentionally blank. Do not replace a blank with an estimate,
an older result, a smoke-test value, or a value from a different measurement
boundary.

TaskGate's `influence` field name is retained in some machine-readable
artifacts for compatibility. In prose and table labels, it means the
**positive-output dependency footprint**, not causal influence or a database
physical-read set.

## 0. Evidence rules and current harness coverage

### 0.1 Rules that apply to every experiment

1. Run from a recorded Git commit. Preserve `git status --short`; a dirty
   worktree must be disclosed and must not be mixed with a clean-run campaign.
2. Record a run-specific environment manifest: CPU, memory, storage, kernel,
   Go, Docker, PostgreSQL, container image IDs/digests, database parameters,
   cache policy, dataset generator/seed, and dataset/import fingerprints.
3. Use immutable data for every paired comparison. Pair baselines by dataset,
   query semantics, result-drain boundary, cache policy, and repetition count.
4. Give every measured TaskGate cell a fresh root task unless the experiment
   explicitly studies history within one root. Never carry exposure history
   from one replicate or attack case into another.
5. Complete correctness checks before accepting timing data. At minimum,
   compare ordered or canonical multiset result hashes and fail the cell on a
   mismatch.
6. Define latency as client wall-clock time through complete result drain.
   Define throughput as successful, fully drained operations divided by the
   measured cell interval. Failed requests are reported separately and are
   never silently removed from a throughput denominator.
7. Use Hyndman--Fan type-7 percentiles, matching
   `evaluation/plots/generate.py`. Report the raw sample count. For the
   four-baseline runner, retain its deterministic p50 bootstrap interval; do
   not report a p99 from a small campaign.
8. Randomize paired baseline order with a recorded seed. Complete warmups
   before measurement. If a cold-cache claim is made, use a recorded reset
   command; do not infer coldness from a first request.
9. Use at least three independent fresh deployments for a publication
   performance campaign. Keep per-deployment summaries visible rather than
   pooling away deployment variation.
10. Preserve raw samples, exact configuration bytes, workload definitions,
    environment and dataset manifests, stdout/stderr, and SHA-256 bindings.
    Derived CSV, plots, and LaTeX are not substitutes for raw evidence.

### 0.2 What can and cannot be run from the current repository

| Experiment | Reusable repository support | Missing work before the requested table is publishable |
|---|---|---|
| 1. PostgreSQL baseline | `evaluation/cmd/v5-full`, final-V5 strict orchestration/sample framework, S1--S6 matrix, and public-path pipeline/artifact timers are implemented | The author must bind the site adapter and private frozen inputs, then run the three-deployment WSL2 campaign; every numeric cell remains blank. |
| 2. PostgreSQL RLS | `evaluation/cmd/rls-adaptive`, real RLS DDL/introspection, independent trace-union oracle, preregistered budget rule, and raw schema are implemented | The site adapter, private frozen task/role inputs, and three-deployment campaign remain for the author; `native_view` is still not RLS. |
| 3. Adaptive attacks | `evaluation/cmd/adaptive-attacks` and the A--E/reverse-direction protocol are implemented alongside existing semantic/oracle checks | No publication A--E sample has been executed. |
| 4. ProvSQL | The historical external baseline is preserved; `evaluation/cmd/taskgate-provsql-pair` orchestration and the connected nonce-join fixture are implemented | The site adapter and fair paired three-system campaign remain, so no TaskGate value may be joined to the old smoke. |
| 5. View DAG scalability | `CompileMeasured` and `evaluation/cmd/view-scale` expose the preregistered depth/source matrix and non-overlapping stages | The five-process, 4,500-compiles-per-deployment campaign (13,500 across three deployments) has not run. |
| 6. Concurrency | `evaluation/cmd/v5-concurrency`, widths 10/50/100/500, forced/natural modes, and explicit production CAS attempt/conflict counters are implemented | The 30-fresh-root cells have not run; width 500 remains invalid without offered-concurrency evidence. |

The final-V5 framework is under `evaluation/final-v5-wsl2/`. Its tiny smoke is
not evidence. Until a frozen, digest-bound, three-deployment publication
campaign passes its finalizer, leave every numeric cell below blank. Current
V5 publication-scale performance remains pending the final frozen WSL2
campaign.

### 0.3 Common repository preflight

Validate the existing four-baseline configurations and workload paths without
contacting a backend:

```sh
make eval-validate
```

Run the existing deterministic exposure semantics and PostgreSQL-oracle suite:

```sh
make eval-exposure
```

These commands are useful preconditions, but neither one by itself completes
all six experiments.

## 1. PostgreSQL baseline performance

### Motivation

Measure the end-to-end execution cost of TaskGate relative to direct
PostgreSQL for four representative reporting shapes: projection/filter,
multi-table join, aggregation, and a query over a nested semantic View. This
experiment answers an overhead question; it does not establish security
correctness, which is checked separately.

### Setup

- Use the repository's TPC-derived SF1 and SF10 evaluation datasets and their
  recorded manifests. Do not call the workload TPC compliant unless all
  official TPC rules are independently satisfied.
- Use one isolated deployment per baseline so that the direct client and
  TaskGate do not contend for the same database resources during a cell.
- Use the same PostgreSQL major/minor version, data, indexes, database settings,
  host class, result row cap, statement timeout, and warm-cache policy.
- Define one query per required shape:

  - `SELECT`: projection plus a selective predicate;
  - `JOIN`: an INNER equi-join over approved stable keys;
  - `GROUP BY`: a grouped aggregate;
  - `VIEW`: a query whose approved semantic root expands through a nested View
    chain.

- Each query must have deterministic output ordering. Baseline-specific SQL may
  use physical tables, reporting views, or logical products, but its result
  multiset and selected output interface must be identical.
- Use concurrency 1 for the primary latency-overhead table. The existing
  runner's concurrency 8 and 32 cells may be retained as a secondary
  throughput analysis only after the target full TaskGate path is wired.

The checked-in TPCH/TPCDS manifests currently contain only grouped aggregate
queries. They do **not** contain the required SELECT, JOIN, or nested-View
cells. In addition, `resource_taskgate` in `evaluation/cmd/runner` omits the
exposure ledger/budget path. It must not be labeled as the requested TaskGate
baseline.

### Baselines

- **B0 -- direct PostgreSQL:** a persistent read-only PostgreSQL connection to
  the physical benchmark tables, with the complete result drained.
- **B1 -- full TaskGate:** the public exposure-enabled execution path including
  authorization, visible/provenance execution, fact derivation, history set
  difference, budget check, atomic settlement, receipt, and result release.

The existing `native_view` and `ast_only_gateway` paths are useful diagnostic
ablations. Report them separately if run; neither replaces B0 or B1.

### Metrics and calculations

- End-to-end latency p50 and p95 in milliseconds for each workload and
  baseline.
- Throughput in completed queries per second (QPS) for each workload and
  baseline.
- `p50 overhead ratio = TaskGate p50 / PostgreSQL p50`.
- `p95 overhead ratio = TaskGate p95 / PostgreSQL p95`.
- `QPS ratio = TaskGate QPS / PostgreSQL QPS`; label it as a throughput ratio,
  not latency overhead.
- Result-hash equality, row-count equality, successful sample count, and failed
  request count as acceptance controls.

Ratios must be calculated within the same dataset, scale, workload,
concurrency, cache policy, and fresh-deployment replicate. Do not divide a
pooled TaskGate percentile by a PostgreSQL percentile from another campaign.

### Expected observation (hypothesis, not a result)

**H1:** Full TaskGate will have higher end-to-end latency and lower or equal
throughput than direct PostgreSQL because it performs additional provenance
and atomic accounting work. The magnitude may differ by query shape and scale.
All paired result hashes are expected to match. This statement is a hypothesis
until the table is filled from a completed campaign.

### Step-by-step execution method

1. Implement the missing workload entries and semantically equivalent SQL
   files using the existing workload-manifest schema. Implement a B1 backend
   that invokes the full exposure path; do not rename `resource_taskgate`.
2. Add a full-run configuration that preserves the existing controls: seeded
   baseline order, five warmups, 30 measured runs per worker, distinct fresh
   tasks, dataset/environment digests, and real metrics probes.
3. Validate the extended configuration with the existing validator:

   ```sh
   make eval-validate
   ```

4. Run the existing smoke suite only as an engineering check:

   ```sh
   make eval-smoke
   ```

   Its current query set and resource-only TaskGate path mean its numbers must
   not populate the table below.
5. After the missing workload and full-path backend work is complete, run the
   publication suite on a fresh deployment:

   ```sh
   make eval-full
   ```

6. Repeat the complete campaign on at least three fresh deployments, with a
   unique campaign ID and fresh task pool each time.
7. Generate summaries only from a completed, digest-bound campaign:

   ```sh
   make artifacts
   ```

8. Verify every B0/B1 result hash pair, inspect failed samples, and calculate
   the ratios from matching summary rows. If any required cell is absent or
   uses the resource-only path, keep that result blank.

### Controls, repetitions, and statistics

- Five warmups and at least 30 measured executions per worker, per query, per
  baseline, matching the existing full configurations.
- At least three fresh-deployment replicates.
- Seeded random baseline/cell order, identical cache policy, and isolated
  baseline resources.
- Type-7 p50/p95; retain the deterministic bootstrap interval for p50. Show the
  range across fresh deployments as a robustness check.
- Fail the cell on unstable results, cross-baseline result mismatch, missing
  metrics probes, reused TaskGate roots, or incomplete result drain.

### Artifacts to save

- Exact extended suite configuration and workload manifest.
- Every workload SQL file for B0 and B1.
- `run.json`, `samples.jsonl`, `samples.csv`, `cells.jsonl`, and the sealed
  campaign manifest under `evaluation/raw/<run-id>/`.
- Dataset and environment manifests and their SHA-256 values.
- Metrics-probe executable paths/digests and probe output.
- `evaluation/generated/summary.csv`, `summary.json`,
  `paper-results.json`, `latency-p95.svg`, and `throughput.svg`.
- A machine-readable mapping from the B1 label to the full exposure execution
  path, so it cannot be confused with `resource_taskgate`.

### Blank result table

Duplicate this table for each dataset scale and fresh-deployment replicate,
then provide a clearly specified cross-replicate summary.

| Workload | PostgreSQL p50 (ms) | PostgreSQL p95 (ms) | PostgreSQL QPS | TaskGate p50 (ms) | TaskGate p95 (ms) | TaskGate QPS | p50 ratio | p95 ratio | QPS ratio |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| SELECT projection/filter | | | | | | | | | |
| JOIN | | | | | | | | | |
| GROUP BY | | | | | | | | | |
| Nested VIEW | | | | | | | | | |

## 2. PostgreSQL RLS comparison with 100 adaptive legal queries

### Motivation

Demonstrate that row authorization and cumulative exposure accounting answer
different questions. PostgreSQL RLS decides whether each statement may access
rows under a policy. TaskGate additionally tracks how much distinct exposure a
root task has accumulated across an adaptive sequence.

### Setup

- Use one immutable TaskGate benchmark snapshot and the same non-owner subject,
  row scope, projected fields, and query semantics in both arms.
- Install a real PostgreSQL RLS policy on the baseline relation. Record
  `relrowsecurity`, the applicable `pg_policies` rows, role membership, and the
  effective session user. A reporting view alone is not RLS.
- Define a versioned trace of exactly 100 individually legal, read-only queries.
  The trace must include pagination, equivalent predicates, repeated
  aggregations, and query choices that depend deterministically on earlier
  returned values.
- Freeze the adaptive policy before running either arm. Given the same prior
  result, it must choose the same next query. Preserve the policy seed and every
  generated query.
- Give TaskGate a predeclared root exposure budget that is below the independent
  oracle's total exposure for the complete trace. Publish the budget-selection
  rule before inspecting the TaskGate stopping position.
- Give both arms sufficient ordinary query/row/time budgets that the exposure
  budget is the intended TaskGate stopping condition.

### Baselines

- **B0 -- PostgreSQL RLS:** all 100 statements run as the restricted non-owner
  role. PostgreSQL supplies authorization but no cross-query exposure ledger.
- **B1 -- TaskGate:** the same logical trace runs under one root task with the
  same approved row scope and a finite release/dependency/outcome budget.
- **Independent exposure oracle:** computes unique FactIDs over the released
  trace for both arms. It is a measurement oracle, not a third access-control
  system.

### Metrics

- Successful queries before the first exposure-budget rejection.
- Total successful queries out of the 100-query trace.
- Cumulative unique release facts, positive-output dependency facts, and query
  outcome facts at every prefix of the trace.
- First rejection index and stable error code.
- Number of results released after the approved exposure budget would have
  been exceeded.
- Per-query authorization denials unrelated to exposure; these must be zero
  for the fixed legal trace or the experiment is invalid.

RLS does not expose a native "unique facts" metric. Derive its curve with the
same independent oracle; do not record the absence of an RLS ledger as zero
exposure.

### Expected observation (hypothesis, not a result)

**H2:** The real RLS policy will authorize the entire trace because every query
is individually legal, while TaskGate will stop releasing results when the
predeclared cumulative exposure budget would be exceeded. TaskGate's committed
exposure is expected never to exceed the approved vector. These are expected
observations, not completed findings.

### Step-by-step execution method

1. Run the exposure oracle/semantic preflight:

   ```sh
   make eval-exposure
   ```

2. Add the missing, versioned RLS DDL and a non-owner test role. Prove through
   catalog introspection that RLS is enabled and that the test role does not
   own or bypass the relation.
3. Add the missing 100-query corpus and deterministic adaptive-policy runner.
   The runner must persist each concrete SQL statement/plan, request ID,
   response code, result hash, and chosen next-query transition.
4. Run the RLS and TaskGate arms in randomized order against the same immutable
   snapshot. Use a fresh RLS session and fresh TaskGate root for each paired
   replicate.
5. Feed both result streams to the independent FactID oracle. Compare exposure
   prefix curves, not only final totals.
6. Verify that every pre-stop query was legal in both arms, that the TaskGate
   rejection was specifically exposure-budget exhaustion, and that no result
   was released for the rejected request.
7. Repeat with at least three fresh TaskGate roots and RLS sessions. Require
   identical trace and exposure digests across deterministic repeats.

There is currently no repository command that performs steps 2--7. In
particular, `make eval-full`'s `native_view` baseline is not RLS. Leave the
table blank until a dedicated runner and RLS DDL are checked in.

### Controls, repetitions, and statistics

- Paired arms, immutable snapshot, identical legal trace, identical subject
  scope, and randomized arm order.
- At least three deterministic paired replicates; preserve any variation
  rather than averaging a stopping index.
- A deliberately unlimited TaskGate exposure-budget control should complete
  the trace and match the RLS result hashes. It is a validity control, not the
  primary comparison.
- A policy-denied negative-control query must be rejected by both arms and must
  not be counted among the 100 legal queries.
- Counts and stopping positions are deterministic protocol outcomes; report
  exact values and replicate agreement rather than a significance test.

### Artifacts to save

- RLS schema/policy DDL and catalog-introspection output.
- Restricted role and session-setting manifest with secrets removed.
- The adaptive policy, seed, 100-query trace, and a SHA-256 of their exact
  bytes.
- Per-query JSONL for both arms, including result and plan digests.
- Independent-oracle per-prefix exposure vectors and set digests.
- TaskGate task manifest, budget vector, receipts, rejection response, and
  before/after root-ledger snapshots with task identifiers hashed.
- Environment and dataset manifests.

### Blank result table

| System | Successful before exhaustion | Successful of 100 | Unique release facts | Unique dependency facts | Unique outcome facts | First rejection index | Exposure controlled |
|---|---:|---:|---:|---:|---:|---:|---|
| PostgreSQL RLS | | | | | | | |
| TaskGate | | | | | | | |

## 3. Adaptive agent attack benchmark

### Motivation

Test whether changing query decomposition, surface syntax, or request identity
can amplify a task's released information without an equivalent increase in
its cumulative exposure. The benchmark is about accounting invariants, not
about claiming that PostgreSQL is an adversarial-defense competitor.

### Setup

- Use one immutable snapshot and a versioned A--E attack corpus.
- Provision an independent fresh TaskGate root for every attack, direction,
  and replicate. Pair any two strategies being compared with equal budgets and
  fresh roots so the first strategy does not prime the second strategy's
  history.
- Use an authorization-only PostgreSQL arm to confirm every accepted statement
  is legal and to produce canonical result hashes.
- Preserve actual/charged vectors and release, dependency, outcome, plan, and
  observation digests for every request.
- Use deterministic `ORDER BY` before every page operation.

### Baselines

- **B0 -- authorization-only PostgreSQL:** executes legal query variants but
  has no cross-query deduplication or task exposure budget.
- **B1 -- TaskGate:** executes the same logical attacks with root-scoped FactID
  history and budgets.
- **Independent effect oracle:** forms exact set unions/differences over each
  strategy and checks exposure conservation.

### Metrics

For every attack, record accepted/rejected requests; stable error codes;
result, plan, observation, and R/I/O set digests; actual and newly charged
R/I/O vectors; final root-ledger vectors; duplicate charge; and any released
result after budget rejection. The attack-specific measurements below refine
this common schema. Exact set equality is required where two strategies claim
the same effect; equal cardinality alone is insufficient.

### Attack A -- pagination and page replay

Use a complete ordered query and compare it with `LIMIT 100 OFFSET 0`,
`LIMIT 100 OFFSET 100`, subsequent disjoint pages, at least one overlapping
page, and a replayed page under a new request ID.

Measure the union of page FactIDs versus the full-query FactIDs; actual and
newly charged facts per page; duplicate charge for an overlapping/replayed
page; final ledger vector; and ordered result coverage.

**H3-A:** Disjoint pages will cumulatively charge their union, while facts in
overlapping or replayed pages will be charged only on first exposure. The final
unique effect is expected to match the equivalent complete result.

### Attack B -- equivalent SQL replay

Instantiate equivalent predicates over an approved field, including
`WHERE a = 1`, `WHERE 1 = a`, and `WHERE a IN (1)`, plus formatting, alias, and
conjunct-order variants. All variants must be within the advertised SQL profile
or be classified separately as fail-closed policy rejections.

Measure acceptance status; canonical QueryPlan/normal-form digest equality;
result hash equality; exposure-set digest equality; and incremental charge
after the first accepted variant.

**H3-B:** Accepted equivalent variants are expected to produce the same
canonical plan/effect identity and zero incremental charge after the first
variant. An unsupported variant's rejection is not evidence of canonical
equality and must be reported separately.

### Attack C -- retry and request-ID variation

Issue the exact same query first with one request ID, then with a different
request ID. As a transport-idempotency control, also retry once with the
original request ID.

Measure query ID, `idempotent_replay`, `semantic_replay`, connector execution
count, plan/observation/result digests, and incremental exposure.

**H3-C:** Reusing the same request ID is expected to replay the same terminal
query record without re-execution. A different request ID is a semantic replay,
not request-ID idempotency; it may create a new query record but is expected to
charge zero new exposure for the same observation.

### Attack D -- UNION splitting

Compare a monolithic query with a set of predicate-partitioned queries whose
result union is identical. Also include the supported advanced UNION DISTINCT
plan as a semantic control where applicable. Ordinary SQL UNION rejection on
the public SQL profile must not be misreported as exposure conservation.

Measure the exact result union; release/dependency/outcome set union; sum of
per-request gross facts; total unique charged facts; final ledger digest; and
any overlap or gap between partitions.

**H3-D:** Splitting a logical result across many queries is expected not to
reduce the final unique exposure charge. Overlap is expected to deduplicate,
and a UNION operand reordering is expected to preserve the accounting effect.

### Attack E -- aggregation probing

Issue a sequence of distinct `COUNT(condition)` questions, including
thresholds that return the same visible count, then replay one question and
issue an equivalent rewrite of it. Keep the underlying release/dependency set
constant where the fixture permits.

Measure canonical plan digests; visible results; release/dependency novelty;
outcome FactIDs and novelty; replay charge; and the request index at which an
outcome budget rejects a further probe.

**H3-E:** Distinct canonical aggregate questions are expected to create
distinct query-outcome facts even when their visible counts or positive-output
dependency sets coincide. Exact or equivalent replays are expected to add no
new outcome fact.

### Expected observation (hypothesis, not a result)

**H3:** Across attacks A--E, query decomposition, equivalent syntax, and
request-ID changes are expected not to reduce the cumulative charge for the
same unique effect. Repeated exposure is expected to deduplicate, while a new
canonical aggregate question is expected to consume new outcome budget. The
case-specific H3-A through H3-E statements above are hypotheses; none is a
reported result in this guide.

### Step-by-step execution method

1. Run the existing deterministic and PostgreSQL-backed semantic campaign:

   ```sh
   make eval-exposure
   ```

   It currently checks split/merge, overlapping pagination, retry, rewrite and
   UNION invariance, and aggregation outcome probing. Treat it as semantic and
   integration evidence, not as the requested live adaptive campaign.
2. Verify the current evidence bindings in
   `evaluation/exposure/results.json`,
   `evaluation/exposure/rq3-integration.json`, and
   `evaluation/exposure/raw/rq3-postgres-go-test.jsonl`.
3. Add the missing versioned A--E live corpus and runner. It must use fresh
   roots, drive the full TaskGate execution path, run the authorization-only
   control, and emit one raw record per request.
4. For attacks A and D, run both directions: complete-then-decomposed on
   independent roots and decomposed-then-complete on independent roots. Compare
   final set unions rather than sums of gross per-query counts.
5. For attack B, distinguish accepted canonical equivalents from unsupported
   SQL-profile rejections.
6. For attack C, execute both the same-request-ID control and the required
   different-request-ID semantic replay.
7. For attack E, predeclare the threshold sequence and outcome budget; do not
   select thresholds after observing where rejection occurs.
8. Repeat every attack on at least three fresh roots and require deterministic
   set/result digests. Fail a case on a digest mismatch, partial result release,
   or ledger overspend.

No checked-in command currently executes step 3 onward as one live A--E
campaign. `make eval-exposure-performance` exercises a real full path, but its
fixed history-ramp workload is not a substitute for this attack matrix.

### Controls, repetitions, and statistics

- At least three fresh-root replicates per attack and strategy direction.
- Exact-set equality is the primary statistic; do not replace it with equal
  cardinalities.
- Preserve gross facts, unique facts, and incremental charge separately.
- Randomize variant order where order is not part of the attack, and run the
  reverse order to expose first-query bias.
- Include negative controls for out-of-profile SQL and verify they fail before
  execution and exposure settlement.
- Counts are deterministic; report exact replicate agreement. Latency, if
  collected, is descriptive and secondary to the invariants.

### Artifacts to save

- Versioned A--E corpus, adaptive driver seed, and exact query/plan bytes.
- Per-request baseline and TaskGate JSONL.
- Result, plan, observation, and R/I/O set digests.
- Actual and charged R/I/O vectors and root-ledger snapshots at every step.
- Receipts and structured rejection responses.
- Independent-oracle union/difference report for A and D.
- Existing `evaluation/exposure` report, integration log, and source/corpus
  digests used as preflight evidence.

### Blank result table

| Attack | PostgreSQL behavior | TaskGate accepted requests | Plan/effect equality | Duplicate charge | Final unique exposure | Budget rejection | Accounting violation |
|---|---|---:|---|---:|---:|---|---:|
| A. Pagination/replay | | | | | | | |
| B. Equivalent SQL | | | | | | | |
| C. Retry/request ID | | | | | | | |
| D. UNION splitting | | | | | | | |
| E. Aggregation probing | | | | | | | |

## 4. Non-competitive ProvSQL comparison

### Motivation

Clarify the boundary between single-query provenance generation and
task-scoped cumulative exposure accounting. This is not a global performance
competition: ProvSQL constructs a general provenance circuit, while TaskGate
constructs typed, budget-oriented exposure sets and maintains task history.

### Setup

Use the existing external baseline under
`evaluation/provenance-baseline/`. It pins PostgreSQL 16 and ProvSQL 1.11.0,
loads the same deterministic Orders--Lineitem-shaped fixture, verifies the
complete canonical data stream, randomizes paired order, uses a warm business
data cache, and forces a novel nonce-bound provenance circuit for every timed
query.

The common timing boundary is:

```text
ordered SQL result production
    + complete client drain
    + provenance representation generation
```

Semiring/probability/Shapley evaluation is outside that boundary. TaskGate
history lookup, budget enforcement, settlement, receipt, and release must not
be silently removed to make it resemble ProvSQL; those additional semantics
must instead be labeled when the TaskGate arm is added.

### Baselines

- **B0 -- direct PostgreSQL:** identical SQL and result drain, without
  provenance representation.
- **B1 -- ProvSQL:** the existing external provenance-generation arm.
- **B2 -- TaskGate:** a required paired arm using the same fixture, tracked
  nonce dependency, query semantics, host, cache policy, run count, and result
  drain boundary. Replay must be disabled for the generation comparison and
  evaluated separately for multi-query capability.

B2 does not currently exist. Values from `evaluation/exposure-performance`
or `evaluation/v4-acceptance` use different fixtures and boundaries and must
not be pasted beside the ProvSQL values.

### Metrics

1. Latency p50 and p95 for B0, B1, and the future paired B2; report each
   provenance arm's ratio versus its matched direct-PostgreSQL run.
2. Persistent representation growth per query and over the complete trace.
   For ProvSQL, retain gate delta and circuit mmap byte delta. For TaskGate,
   retain FactSet/bitmap, root-ledger, observation, receipt, and index growth.
   Label the represented semantics beside the bytes; they are not equivalent
   objects.
3. Multi-query capability checks:

   - identifies the provenance/exposure effect of one query;
   - unions and deduplicates effects across a task root;
   - reports incremental novelty on replay;
   - enforces a cumulative exposure budget before result release.

4. Ordered visible-result equivalence, provenance-representation completeness,
   sample completeness, and pinned-version/source-digest gates.

Use `N/A`, not numeric zero, for a semantic operation a system does not offer.

### Expected observation (hypothesis, not a result)

**H4:** Both provenance-generating arms are expected to incur additional work
and persistent representation growth relative to direct PostgreSQL. ProvSQL is
expected to explain per-query result origin; TaskGate is expected additionally
to deduplicate exposure over a task history and enforce a cumulative budget.
There is deliberately no directional hypothesis that TaskGate is faster,
slower, smaller, or better than ProvSQL on globally unequal semantics.

### Step-by-step execution method

1. Validate the checked-in smoke configuration without a database:

   ```sh
   docker run --rm -v "$PWD:/src" -w /src \
     golang@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 \
     go run ./evaluation/cmd/provenance-baseline \
     -config evaluation/provenance-baseline/config.smoke.json -validate-only
   ```

2. Run the real smoke campaign only to validate the environment:

   ```sh
   make eval-provenance-baseline
   ```

   Do not publish its one-scale, three-measurement output.
3. Copy the full configuration template, assign a unique campaign ID, and run
   it on a fresh Compose project/volumes:

   ```sh
   cp evaluation/provenance-baseline/config.full.example.json /tmp/provsql-full.json
   # Edit campaign_id in /tmp/provsql-full.json.
   PROVENANCE_BASELINE_CONFIG=/tmp/provsql-full.json \
   PROVENANCE_BASELINE_RUN_ID="paper-$(date -u +%Y%m%dt%H%M%Sz)" \
     ./evaluation/provenance-baseline/run.sh
   ```

4. Repeat the full campaign on at least three fresh database volumes, preserving
   each run-specific environment manifest and raw directory.
5. Inspect every gate, result hash pair, sample, gate delta, and byte delta in
   each run's `config.json`, `report.json`, and `results.json`.
6. Implement the missing B2 paired TaskGate arm. It must include the same novel
   nonce dependency and measurement boundary. Run it on the same host and with
   the same workload order, warmups, 30 measured executions, and three fresh
   deployments.
7. Run a separate multi-query trace for the capability checks. Do not include
   its history-hit latency in the novel representation-generation comparison.
8. Only after B2 passes the fairness gates, derive matched ratios and fill the
   tables. Without B2, publish the ProvSQL/direct external baseline separately
   and leave every TaskGate numeric cell blank.

### Controls, repetitions, and statistics

- The full template uses five warmups and 30 measured executions per workload
  and system; use the same values for B2.
- At least three fresh-volume deployments for every arm.
- Byte-identical workload SQL and paired randomized system order where the
  system interfaces permit it.
- Warm business data with a new provenance nonce/circuit/effect per measured
  operation.
- Type-7 p50/p95, per-deployment summaries, and matched ratios versus direct.
- Fail on a result mismatch, repeated nonce representation, absent provenance
  carrier, unpinned version, reused volume, or incomplete sample set.

### Artifacts to save

- Per-run `config.json`, `report.json`, and `results.json` under
  `evaluation/provenance-baseline/raw/<run-id>/`.
- Direct and ProvSQL image IDs, pinned ProvSQL revision, PostgreSQL settings,
  cgroup lifetime peaks, and host environment manifest.
- Every raw paired sample, visible-result hash, representation hash, gate
  before/after count, and artifact-byte before/after count.
- For B2, exact TaskGate query/plan, nonce binding, component timings, FactSet
  and ledger sizes, receipts, and source/config digests.
- A capability-test trace and pass/fail evidence independent of latency data.

### Blank latency and storage table

Repeat rows for each configured scale.

| Workload | System | p50 (ms) | p95 (ms) | Ratio vs matched direct | Persistent representation delta (bytes/query) | Representation semantics |
|---|---|---:|---:|---:|---:|---|
| Join + GROUP BY | Direct PostgreSQL | | | | | |
| Join + GROUP BY | ProvSQL | | | | | |
| Join + GROUP BY | TaskGate | | | | | |

### Blank multi-query capability table

| System | Single-query lineage/effect | Cross-query union/dedup | Replay novelty | Cumulative budget | Pre-release enforcement |
|---|---|---|---|---|---|
| Direct PostgreSQL | | | | | |
| ProvSQL | | | | | |
| TaskGate | | | | | |

## 5. Nested View DAG and join-graph scalability

### Motivation

Evaluate the Phase B View compiler at its supported complexity boundary and
show that transparent nesting and equivalent join-graph rewrites preserve the
compiled semantics. The experiment isolates compiler cost; it is not a query
execution benchmark.

### Setup

Run two independent sweeps:

1. **View-depth sweep:** transparent chains at depths 1, 2, 4, 8, and 16 over
   a fixed terminal product and fixed output interface.
2. **Join-graph sweep:** connected INNER equi-join graphs with 2, 4, 8, and 16
   terminal products, fixed chain topology for the primary scaling curve, and
   fixed output cardinality/selectivity.

The implementation limits are 16 View levels and 16 expanded join sources.
Add depth-17 and source-17 negative controls to prove the limit is enforced;
do not include rejected controls as performance points.

Use deterministic synthetic `RegistrySnapshot` and `queryplan.Product`
fixtures for compiler-only timing. Separately execute a small frozen
PostgreSQL fixture to compare the expanded plan with a direct reference query.
Record exact registry, definition, product, and fixture digests.

### Baselines

- **B0 -- direct semantic form:** the equivalent unnested View definition or
  canonical join plan.
- **B1 -- nested/graph form:** the requested depth or join-source count.
- For correctness metamorphisms, compile alias-renamed, join-order, and
  parenthesization variants of B1 and compare canonical artifacts.

### Metrics and timing boundaries

- **Total compilation time:** wall time from entry to
  `viewcompiler.Compiler.Compile` through a complete returned `Artifact`,
  excluding registry discovery and PostgreSQL query execution.
- **Rewrite/expansion time:** recursive View parse/validation, dependency
  expansion, join-graph canonicalization, and materialization of the canonical
  `QueryPlan`, measured as a non-overlapping internal stage.
- **Digest generation time:** canonical plan, interface, dependency, and
  binding digest construction over already prepared stage inputs, measured as
  a non-overlapping internal stage.
- Artifact sizes: reachable nodes/edges, expanded sources, definition bytes,
  and canonical plan bytes. These are explanatory variables, not substitutes
  for time.
- Correctness:

  - compile success at every supported point;
  - exact result multiset equality with the direct PostgreSQL reference;
  - canonical plan digest equality for transparent direct/nested forms;
  - canonical plan/effect equality under alias, join order, and
    parenthesization variants;
  - deterministic artifact equality across repeated compilation;
  - expected structured rejection at depth/source count 17.

Total compilation includes rewrite and digest work. Do not add the component
times to total unless the new instrumentation proves that the stage intervals
are exhaustive and non-overlapping.

### Expected observation (hypothesis, not a result)

**H5:** Compilation, rewrite, and digest cost are expected to increase with
reachable View depth and join-graph size while remaining bounded at the
supported 16-level/16-source ceiling. Transparent wrappers and equivalent join
representations are expected to preserve canonical plan/effect digests and
PostgreSQL results. No measured complexity claim is made until the timing
harness exists.

### Step-by-step execution method

1. Run the implemented correctness/property tests:

   ```sh
   go test -count=1 ./internal/viewcompiler ./internal/sqllowering ./internal/queryplan
   ```

   These tests already cover transparent nesting, 16-versus-17 guards,
   16-source joins, deterministic compilation, alias/join-order invariance,
   and digest sensitivity. They do not emit the requested timing matrix.
2. Run the existing exposure suite as a separate normalizer sanity check:

   ```sh
   make eval-exposure
   ```

   Its `normalizer_depth` curve measures algebra normalization at a different
   set of depths. It must not be relabeled as View compilation, View rewrite,
   or digest time.
3. Add a dedicated benchmark driver that generates the two exact matrices and
   emits raw per-iteration JSON. Add explicit stage timers rather than timing
   `view-contract` externally; external timing would conflate live registry
   discovery, connection setup, rewrite, digesting, JSON encoding, and stdout.
4. For every cell, construct the fixture once, warm the compiler code path,
   then collect at least 100 measured compiles. Start each measured compile
   from the same immutable registry/product inputs and retain allocation/GC
   counters if available.
5. Run at least five fresh processes per cell, randomizing cell order with a
   recorded seed. Pin `GOMAXPROCS`, Go version, CPU allocation, and power policy.
6. For the depth sweep, hold the terminal product, output fields, filters, and
   join-source count constant. Change only wrapper depth.
7. For the join sweep, hold depth, topology, projected interface, predicate
   count per edge, and fixture result cardinality constant where possible.
   Change only source count.
8. Execute the direct and compiled SQL on the frozen PostgreSQL fixture and
   compare canonical result multisets. Compare canonical artifact/effect
   digests for every metamorphic variant.
9. Run the depth-17 and source-17 controls once per process and require the
   documented structured limit errors; exclude their rejection latency from
   scaling summaries.

There is no checked-in timing driver or Make target for steps 3--9. Do not
invent a command in the paper or fill the tables from unit-test wall time.

### Controls, repetitions, and statistics

- At least 100 measured operations per cell and five fresh-process replicates,
  after untimed warmup.
- Seeded randomized cell order and fixed CPU/Go/runtime settings.
- Report type-7 p50 and p95 for every timer; retain raw observations and
  process-level summaries.
- Use identical fixture bytes for direct/nested comparisons. Fail on any result
  or required digest mismatch.
- Include depth/source 17 only as fail-closed boundary controls.

### Artifacts to save

- Benchmark source, exact generated registries/products, and fixture digests.
- Raw JSONL with run/process/cell/iteration, total compile, rewrite, digest,
  allocations, and correctness fields.
- Per-cell environment/runtime configuration and randomization seed.
- Every returned `Artifact` digest and a canonical artifact-size summary.
- PostgreSQL direct/compiled SQL, result hashes, and row counts.
- Unit/property-test JSON output and Go test logs.

### Blank View-depth result table

| View depth | Total compile p50 (ms) | Total compile p95 (ms) | Rewrite p50 (ms) | Rewrite p95 (ms) | Digest p50 (ms) | Digest p95 (ms) | Result correct | Digest invariant |
|---:|---:|---:|---:|---:|---:|---:|---|---|
| 1 | | | | | | | | |
| 2 | | | | | | | | |
| 4 | | | | | | | | |
| 8 | | | | | | | | |
| 16 | | | | | | | | |

### Blank join-graph result table

| Joined tables | Total compile p50 (ms) | Total compile p95 (ms) | Rewrite p50 (ms) | Rewrite p95 (ms) | Digest p50 (ms) | Digest p95 (ms) | Result correct | Digest invariant |
|---:|---:|---:|---:|---:|---:|---:|---|---|
| 2 | | | | | | | | |
| 4 | | | | | | | | |
| 8 | | | | | | | | |
| 16 | | | | | | | | |

## 6. Shared-root concurrency isolation

### Motivation

Verify that many agents sharing one root task cannot overspend the approved
three-dimensional exposure budget under concurrent settlement. Separately
measure actual CAS conflicts/retries rather than inferring them from lock
waiters.

### Setup

- Requested concurrency levels: 10, 50, 100, and 500 simultaneous contenders.
- Each measured cell uses a new root task family and enough delegated contender
  tasks for its width.
- Pre-position the root at exact `B-1` in one boundary dimension while keeping
  valid limits for release, positive-output dependency, and outcome.
- Release all contenders through a client start barrier. Each executes the
  same observation that adds one novel unit at the boundary. Exactly one
  settlement should charge the shared novelty; equivalent followers should
  settle as zero novelty.
- After the batch, issue a distinct `B+1` overflow plan and require a fail-closed
  rejection with no result or partial ledger/content commit.
- Provision enough Gateway replicas, client connections, and Control
  PostgreSQL connections that width 500 reaches the service concurrently
  rather than waiting in the client pool. Record actual active/queued counts.

The current V4 runner is a valuable lower-width acceptance test, but it
requires exactly two Gateway URLs and hard-codes widths 1, 4, 8, and 16. It
also states that its transitive root-lock queue does not reveal which epoch a
request read and does not prove a CAS conflict or retry.

### Baselines

- **B0 -- serial control:** the same B-1, B, and B+1 sequence with one
  contender on a fresh root.
- **B1 -- concurrent TaskGate:** widths 10/50/100/500 on fresh roots.
- Retain the current forced root-lock-queue mode as a safety control. Add a
  natural-contention mode without an acceptance-owned blocking lock for CAS
  measurement; report the two modes separately.

### Metrics and definitions

- **CAS attempts:** production root-head compare-and-swap attempts attributed
  to the cell.
- **CAS conflicts:** attempts whose expected root epoch/version did not match.
  Count only an explicit production telemetry event/counter. PostgreSQL lock
  waiters, elapsed time, zero-novelty settlements, or client retries are not a
  CAS-conflict proxy.
- CAS conflict rate: conflicts divided by attempts.
- Successful requests, failed requests, charged winners, and zero-novelty
  settlements.
- Rejected overflow attempts with error code
  `EXPOSURE_BUDGET_EXHAUSTED` and no encrypted result, result chunk,
  materialization, observation reference, root observation, success audit, or
  partial content/head mutation.
- **Budget violation:** any observed committed state with release, dependency,
  or outcome used greater than its approved limit, or any released result for
  a request whose atomic settlement should exceed a limit.
- Initial, B-1, B, and post-rejection root epoch/vector/set digests.
- Descriptive client latency and actual concurrent/queued session counts.

If explicit CAS telemetry is absent, record CAS attempts/conflicts as
`unmeasured`; do not enter zero.

### Expected observation (hypothesis, not a result)

**H6:** At every requested width, the committed exposure vector is expected to
remain within the approved budget, one contender is expected to commit the
shared novel effect, equivalent contenders are expected to add zero novelty,
and the distinct B+1 request is expected to be rejected without partial
release. Actual CAS conflict counts may increase with natural contention, but
the safety invariant is expected to be independent of the conflict count.

### Step-by-step execution method

1. Validate the implemented lower-width template without contacting services:

   ```sh
   go run ./evaluation/cmd/v4-concurrency \
     -config evaluation/v4-concurrency/template.json -validate-only
   ```

2. Run the existing 1/4/8/16 acceptance campaign as an engineering preflight,
   following `evaluation/v4-concurrency/README.md`:

   ```sh
   docker compose -p taskgate-v4-concurrency \
     -f compose.yaml -f evaluation/v4-concurrency/compose.yaml up -d --build

   export V4_CONCURRENCY_CONTROL_DSN="postgres://gateway_control:${CONTROL_DB_PASSWORD}@127.0.0.1:${CONTROL_POSTGRES_PORT:-25433}/${CONTROL_POSTGRES_DB:-taskbound_gateway}?sslmode=disable"

   go run ./evaluation/cmd/v4-concurrency \
     -config evaluation/v4-concurrency/template.json \
     -prepare -output /new-private-run/config.json

   go run ./evaluation/cmd/v4-concurrency \
     -config /new-private-run/config.json \
     -output /new-private-run/results.json
   ```

   Do not copy its 1/4/8/16 values into the 10/50/100/500 table, and do not
   label `root_lock_waiters_observed` as CAS conflicts.
3. Extend the runner/config validator for the four requested widths, a
   sufficient dynamic Gateway pool, repeated fresh-root cases, and explicit
   CAS-attempt/conflict telemetry. Keep the current source/config digest
   binding and fail-closed evidence gates.
4. Provision a fresh serial root and fresh concurrent roots through the public
   MCP/OA flow. Hash task identities in results and keep private IDs outside
   version control.
5. Run the forced-queue safety phase: commit B-1, hold the root row using the
   acceptance-owned transaction, start all contenders, prove they reached the
   wait chain, release the blocker, and verify the exact B state plus B+1
   failure atomicity.
6. Run the natural-contention CAS phase on a different fresh root: commit B-1,
   synchronize clients at an application barrier without holding the root row,
   execute the batch, and collect explicit CAS events/counters.
7. Read and persist root vectors, epochs, content counts, and set digests before
   B-1, at B-1, after the concurrent batch, and after rejected overflow.
8. Repeat each width/mode on at least 30 fresh root families. Randomize width
   order across fresh deployments and retain failed/time-out trials as evidence.
9. For any zero observed budget violations, report the number of trials and
   requests; an optional binomial confidence bound must be labeled as a bound,
   not proof of impossibility.

There is no current command that can execute the target widths or produce a
valid CAS-conflict count. The command form in step 2 becomes reusable only
after the validator, deployment capacity, configuration, and telemetry work in
step 3 is implemented.

### Controls, repetitions, and statistics

- Serial B0 on a fresh root for each deployment.
- At least 30 fresh-root trials per width and contention mode.
- Separate forced-queue safety evidence from natural-contention CAS evidence.
- Barrier telemetry must demonstrate that the offered concurrency reached the
  service; otherwise label the cell invalid rather than as width 500.
- Report total counts and per-trial distributions for CAS conflicts and client
  latency. Report exact budget-violation and rejected-overflow counts.
- Fail a cell on a reused root, missing contender, result release after B+1,
  partial commit, unobserved offered concurrency, or any committed over-budget
  vector.

### Artifacts to save

- Exact template and prepared private configuration for every trial, with
  public evidence using hashed task identities.
- V4 concurrency runner source digest, configuration digest, image IDs, and
  environment manifest.
- Per-request JSONL with start/barrier/service timestamps, Gateway replica,
  response/error, actual/charged vectors, root epoch, and result/observation
  digests.
- Explicit CAS attempt/conflict event stream or counter snapshots with the
  attribution method.
- Root-lock queue samples for the forced-queue phase.
- Initial, B-1, B, and post-overflow root/content snapshots.
- Failure audits, receipts, and proof that rejected overflow left no result or
  partial content.
- Trial-level and aggregate summaries; never discard failed trials.

### Blank result table

| Concurrency | Valid trials | CAS attempts | CAS conflicts | Conflict rate | Charged winners | Zero-novelty settlements | Overflow attempts | Rejected overflow | Budget violations |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 10 | | | | | | | | | |
| 50 | | | | | | | | | |
| 100 | | | | | | | | | |
| 500 | | | | | | | | | |

## 7. Publication acceptance checklist

Before transferring any value into the manuscript, verify all of the
following:

- The cell was produced by the target experiment rather than a smoke,
  unit-test, legacy resource-only, or differently scoped campaign.
- Every required workload/width/depth/attack case is present.
- Baseline and TaskGate result hashes match where equivalence is required.
- Raw samples, configurations, environment, dataset, source, and executable
  digests are preserved.
- Repetitions and cache policy match this guide and are stated beside the table.
- Missing metrics are `unmeasured`/`N/A`, never zero.
- Every expected observation is still labeled as a hypothesis until supported
  by the preserved evidence.
- All numeric cells in this guide remain blank until the author runs and
  validates the corresponding experiment.
