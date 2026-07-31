# TaskGate V4 acceptance harness

`evaluation/cmd/v4-acceptance` is the executable evidence driver for the
Snapshot-Indexed Hybrid Bitmap Ledger. It directly calls the advanced
`execute_plan` MCP method, which is retained for deterministic harnesses but is
not advertised in an ordinary Agent's `tools/list`, and a direct read-only
PostgreSQL baseline. Every trial uses a distinct
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

The small-query regression gate consumes two independent benchmark artifacts:
a baseline and a V4 candidate. The configuration binds each artifact by exact
lowercase SHA-256 and records its positive finite P50 latency and throughput.
The gate passes only when candidate latency degradation and candidate
throughput degradation are both at most 10%; a missing artifact is unmeasured,
while malformed evidence or a digest mismatch is a failure. The full
provisioning command injects both references into the generated configuration.

Regenerate the V4 candidate from the same fixed warm workload by adding the
small-regression Catalog overlay to the exposure-performance runner:

```sh
EXPOSURE_PROJECT_NAME="taskgate-v4-small-$(date -u +%Y%m%dT%H%M%SZ)" \
EXPOSURE_RUN_ID="v4-small-$(date -u +%Y%m%dT%H%M%SZ)" \
EXPOSURE_RUNS=200 EXPOSURE_RAMP_RUNS=32 EXPOSURE_WORKERS=8 \
EXPOSURE_CONCURRENCY=1 \
EXPOSURE_COMPOSE_OVERRIDE="$PWD/evaluation/v4-acceptance/compose.small-regression.yaml" \
./evaluation/run-exposure-performance.sh
```

Use that run's `report.json` as the candidate artifact. The full provisioner
requires its complete-history hit cell to have 100% query/fact-history and
semantic-replay hit rates before accepting the reference.

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

## Narrow maximum-point provisioning

The scale-narrow deployment has a deliberately small first gate: one
Join--Group workload, 0% prior overlap, 20 independent root tasks, and a
distinct-request semantic replay after every novel request. The fixed query
joins 45,000 orders to five line items each and returns three groups with three
aggregates. Its required observation is exactly 12 Release, 1,035,000
Influence, and one Outcome fact.

Task preparation uses the same public control flow as a user: every root is
created with `request_data_task`, submitted by Alice, approved by Bob in the OA
demo, and polled until `ACTIVE`. It does not insert tasks into Control
PostgreSQL and does not execute the measured plan. Start the isolated narrow
deployment first, then run:

```sh
export V4_NARROW_RUN_DIR="$PWD/evaluation/v4-acceptance/raw/narrow-$(date -u +%Y%m%dT%H%M%SZ)"
docker compose \
  --file compose.yaml \
  --file evaluation/v4-acceptance/compose.scale-narrow.yaml \
  --profile v4-narrow-tools run --rm v4-narrow-prepare
```

The command writes credential-private `tasks.json` and a ready-to-run
`config.json` under `V4_NARROW_RUN_DIR`; it refuses to overwrite either file.
The generated config is accepted only when the task pool contains exactly 20
unique 45,000-order trials. The plan and direct SQL are pinned together by the
preparation command, and the campaign itself additionally compares their
canonical result multisets. Run the campaign promptly because task expiry is
the human-approved Catalog TTL.

For a non-Compose deployment, put `exposure-bench` and `v4-acceptance` on
`PATH`, set the documented Gateway/OA credentials and URLs, and invoke
`evaluation/v4-acceptance/provision-narrow.sh`. The script only provisions and
generates configuration; the later `v4-acceptance -config ... -output ...`
command remains an explicit separate action.

## Full acceptance campaign

The full matrix uses 140 fresh roots: 20 trials for each of four exact
Influence-overlap points (0/50/90/100%) and 20 trials for each additional
Scan, Page, and Union shape. The 0% Join--Group case is the maximum point used
for the novel and replay SLOs. Provisioning creates and approves the roots but
does not execute a measured query.

Use a new Compose project so both PostgreSQL volumes are isolated. Include the
observer overlay on the first startup: adding `pg_stat_statements` after a
Business volume has already been initialized is unsupported. The environment
manifest must contain nonempty `host`, `software`, `database`, and `datasets`
objects. Baseline and candidate files must be independently produced,
like-for-like small-query benchmark artifacts; their P50 and throughput values
are read and digest-bound during provisioning.

The environment manifest is run-specific evidence, not a reusable template.
Build every service/profile image first, start the fresh stack with
`--no-build`, then capture the actual image IDs, source/Catalog/artifact
digests, database settings, and dataset counts before provisioning roots. Keep
that manifest outside `V4_FULL_RUN_DIR`: the runner mounts the evidence
read-only while `/results` is writable, so the two mounts must not reference
the same inode. A manifest from an earlier Catalog or image build is invalid
even though its JSON shape would still pass schema validation.

```sh
export V4_FULL_PROJECT="taskgate-v4-full-$(date -u +%Y%m%dT%H%M%SZ)"
export V4_FULL_RUN_DIR="$PWD/evaluation/v4-acceptance/raw/$V4_FULL_PROJECT"
# compose.scale-narrow.yaml contains an inactive narrow-only service whose
# bind variable is interpolated before profile filtering.
export V4_NARROW_RUN_DIR="$V4_FULL_RUN_DIR"
mkdir -m 700 -p "$V4_FULL_RUN_DIR"

export V4_FULL_ENVIRONMENT_MANIFEST=/absolute/path/to/environment.json
export V4_FULL_BASELINE_ARTIFACT=/absolute/path/to/small-query-baseline.json
export V4_FULL_CANDIDATE_ARTIFACT=/absolute/path/to/small-query-candidate.json
export V4_FULL_ENVIRONMENT_SHA256="$(sha256sum "$V4_FULL_ENVIRONMENT_MANIFEST" | awk '{print $1}')"
export V4_FULL_BASELINE_SHA256="$(sha256sum "$V4_FULL_BASELINE_ARTIFACT" | awk '{print $1}')"
export V4_FULL_CANDIDATE_SHA256="$(sha256sum "$V4_FULL_CANDIDATE_ARTIFACT" | awk '{print $1}')"

export V4_ACCEPTANCE_CONFIG="$V4_FULL_RUN_DIR/config.json"
export V4_ACCEPTANCE_RUN_DIR="$V4_FULL_RUN_DIR"

docker compose --project-name "$V4_FULL_PROJECT" \
  --file compose.yaml \
  --file evaluation/v4-acceptance/compose.scale-narrow.yaml \
  --file evaluation/v4-acceptance/compose.observer.yaml \
  --file evaluation/v4-acceptance/compose.full.yaml \
  up --detach --build --wait gateway

docker compose --project-name "$V4_FULL_PROJECT" \
  --file compose.yaml \
  --file evaluation/v4-acceptance/compose.scale-narrow.yaml \
  --file evaluation/v4-acceptance/compose.observer.yaml \
  --file evaluation/v4-acceptance/compose.full.yaml \
  --profile v4-full-tools run --rm v4-full-prepare

docker compose --project-name "$V4_FULL_PROJECT" \
  --file compose.yaml \
  --file evaluation/v4-acceptance/compose.scale-narrow.yaml \
  --file evaluation/v4-acceptance/compose.observer.yaml \
  --file evaluation/v4-acceptance/compose.full.yaml \
  --profile v4-acceptance run --rm v4-acceptance \
  -config /config/v4-acceptance.json \
  -output /results/results.json \
  -require-complete

docker compose --project-name "$V4_FULL_PROJECT" \
  --file compose.yaml \
  --file evaluation/v4-acceptance/compose.scale-narrow.yaml \
  --file evaluation/v4-acceptance/compose.observer.yaml \
  --file evaluation/v4-acceptance/compose.full.yaml \
  stop
```

`compose.full.yaml` gives Gateway a 2 GiB memory limit and sets the total
memory-plus-swap limit to the same value, which disables swap. This is not the
acceptance threshold: the gate still requires the observed cgroup-v2
`memory.peak`, including touched mmap pages, to be at most 512 MiB. The larger
container ceiling lets a passing run report its natural peak instead of being
clipped at the threshold. Current/maximum memory, current/peak/maximum swap,
and the cgroup `max`, `oom`, and `oom_kill` event counters are retained in raw
observer evidence. Reaching the 2 GiB ceiling or suffering an OOM is a failed
run, not a passing memory observation.

With a required observer, `-require-complete` evaluates 30 gates: provenance,
fixed environment, execution integrity, and observer completeness; four
overlap points and four query shapes; maximum-point novel and replay SLOs; two
independent replay-no-SQL checks; cgroup memory, network, and WAL evidence;
index-build time and RSS; total and hot artifact size; warm activation and
amortized storage; bitmap derivation, ordinal streaming, and settlement
timers; and the dual-artifact small-query regression. A failed or unmeasured
gate makes the command nonzero. `stop` deliberately retains the evidence
directory and Compose volumes for diagnosis; remove them only after the run
has been reviewed.

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
    "gateway_memory_current_bytes": 201326592,
    "gateway_memory_peak_bytes": 268435456,
    "gateway_memory_max_bytes": 2147483648,
    "gateway_memory_swap_current_bytes": 0,
    "gateway_memory_swap_peak_bytes": 0,
    "gateway_memory_swap_max_bytes": 0,
    "gateway_memory_events_max_total": 0,
    "gateway_memory_events_oom_total": 0,
    "gateway_memory_events_oom_kill_total": 0,
    "gateway_network_rx_bytes": 1000000,
    "gateway_network_tx_bytes": 2000000,
    "business_sql_queries_total": 42
  }
}
```

Memory current and configured maximum values are gauges; memory/swap peaks and
the `memory.events` values are cumulative within one Gateway cgroup. Network
bytes and Business SQL calls are monotonic counters. The acceptance gate reads
the cgroup memory peak from the after-snapshot rather than subtracting it.
Configure the corresponding metric names and required memory scope. The replay
zero-SQL gate remains `unmeasured` when only Gateway component labels are
available; it passes only with an external Business-query counter whose delta
is zero for every replay.
The observer must obtain that counter out of band (for example from an existing
metrics collector); its own snapshot calls must not increment the counter it
is supposed to observe.

The checked-in `v4-compose-observer` implements this contract without entering
the measured HTTP path. It uses the mounted Docker Engine socket to read the
Gateway container's private cgroup-v2 `memory.peak` and network namespace
counters. It fails closed unless `/proc/self/cgroup` proves that the cgroup
namespace root is the Gateway container, so a host-wide peak cannot be
misreported as Gateway memory. Linux cgroup-v2 accounts touched mmap-backed
pages in the cgroup memory controller.

For the SQL counter it opens a separate out-of-band connection using the
Business evaluation DSN and sums that role's own `pg_stat_statements.calls`
whose normalized text references `reporting.scale_*` or
`taskgate_ordinal.*`. The acceptance driver's `pg_current_wal_lsn`,
`pg_wal_lsn_diff`, and the observer query itself do not match the relation
filter. Replay therefore passes only when no visible or ordinal companion SQL
was issued.

Include `compose.observer.yaml` from the first start of a fresh evidence stack;
its init mount creates `pg_stat_statements`, while its PostgreSQL command
enables the required preload library. Adding the overlay only after reusing an
initialized Business volume is intentionally unsupported. A campaign can run
the bundled observer and acceptance driver with:

```sh
export V4_ACCEPTANCE_CONFIG="$V4_NARROW_RUN_DIR/config.json"
export V4_ACCEPTANCE_RUN_DIR="$V4_NARROW_RUN_DIR"
docker compose \
  --file compose.yaml \
  --file evaluation/v4-acceptance/compose.scale-narrow.yaml \
  --file evaluation/v4-acceptance/compose.observer.yaml \
  --profile v4-acceptance run --rm v4-acceptance
```

The Docker socket grants the evidence runner infrastructure-level authority;
this service is a trusted benchmark component and must not be exposed to an
agent or an untrusted plan.

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

The full template runs the bundled single-process compiler against the same
frozen Business snapshot used online:

```text
/usr/local/bin/v4-offline build
  -input /usr/local/share/taskgate/snapshots/scale-orders-v4-narrow-1.json
  -input /usr/local/share/taskgate/snapshots/scale-lineitem-v4-narrow-1.json
  -output-dir {{run_dir}}
```

Warm activation is split into two evidence-bearing commands over the Gateway's
retained, read-only `/var/lib/taskgate/snapshot-index` volume. First,
`v4-offline verify` streams and strictly verifies the manifest, HOT, COLD, and
sidecar, then writes a canonical receipt containing the approved semantic and
transport digests plus stable read-only inode identities. Its wall time,
command-output digest, and receipt are retained as raw evidence but are not
charged to the two-second online activation SLO. The runner hashes that exact
receipt and injects the measured SHA-256 into each `v4-offline activate` run.
Activation rejects a different receipt, mount, inode, manifest, or HOT digest;
only then does it load the HOT dictionaries without rereading COLD/sidecar.
The offline build receives `SNAPSHOT_POSTGRES_DSN`; its output stays under the
campaign directory and is separately counted, while online artifacts are
never overwritten.
