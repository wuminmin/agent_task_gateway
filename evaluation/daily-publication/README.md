# RQ5 daily reporting-publication experiment

This harness asks whether TaskGate can publish a scheduled reporting snapshot
within a daily operating window while keeping already-approved task families
on their original immutable publication. It does **not** model a mutable OLTP
primary, CDC, or an in-place hot swap of one Gateway service.

The deployment contract is: any `latest` alias is resolved to a concrete
publication in the authorization snapshot presented for root approval;
approval confirms that binding, and queries never resolve the alias. A
publication transition changes the version selected for new authorization
snapshots. Retained, version-routed Gateway/Catalog instances continue to serve
active roots and delegated children bound to older versions.

## Workload

The isolated PostgreSQL fixture is TPC-H-shaped, not a claim of audited TPC-H
compliance. It uses deterministic order and five-lineitem keys and the same
price formula as `evaluation/exposure-scale`. Four populated materialized views
are owned by a separate `NOLOGIN` role:

| Publication | Change from previous publication |
| --- | --- |
| Day0 | Initial snapshot |
| Day1 | Exactly 1% `l_extendedprice` updates |
| Day2 | Exactly 5% `l_extendedprice` updates |
| Day3 | Exactly 10% updates plus 1% inserts and 1% deletes |

The default smoke uses 2,000 rows. `DAILY_PUBLICATION_ROWS=345000` selects the
opt-in publication-scale point. The row count must be a multiple of 500, which
makes every percentage an exact integer and keeps five-lineitem order groups
whole. The generated dataset manifest independently joins adjacent views to
count updates/inserts/deletes and fingerprints every ordered version.

## Offline run

From the repository root:

```sh
DAILY_PUBLICATION_ALLOW_INCOMPLETE=1 \
  evaluation/daily-publication/run.sh
```

The driver creates a new isolated Compose project and refuses to overwrite its
run directory. For every day it performs an unmeasured calibration build to
obtain candidate digests, writes an approved input bound to those exact
digests, and then runs three independent measured samples. Each sample invokes
the existing production path:

```text
v4-offline build -> v4-offline verify -> v4-offline activate
```

`verify` receives the artifact directory through a read-only bind mount and
streams the manifest, HOT, COLD, and sidecar. `activate` receives that same
read-only mount plus the exact verification receipt and SHA-256, then loads the
HOT index without charging strict verification to activation. A dedicated
single-child wrapper records monotonic child wall time and Linux
`/proc/<pid>/status` `VmHWM`; Compose/container startup is outside the timing.
Artifact bytes and semantic/transport digests come from `v4-offline`, not from
an estimate.

The offline gate is deliberately broad: each measured build + strict verify +
receipt-bound activation cycle must finish within 300,000 ms. It is a local
engineering target for a 24-hour publication cadence, not an industry SLO.

For the opt-in scale run, use a fresh ID and allow enough memory/disk for 16
publication builds (four calibration plus twelve measured artifacts):

```sh
DAILY_PUBLICATION_ROWS=345000 \
DAILY_PUBLICATION_RUN_ID=scale-$(date -u +%Y%m%dT%H%M%SZ) \
DAILY_PUBLICATION_ALLOW_INCOMPLETE=1 \
  evaluation/daily-publication/run.sh
```

The compiler's existing 2 GiB total-artifact and 160 MiB HOT limits remain in
force. If the scale point exceeds a production limit or the default 6 GiB
phase-container ceiling, the run fails; the harness does not replace it with a
smaller result.

By default the disposable Compose database/volume is removed at exit while the
run evidence is retained under ignored `raw/<run-id>/`. Set
`DAILY_PUBLICATION_KEEP_STACK=1` for diagnosis. `results.json` exits with code 2
when offline evidence passes but required online evidence is absent; the
`DAILY_PUBLICATION_ALLOW_INCOMPLETE=1` flag makes that expected engineering
smoke condition return zero without changing the JSON status.

## Online transition evidence

The repository includes an isolated online runner under
`evaluation/daily-publication-online/`. It constructs four retained,
Catalog-bound Gateway services over four cloned frozen PostgreSQL databases and
uses an experiment-only approval-time router. The router does not add a mutable
runtime `latest` alias or change production routing. Its switch timing covers
only the experiment pointer; offline timings cannot be relabeled as switch,
first-query, replay, task-binding, or ledger-isolation measurements.

Run the default correctness smoke with:

```sh
evaluation/daily-publication-online/run.sh
```

See `evaluation/daily-publication-online/README.md` for the opt-in 345,000-row
command, four-registry topology, fail-closed preparation path, and exact claim
boundary.

Any version-routed campaign may pass its JSON path as
`DAILY_PUBLICATION_ONLINE_EVIDENCE`. It must use schema
`taskgate-daily-publication-online-evidence-v1`, routing model
`approval_time_version_routed_retained_instances`, and exactly three ordered
transitions (`day0->day1`, `day1->day2`, `day2->day3`). Top-level
`rows_per_publication`, the exact experiment-only `measurement_boundary`, and a
`fixture` object bind the generator/config/dataset hashes plus all four daily
Catalog, publication, artifact, and direct-result hashes. Each transition
records positive client-wall `switch_wall_ms`, `first_query_wall_ms`, and
`replay_wall_ms`, plus these raw values from public query responses and the
isolated Control database:

```json
{
  "from": "day0",
  "to": "day1",
  "switch_wall_ms": 1.0,
  "first_query_wall_ms": 1.0,
  "replay_wall_ms": 1.0,
  "old_task": {
    "publication_digest_before": "<sha256>",
    "publication_digest_after": "<same sha256>",
    "expected_publication_digest": "<same sha256 from direct snapshot>",
    "result_sha256_before": "<sha256>",
    "result_sha256_after": "<same sha256>",
    "expected_result_sha256": "<same sha256 from direct snapshot>"
  },
  "new_task": {
    "publication_digest": "<day1 sha256>",
    "expected_publication_digest": "<same sha256 from direct snapshot>",
    "result_sha256": "<sha256>",
    "expected_result_sha256": "<same sha256 from direct snapshot>"
  },
  "old_ledger": {
    "before_switch_sha256": "<canonical ledger sha256>",
    "after_switch_sha256": "<same sha256>"
  },
  "cache": {
    "old_cache_key_sha256": "<sha256>",
    "first_new_cache_key_sha256": "<different sha256>",
    "first_new_semantic_replay": false,
    "replay_new_cache_key_sha256": "<same new sha256>",
    "replay_new_semantic_replay": true
  },
  "delegation": {
    "root_task_id": "<root>",
    "child_root_task_id": "<same root>",
    "child_parent_task_id": "<nonempty parent>",
    "root_publication_digest": "<sha256>",
    "child_publication_digest": "<same sha256>"
  }
}
```

The summarizer derives—rather than accepts asserted pass flags—the five RQ5
conditions: old task/old data, new task/new data, ledger unchanged by the
switch, cross-publication cache miss followed by an intra-publication replay,
and delegated-child/root publication equality against the transition target.
All five must pass for every transition. It also reports whether online and
offline evidence use the same fixture, different row scales, or distinct
fixtures at one scale; online latency always retains its own row-count label.
The offline driver copies supplied online evidence into its new run directory
before summary generation.

## Evidence integrity and validation

The result binds the config, dataset manifest, copied online evidence, every
JSON raw/receipt/bundle manifest, and exact production source inputs by
SHA-256. The source manifest includes both drivers and Compose/Docker/SQL
assets, the online runner and sidecar installer, and the directly used
approval, Catalog, connector, Control, Gateway, MCP, snapshot, ordinal, domain,
and exposure packages. This remains auditable even when the surrounding Git
worktree is dirty.

Run the non-integration tests with:

```sh
make eval-daily-publication-validate
```

That target validates both source-controlled compact packs, recomputes the
combined paper evidence, runs their mutation tests, checks both shell drivers
and the Compose model, and executes the pinned-container Go tests. The offline
pack is `evidence/scale-20260730-final3/`; the online descriptor pack is
`../daily-publication-online/evidence/scale-20260730-final/`. Neither pack
contains the multi-gigabyte HOT/COLD/sidecar payload bytes, and each validator
reports that audit boundary explicitly.

The checked-in `results.json` is the complete 345,000-row formal summary:
all twelve gates pass and the cross-campaign relationship is
`same_dataset_distinct_attested_artifacts`. A fresh offline-only run still
produces an honest `incomplete` result: missing online infrastructure leaves
latencies null and all five online correctness gates `unmeasured`; no offline
timing is copied into an online RQ5 field.
