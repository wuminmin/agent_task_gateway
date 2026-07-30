# TaskGate V4 acceptance harness

`evaluation/cmd/v4-acceptance` is the executable evidence driver for the
Snapshot-Indexed Hybrid Bitmap Ledger. It calls the public `execute_plan` MCP
tool and a direct read-only PostgreSQL baseline. Every trial uses a distinct
ACTIVE V4 task and, by default, refuses a root whose V4 epoch is already
nonzero.

The driver hashes the direct result and the released Gateway result as a
canonical row multiset and fails the trial when they differ. Multiplicity is
preserved while irrelevant delivery order is removed. Plans using pagination
must still define a deterministic order before `LIMIT/OFFSET`.

The output is one `schema_version=1` JSON document containing raw samples,
Hyndman--Fan type-7 summaries, coverage, provenance digests, and a gate table.
It never inserts a synthetic duration. A missing observer, timer, artifact, or
workload appears as `status: "unmeasured"`; a violated measured threshold
appears as `status: "fail"`. `acceptance` is therefore one of `pass`, `fail`,
or `incomplete`.

For publication runs, configure `environment_manifest` with a path and exact
lowercase SHA-256. The JSON must contain nonempty `host`, `software`,
`database`, and `datasets` objects. Without it the fixed-environment gate is
unmeasured; a digest mismatch is a failure.

## What is measured

- direct SQL client wall time;
- V4 novel and distinct-request semantic replay wall time;
- exact overlap from `(actual - charged) / actual` for Release, Influence,
  Outcome, or all three dimensions;
- Gateway component timings including visible PostgreSQL, provenance
  PostgreSQL, connector overhead, final derivation, and atomic settlement;
- Business and Control PostgreSQL WAL movement using `pg_wal_lsn_diff`;
- all `v4_*` Control PostgreSQL relation sizes and filesystem artifact bytes,
  with explicit page-granular estimates for 1, 10, and 100 roots;
- optional index-build and warm-activation commands, including wall time,
  root-process RSS, and artifact sizes;
- optional external cgroup memory, network, and Business-query counters.

The Gateway reports `ordinal_stream` as companion query-to-drain wall time and
separately accumulates time spent synchronously inside `VisibleResult`,
`Begin`, `Row`, and `Finish` as `bitmap_derivation`. It also reports the three
bitmap leaves and the stream-consumer leaf. The harness checks the two exact
timer equations and rejects missing, zero-placeholder, negative, or incoherent
component evidence. `connector_overhead` excludes the separately reported
`VisibleResult` work, so the leaf decomposition does not count that callback
twice; `ordinal_stream` remains an explicitly overlapping diagnostic aggregate.

The 3/4-second novel and 100/150-millisecond replay SLO gates use only samples
whose committed observation reports exactly 12 Release and 1,035,000 Influence
facts (and one Outcome). Faster small queries cannot dilute that distribution;
without an exact maximum-point workload both latency gates are unmeasured.

The driver still records digest-bound small-query latency evidence but is
sequential and has no like-for-like throughput cell, so the combined
small-query regression gate remains unmeasured. With the current production
driver concurrency, `acceptance` remains incomplete until the throughput cell
and every configured external/offline gate are measured; threshold violations
still make it `fail`.

## Workload contract

Configure cases for all overlap targets `0`, `50`, `90`, and `100`, and all
shapes `scan`, `join_group`, `union`, and `page`. The harness does not assume a
plan has the advertised overlap: it verifies the charged and actual
cardinalities returned by V4. `setup_plans` populate only that trial's fresh
root. The measured `plan` must then be a semantic-cache miss; the harness calls
the identical plan again under a new request ID and requires
`semantic_replay=true`, the same observation digest, and zero R/I/O novelty.

The checked-in `config.example.json` is a schema and plan-shape example. The
join/group and union overlap setup is deliberately labeled `replace-with-...`:
exact 90/100% overlap depends on the published fixture and must be calibrated
from the oracle, not asserted from query text. Replace every task placeholder
and add the required setup plans before running evidence.

The checked-in frozen demo has only ten business rows, so this example cannot
exercise the exact 12-Release/1,035,000-Influence maximum point. It is not a
scale configuration and its maximum-point latency gates must remain
`unmeasured`. A publication run must supply a separately frozen large
publication, calibrated plans and real fresh task IDs. Its deployment must
also keep `GATEWAY_CONNECTOR_MAX_ROWS` at or above the largest provenance row
count (the repository deployment default is 1,200,000); connector truncation
is a fail-closed query failure, never a measured maximum-point sample.

Validate the JSON without credentials or services:

```sh
go run ./evaluation/cmd/v4-acceptance \
  -config evaluation/v4-acceptance/config.example.json -validate-only
```

Run a campaign from the repository root:

```sh
export TASKBOUND_ALICE_TOKEN=...
export V4_EVAL_BUSINESS_DSN='postgres://...'
export V4_EVAL_CONTROL_DSN='postgres://...'

go run ./evaluation/cmd/v4-acceptance \
  -config evaluation/v4-acceptance/config.local.json \
  -output evaluation/v4-acceptance/raw/run-$(date -u +%Y%m%dT%H%M%SZ)/results.json
```

Use `-require-complete` for a publication gate. The output path must not
already exist, so an earlier evidence file cannot be silently overwritten.
The evaluation Dockerfile also exposes target `v4-acceptance`.

## External observer contract

The optional observer is an exact argv array, never a shell string. It is
called immediately before and after each operation with:

- `V4_EVAL_CASE`
- `V4_EVAL_PHASE`
- `V4_EVAL_TRIAL`
- `V4_EVAL_POINT` (`before` or `after`)

It must emit exactly one JSON object:

```json
{
  "schema_version": 1,
  "memory_scope": "cgroup_v2_memory_peak_including_mmap",
  "metrics": {
    "gateway_memory_peak_bytes": 268435456,
    "gateway_network_rx_bytes": 1000000,
    "gateway_network_tx_bytes": 2000000,
    "business_sql_queries_total": 42
  }
}
```

All metrics are nonnegative monotonic counters except that the cgroup peak is
read as the after-snapshot value. Configure the corresponding metric names and
required memory scope. The replay zero-SQL gate remains `unmeasured` when only
Gateway component labels are available; it passes only with an external
Business-query counter whose delta is zero for every replay.
The observer must obtain that counter out of band (for example from an existing
metrics collector); its own snapshot calls must not increment the counter it
is supposed to observe.

Publication storage runs need an isolated Control PostgreSQL instance. The
driver reports the actual number of settled roots in that instance; unrelated
pre-existing roots would make the page-granular runtime-bytes/root estimate a
measurement of the shared deployment rather than this campaign.

## Offline command contract

`index_build` and `activation` use exact argv arrays. `{{run_dir}}` in any
argument or artifact path expands to a newly created, never-reused directory.
The command receives `V4_EVAL_RUN_DIR` and `V4_EVAL_RUN`. RSS is sampled from
the root process's `/proc/<pid>/status`; the 4 GiB gate is usable only when
`single_process: true`. Otherwise child-process memory is unknown and the gate
is correctly marked unmeasured. Activation additionally requires
`warm_verified: true` before its two-second gate can be evaluated.
