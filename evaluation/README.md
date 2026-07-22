# Reproducible evaluation harness

This directory implements the four-baseline TDSC evaluation without shipping
benchmark data or checked-in measurements. It never substitutes mock timing
data when a prerequisite is absent.

## Baselines and fairness boundary

The driver executes the same deterministic, ordered result set through:

1. `direct_postgresql`: a persistent PostgreSQL connection using a read-only
   transaction and the physical benchmark tables.
2. `native_view_rls`: a distinct non-owner PostgreSQL role restricted to the
   native reporting views (deployments may add RLS beneath those views).
3. `ast_only_gateway`: `evaluation/cmd/ast-gateway`, which uses the repository's
   PostgreSQL AST policy/rewrite and read-only connector but has no task,
   approval, Control PostgreSQL, budget ledger, or receipt work.
4. `full_taskgate`: the real Gateway `query_sql` MCP tool with pre-approved,
   independent tasks.

Direct and native queries use physical/view names; both Gateway paths use the
logical product name. Every query is ordered. The driver hashes returned rows
and fails the run if a query is unstable or differs across baselines.

Each worker owns a distinct TaskGate task and issues only one query at a time.
Warmups complete before measurement begins. Full configurations use five
warmups and 30 measured runs **per worker** at concurrency 1, 8, and 32 for
TPC-H and TPC-DS SF1/SF10.

## Prerequisites

- Docker Engine and a network path from the evaluation container to every
  service.
- Four isolated PostgreSQL/Gateway deployment paths for each selected dataset.
- Licensed TPC-H/TPC-DS kits and generated SF1/SF10 data. See
  `datasets/README.md`; generated data is intentionally not redistributed.
- One ACTIVE TaskGate task per worker **and per concurrency cell**. Every task
  ID must be globally distinct across all four experiments and all concurrency
  cells; do not reuse a task between SF1/SF10, TPC-H/TPC-DS, or concurrency
  1/8/32. Each task must approve every workload product/column,
  `eval_scope=["all"]`, at least `warmup + measured` queries (the supplied
  catalogs allow 64), and a TTL long enough for the cell.
  `config/tasks.example.json` contains distinct placeholders for the complete
  four-experiment pool.
- For a full run, a real metrics probe per baseline and dataset. It should
  query the deployment's monitoring system over the supplied time window.

Create the ignored local environment file, then replace every placeholder used
by the selected suite:

```sh
cp evaluation/.env.example evaluation/.env
cp evaluation/config/tasks.example.json evaluation/config/tasks.local.json
```

A task-pool path is a container path, normally
`/workspace/evaluation/config/tasks.local.json`. For every full-run dataset,
set `EVAL_*_DATASET_MANIFEST` to the repository-local manifest created by
`datasets/record-manifest.sh`, and set `EVAL_*_DATASET_SHA256` to the exact
64-character lowercase SHA-256 that script prints. The runner reads the
manifest and verifies its digest before measuring. Do not put secrets, real
task IDs, local manifests, or local probes in version control.

The AST-only process can be launched separately for each dataset deployment:

```sh
EVAL_ENV_FILE=evaluation/.env evaluation/run-ast-gateway.sh tpch
```

The TaskGate deployment should use `taskgate/catalog.tpch.yaml` or
`taskgate/catalog.tpcds.yaml` and the matching reporting views.

## Commands

Validate all manifests and query paths without contacting a backend:

```sh
make eval-validate
```

Run a small, real four-baseline SF1 cell (one warmup, three measurements):

```sh
make eval-smoke
```

Run both SF1 and SF10 performance suites, followed by the attack corpus and
the default 24-CPU-hour fuzz campaign:

```sh
make eval-full
```

Missing DSNs, URLs, tokens, task pools, dataset digests, or required probes are
fatal. The full target also fails if any security test/fuzz target fails or if
the parsed sum of GNU `time` user+system seconds is below the requested 24 CPU
hours. Partial performance runs retain `run.json` with `status=failed` and are
excluded from artifact generation.

Before any measurement starts, `evaluation/run.sh full` performs one combined
runtime preflight over both SF1 and SF10 configs. It validates every DSN/URL,
token, globally unique task pool, dataset manifest/digest, and all 16 metrics
probe commands. Thus an SF10 prerequisite failure cannot be discovered only
after completing SF1. The subsequent two runs share one campaign ID. A campaign
manifest remains `running` after an interrupted/failed suite and is sealed as
`complete` with both `run.json` SHA-256 values only after both suites finish.

## Metrics probe contract

Each full config names an environment variable whose value is a JSON argv
array, for example:

```text
["/workspace/evaluation/metrics/prometheus-probe", "--url", "http://metrics:9090"]
```

The first argv element must be a regular executable file under `/workspace`;
commands found only through the container image or host `PATH` are rejected.
The runner records the repository-relative probe path and SHA-256 for every
experiment/baseline so artifact generation can detect a changed probe.

The driver invokes it after a measurement cell with these environment values:

- `EVAL_PROBE_EXPERIMENT`
- `EVAL_PROBE_BASELINE`
- `EVAL_PROBE_CONCURRENCY`
- `EVAL_PROBE_START_UNIX_NS`
- `EVAL_PROBE_END_UNIX_NS`

It must return one JSON object:

```json
{
  "cpu_seconds": {"postgres": 12.3, "gateway": 1.4},
  "peak_memory_bytes": {"postgres": 536870912, "gateway": 67108864},
  "control_transactions": 210,
  "receipt_storage_bytes": 48210,
  "component_ms": {"control": 83.2, "policy": 41.7}
}
```

CPU and memory maps are mandatory when a probe is configured. Control
transactions, receipt storage, and component timings may be `null` when they
do not apply, but a paper must leave them unreported rather than infer values.
The harness does not mount the Docker socket or guess server resource use from
client process metrics.

## Raw and derived data

Every run gets a new `raw/<run-id>/` directory:

- `run.json`: config/workload/dataset/probe paths and digests, campaign and Git
  provenance, endpoints with secrets removed, and success/failure status.
- `samples.jsonl` and `samples.csv`: one measured query per row.
- `cells.jsonl`: elapsed interval, observed throughput, and probe output.
- `campaign-<id>.json`: the exact SF1+SF10 run IDs and their sealed `run.json`
  digests for a completed full campaign.

Generate deterministic summaries, SVGs, LaTeX, and the paper interchange file:

```sh
make artifacts
```

By default the generator selects the latest publishable full campaign, or the
latest smoke suite only when no completed full run exists. A publishable
campaign is exactly one SF1 and one SF10 suite with exact TPC-H/TPC-DS,
four-baseline, and concurrency 1/8/32 coverage; both runs must have the same
campaign ID, known Git revision, clean worktree, runtime provenance, and a
matching complete campaign manifest. Partial, dirty, mixed-revision, or
unlinked full runs are never labeled complete. With `--allow-empty` they
produce `performance.status=not_measured`; ordinary artifact generation fails.
Any checksum mismatch in a selected campaign is always a hard integrity error.
Smoke summaries are labeled `performance.status=smoke`, not complete.

Pin exact run inputs with a colon-separated repository-relative list. Both
full run directories must belong to the same sealed campaign:

```sh
EVAL_RAW_RUNS=evaluation/raw/full-sf1-...:evaluation/raw/full-sf10-... make artifacts
```

The output `generated/paper-results.json` includes SHA-256 provenance for every
raw input. Percentiles use Hyndman-Fan type 7. No human-study metric is present;
approval counts are computational protocol events only.

## Fuzz and attack scaffolding

`attacks/` contains versioned SQL and prompt-injection boundary cases. Prompt
cases map untrusted text to representative physical-object, system-catalog,
and non-SELECT SQL attempts; the test checks the deterministic Gateway policy
boundary only and makes no claim about model robustness. `fuzz/` contains
three targets: SQL authorization panic safety, formatting metamorphism, and
QueryPlan compilation panic safety. A long campaign is explicit and preserves
complete logs:

```sh
FUZZ_CPU_HOURS=24 FUZZ_WORKERS=4 make fuzz
```

Requested CPU-hours are a schedule, not a result. Publication acceptance is a
separate fixed bar: all three exact targets must pass and the hash-verified GNU
`time` logs must contain at least 24 hours of aggregate user+system CPU time.
For pipeline development only, for example:

```sh
SECURITY_ALLOW_PARTIAL=1 FUZZ_CPU_HOURS=1 FUZZ_SECONDS_PER_TARGET=1 \
  evaluation/security/run-full.sh
```

The override is recorded and can never satisfy the fixed publication bar;
without the explicit `SECURITY_ALLOW_PARTIAL=1` development opt-in, the
security pipeline exits nonzero and does not leave a canonical result.
`security/verify.py` reparses `go test -json` and GNU-time logs, checks the
target/corpus membership and every declared SHA-256 digest, and deterministically
writes `security/results.json`. Artifact generation invokes the verifier again
and includes that result plus all evidence files in top-level provenance.

The scoped security summary remains `partial` even after the corpus and fuzz
components pass: these runs do not measure connector-crossing or ledger fault
invariants, so those values remain null and the full security acceptance bar is
not claimed. Preserve the raw manifests, Go version, Git revision, seeds, and
complete logs with any published component claim.
