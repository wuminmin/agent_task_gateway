# Exposure end-to-end performance and ablation

This harness measures the real V2 exposure path instead of relabeling the
resource-only `query_sql` benchmark. Full-path observations enter through the
public MCP `execute_plan` tool and execute authorization, reservation, a
repeatable-read visible/provenance pair, V2 derivation, immutable FactID
deduplication, shared-root dual-ledger settlement, result encryption, terminal
audit, and receipt signing.

Run the isolated local smoke campaign with:

```sh
make eval-exposure-performance
```

The command creates a dedicated Compose project and volumes, bootstraps one
root task plus delegated worker tasks through the public MCP and OA flows,
measures the campaign, and writes ignored raw evidence to
`evaluation/exposure-performance/raw/<run-id>/`. Set
`EXPOSURE_KEEP_STACK=1` to preserve the isolated deployment. Useful campaign
controls are `EXPOSURE_RUNS`, `EXPOSURE_RAMP_RUNS`, `EXPOSURE_WORKERS`, and
`EXPOSURE_CONCURRENCY` (for example `1,8,32`). The largest concurrency must not
exceed the worker count.

The paper campaign used three independent fresh-stack runs with 200 measured
operations per worker at concurrency `1,4,8` and a 32-query history ramp:

```sh
EXPOSURE_RUN_ID=rq4-20260725-trial1 EXPOSURE_RUNS=200 \
  EXPOSURE_RAMP_RUNS=32 EXPOSURE_WORKERS=8 EXPOSURE_CONCURRENCY=1,4,8 \
  ./evaluation/run-exposure-performance.sh
```

Repeat with `trial2` and `trial3`, then run
`python3 evaluation/exposure-performance/summarize_campaign.py`. The validator
requires exactly 10,432 samples per trial and writes the source-controlled
31,296-observation `results.json`.

## Ablations

Each non-full cell uses the same ordered, one-row Sales expense workload:

1. `business_sql`: one read-only Business PostgreSQL query.
2. `paired_snapshot`: the visible and provenance statements in one read-only
   repeatable-read transaction, without algebra or Control PostgreSQL.
3. `paired_plus_algebra`: the same pair plus V2 `Scan`, predicate support, and
   `Observe` derivation, without persistence.
4. `full_history_ramp`: sequential public `execute_plan` requests cycling over
   four stable entity keys, exposing novel-to-hit history behavior.
5. `full_history_hit`: fixed-plan public requests after the ramp. Concurrent
   workers use distinct delegated tasks sharing the same root exposure ledger,
   so the root row lock is the intended serialization point.

This decomposition makes the incremental cost of paired snapshots, algebra,
and durable enforcement visible. It is not a claim that the first three
ablations have the same security semantics as the full path.

## Metrics and exact definitions

- Latency is client wall-clock time per completed operation. Throughput is the
  completed sample count divided by cell wall-clock seconds. Percentiles use
  Hyndman-Fan type 7.
- `client_peak_heap_bytes` samples Go `HeapAlloc` every 5 ms. The wrapper also
  samples the Gateway, Control PostgreSQL, and Business PostgreSQL container
  memory with Docker stats and merges their peaks into `results.json`.
- Ledger growth is an after-minus-before snapshot of shared-root release and
  influence counters, immutable fact-row count, canonical fact payload bytes,
  and the global `exposure_facts` table/index physical sizes. Physical size is
  page-granular and may legitimately remain flat for a small cell.
- Lock contention samples `pg_stat_activity` every 10 ms by default. It reports
  samples with waiters, maximum waiting sessions, and approximate
  waiter-session milliseconds. The full response also reports
  `exposure_ledger_lock`, the client-side duration of the root-ledger
  `SELECT ... FOR UPDATE`; this includes server/driver round-trip and is not
  mislabeled as pure PostgreSQL lock wait.
- Fact history hit rate is `(actual facts - newly charged facts) / actual
  facts`, combining release and influence families. Query history hit rate is
  the fraction of exposure queries with nonzero actual facts and zero newly
  charged facts.

`samples.jsonl` contains per-operation evidence, `report.json` is the driver
report, `docker-stats.jsonl` is the external memory stream, and `results.json`
is their merged summary. Task IDs stay only in the ignored `tasks.json`; the
summary contains a one-way root-task digest.

The default `1,4` / ten-run settings remain an engineering smoke. The recorded
paper campaign uses more observations, repeated fresh deployments, a committed
environment manifest, and controlled warm-cache policy, but still uses the
bundled ten-row fixture. It must not be presented as TPC, multi-node, or
production evidence.
