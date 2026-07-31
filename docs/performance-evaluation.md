# Performance evaluation

This document defines a scale and overhead methodology for TaskGate. It does
not report new measurements. The required cardinality sweep is:

```text
10K, 100K, 1M, 10M, and 100M facts
```

Three boundaries are measured separately: in-memory bitmap operations, durable
root-ledger settlement in Control PostgreSQL, and complete query execution for
PostgreSQL versus PostgreSQL plus TaskGate. A timing from one boundary must not
populate another boundary's table.

## Measurement rules

For every publication run:

1. Pin a Git commit and record `git status --short`, host CPU/memory/storage,
   kernel, Go, Docker, PostgreSQL, container images, database parameters,
   Catalog/publication/dataset digests, and cache policy.
2. Validate exact set cardinality and content digest before accepting timing
   data. Fail the cell on a result, effect, overlap, or replay mismatch.
3. Run at least three fresh deployments. Complete warmups before measurement,
   randomize paired arm order with a recorded seed, and retain raw samples for
   each deployment rather than pooling away deployment variation.
4. Report client wall-clock latency through the declared completion boundary
   and throughput as successful completed operations divided by the measured
   cell interval. Keep failures in the denominator/accounting record and report
   them separately.
5. Use Hyndman--Fan type-7 p50 and p95 and report sample count. Do not report
   p99 without at least 10,000 samples. Preserve memory, CPU, WAL, network, and
   allocation measurements as supporting metrics.
6. Do not extrapolate a missing 10M or 100M cell from a smaller point. A timed-
   out, OOM, or unavailable scale is `unmeasured` or `failed`, with the reason
   and resource ceiling retained.

The primary scale `N` is an exact FactID-set cardinality, not a row count,
serialized byte count, SQL result count, or sum across repetitions.

## P1 -- bitmap operation scaling

### Inputs

For each `N`, construct immutable `candidate` and `history` bitmap sets at
overlap targets `0%`, `50%`, `90%`, and `100%`. Cover three ordinal
distributions:

- **dense:** contiguous ordinal ranges;
- **clustered:** deterministic runs separated across high-16 containers; and
- **random sparse:** unique pseudorandom ordinals generated from a recorded
  seed.

Run both a one-segment case and a recorded multi-segment/dictionary case. A
segment ordinal is 32-bit in the current representation, so a 100M fixture may
span several valid segments; never wrap or silently reuse an ordinal. Preserve
portable-container and complete-set digests so the exact operands can be
reconstructed.

Exclude fixture generation and artifact loading from the operation timer, but
report them separately. Activate and verify inputs before warmup. Use a fresh
process for each distribution/scale replicate or otherwise prove that heap and
GC state are controlled.

### Operations

Measure these public semantics directly:

```text
ANDNOT/cardinality: candidate.Difference(history).Cardinality()
union:              history.Union(candidate)
cardinality:        candidate.Cardinality()
```

`BitmapSet.Difference` and `BitmapSet.Union` do not mutate their operands and
currently clone affected bitmaps. The timed value therefore includes those
copy/allocation costs; do not describe it as an in-place Roaring primitive.
Prevent dead-code elimination by consuming the result cardinality and digest
outside the timed region.

For each operation record latency, operations per second, allocated bytes,
allocations, peak RSS/cgroup memory, input/output cardinality, segment and
container counts, serialized operand/result bytes, and exact output digest.
The correctness oracle computes ordinary set difference/union at smaller audit
points and validates algebraic identities/digests at all points.

### Blank bitmap table

Duplicate the table for every distribution, segment layout, overlap, and fresh
deployment.

| Facts `N` | ANDNOT + cardinality p50 (ms) | ANDNOT p95 (ms) | Union p50 (ms) | Union p95 (ms) | Cardinality p50 (ms) | Ops/s | Peak memory | Correct digest |
|---:|---:|---:|---:|---:|---:|---:|---:|---|
| 10K | | | | | | | | |
| 100K | | | | | | | | |
| 1M | | | | | | | | |
| 10M | | | | | | | | |
| 100M | | | | | | | | |

## P2 -- durable ledger latency and throughput

### Boundary

Measure the production V4 Control PostgreSQL settlement path from transaction
begin through commit. It must include validation/normalization of the supplied
R/D/O effect, immutable content/manifests as applicable, exact history
`ANDNOT`, budget check, set union, one three-dimensional root-head epoch CAS,
reservation/observation/audit/receipt-related control state required by the
selected finalization API, and commit. It excludes Business PostgreSQL query
execution and result encoding unless the API cannot separate them; any broader
boundary must be labeled explicitly.

Keep Enforcement-Layer component timers, client wall time, PostgreSQL statement
time, WAL, transaction count, and storage growth as distinct fields. A lock
wait duration is not pure PostgreSQL lock time, and a waiter is not a CAS
conflict.

### Cells

For each exact scale `N`:

- prepopulate a fresh root history to each overlap target;
- settle one `N`-fact candidate in the target dimension while publishing the
  full `(R,D,O)` vector and keeping the other dimensions at recorded fixed
  cardinalities;
- run a distinct-request semantic replay of the identical committed
  observation and require zero charge and no bitmap-content growth;
- attempt one fact beyond a predeclared budget and require atomic rejection;
  and
- run throughput at concurrency `1,4,8,16` on both distinct roots (capacity)
  and one shared root (contention), with enough connections to reach the
  service.

Use a new root for every novel measured sample unless the cell explicitly
studies ledger growth. Do not carry a 10K history into a nominally fresh 100K
replicate. For shared-root throughput, verify the final exact union and record
explicit CAS telemetry or mark it `unmeasured`.

### Metrics

- settlement client latency p50/p95 and committed settlements per second;
- novel and replay charged facts and root epoch movement;
- immutable content/manifests created, logical ledger bytes, physical
  PostgreSQL table/index bytes, and WAL bytes;
- Control PostgreSQL CPU and peak memory;
- CAS attempts/conflicts/retries when explicit telemetry exists;
- rejected overflow count and any partial-state or result-release violation;
  and
- final set cardinalities and digests versus the independent oracle.

### Blank ledger table

| Facts `N` | Overlap | Novel p50 (ms) | Novel p95 (ms) | Replay p50 (ms) | Committed settlements/s | WAL bytes | Ledger growth bytes | Peak memory | Correct/atomic |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| 10K | | | | | | | | | |
| 100K | | | | | | | | | |
| 1M | | | | | | | | | |
| 10M | | | | | | | | | |
| 100M | | | | | | | | | |

## P3 -- PostgreSQL versus PostgreSQL plus TaskGate

### Paired arms

- **PostgreSQL:** persistent read-only PostgreSQL connection, identical
  immutable data, complete result drain, and no TaskGate process in the request
  path.
- **PostgreSQL + TaskGate:** the full exposure-enabled path, including
  authorization, visible/provenance execution in a repeatable-read snapshot,
  fact derivation, root-history difference, budget check, atomic settlement,
  encrypted artifact/receipt handling, and result release/drain through the
  declared client interface.

`resource_taskgate` in the generic four-path runner is not the second arm. An
AST-only gateway and a native reporting view are useful diagnostic ablations,
but neither substitutes for full TaskGate.

### Workloads and scales

At each `N`, choose or generate a supported controlled analytical workload
whose independent oracle proves exactly `N` positive-output Dependency facts.
Report the complete `(Release,Dependency,Outcome)` vector; keep Release and
Outcome shape fixed where possible so that `N` isolates dependency scaling.
Include Scan, Join--Group, Union, and Page shapes where the current controlled
execution interface supports them. The agent-facing
`taskgate-reporting-sql-v1` arm contains only shapes that losslessly lower from
that SQL profile; SQL set operations are rejected. Evaluate Union only through
the advanced `execute_plan` harness and label that row `execute_plan-only`
rather than implying agent-facing SQL support. Use deterministic order for Page
and identical canonical result multisets in both arms.

For each workload run:

- a **novel** TaskGate query on a fresh root;
- a **history-overlap** query at `50%`, `90%`, and `100%` where calibrated;
- a **distinct-request semantic replay**, reported separately because it may
  avoid Business/provenance SQL; and
- the matching direct PostgreSQL query with the same result-drain and cache
  policy.

The direct arm cannot have a TaskGate FactID scale by itself. The `N` label is
the oracle-derived effect of the paired logical workload.

### End-to-end metrics

- latency p50/p95 and fully drained QPS for both arms;
- `p50 overhead ratio = TaskGate p50 / PostgreSQL p50`;
- `p95 overhead ratio = TaskGate p95 / PostgreSQL p95`;
- `QPS ratio = TaskGate QPS / PostgreSQL QPS`, labeled as a throughput ratio;
- result hash/row-count equality and successful/failed sample counts;
- TaskGate Enforcement Layer, Business PostgreSQL, Control PostgreSQL, and object-store CPU,
  memory, network, WAL, and storage; and
- TaskGate component timings for authorization, visible/provenance SQL,
  ordinal/bitmap derivation, settlement, result encoding/encryption, and
  receipt signing.

Calculate ratios only within the same deployment replicate, scale, workload,
concurrency, cache policy, and measured sample set. Do not divide a pooled
TaskGate percentile by a PostgreSQL percentile from another campaign.

### Blank end-to-end table

Duplicate per query shape, overlap class, concurrency, and fresh deployment.

| Facts `N` | PostgreSQL p50 (ms) | PostgreSQL p95 (ms) | PostgreSQL QPS | TaskGate novel p50 (ms) | TaskGate novel p95 (ms) | TaskGate QPS | p50 ratio | p95 ratio | QPS ratio | Results equal |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| 10K | | | | | | | | | | |
| 100K | | | | | | | | | | |
| 1M | | | | | | | | | | |
| 10M | | | | | | | | | | |
| 100M | | | | | | | | | | |

## Repetitions and statistics

Use five warmups and at least 30 measured operations per latency cell when the
resource envelope permits, matching the repository's existing full-run
convention. For very large cells, predeclare a smaller sample count from a
pilot before inspecting results, retain every sample, and state the reduced
precision. Use at least three fresh deployments at every published point.

For throughput, use a fixed measurement interval long enough to pass startup
transients and report completed, failed, and offered operations. Randomize
paired PostgreSQL/TaskGate order. A warm-cache claim requires completed
warmups; a cold-cache claim requires a recorded reset command. "First request"
must not be relabeled a controlled cold-cache measurement.

## Current repository coverage

The repository provides several relevant but non-interchangeable tracks:

- `evaluation/exposure-performance/` measures a real full exposure path and
  ablations on a small warm local fixture. Validate its checked evidence with:

  ```sh
  python3 evaluation/exposure-performance/summarize_campaign.py --check
  python3 evaluation/exposure-performance/analyze_paths.py
  ```

- `evaluation/v4-acceptance/` pairs direct PostgreSQL with the full V4
  advanced-`execute_plan` evaluation path and checks calibrated overlap/query
  shapes. The checked maximum point has 1,035,000 Dependency facts; it does not
  supply the required 10M or 100M cells. Its full command and observer contract
  are in
  [evaluation/v4-acceptance/README.md](../evaluation/v4-acceptance/README.md).
  Validate only the example schema with:

  ```sh
  go run ./evaluation/cmd/v4-acceptance \
    -config evaluation/v4-acceptance/config.example.json -validate-only
  ```

- `make eval-exposure-scale` is a PostgreSQL 16 Join--Group scale campaign whose
  checked legacy path reaches 1,035,000 dependency facts. It is not a V4 bitmap
  sweep and cannot fill the 10M/100M rows.
- `evaluation/v4-kernel/results.json` is explicitly engineering-only warm-HOT
  derivation evidence. It excludes Business SQL, authorization, Control
  PostgreSQL settlement, receipts, replay, and queueing.
- `make eval-exposure-storage` measures the earlier Control PostgreSQL
  row-oriented fact-store path only through 10,000 facts per ledger. It cannot
  be extrapolated to the V4 100M ledger point.
- `make eval-exposure-performance` runs a useful engineering smoke, but its
  default values are not a publication-scale campaign.

No checked-in runner currently executes the exact `10K..100M` bitmap,
settlement, and paired end-to-end matrix in this document. Existing
source-controlled results may be cited within their stated boundaries, but
they must not be relabeled or used to fill missing scale cells.

## Evidence to preserve

- generated operand/workload definitions, seeds, exact cardinalities, overlap
  proofs, container/segment counts, and set/result digests;
- raw per-operation samples rather than summaries alone;
- exact timing-boundary definitions and component-timer equations;
- environment, dataset, Catalog, publication, image, configuration, and source
  manifests with SHA-256 bindings;
- PostgreSQL settings, query plans, WAL/storage snapshots, and observer output;
- result and FactID oracle equality reports, replay-zero-growth evidence, and
  overflow atomicity evidence; and
- trial-level summaries, failures, timeouts, and resource-ceiling events for
  every scale, including unmeasured cells.
