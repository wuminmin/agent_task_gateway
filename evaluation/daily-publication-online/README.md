# RQ5 retained-publication online transition experiment

This isolated experiment measures the online half of RQ5. It uses the real
TaskGate Control store, PostgreSQL connector, Gateway services, the public
`request_data_task` tool and directly callable advanced `execute_plan` method,
and signed OA submitted plus approved/narrowed callbacks. `execute_plan` is
retained for deterministic harnesses but hidden from an ordinary Agent's
`tools/list`. The experiment does not add or modify a production `latest`
router.

The experiment-only router owns four already-constructed, Catalog-bound
Gateway services. It changes only the service selected for a subsequent root
approval. Query execution resolves the task's persisted Catalog version back
to its retained service, and a delegated child is created through the same
public approval path as its root.

Each service connects to a separate PostgreSQL database cloned from one
deterministically generated daily reporting fixture. This is intentional: the
production sidecar registry gives each content digest one immutable owner, and
Day0 through Day2 retain the same entity-key set. Separate cloned databases
therefore provide four durable registries without weakening that uniqueness
constraint. Every generated Catalog commits its exact clone database identity.

## Run

From the repository root, the default 2,000-row correctness smoke is:

```sh
evaluation/daily-publication-online/run.sh
```

The publication-scale point is opt-in and uses the same command path:

```sh
DAILY_PUBLICATION_ROWS=345000 \
DAILY_PUBLICATION_ONLINE_RUN_ID=scale-$(date -u +%Y%m%dT%H%M%SZ) \
evaluation/daily-publication-online/run.sh
```

The tool and installer containers default to a 6 GiB limit. Override it with
`DAILY_PUBLICATION_ONLINE_MEMORY_LIMIT` when the host requires another explicit
limit. Set `DAILY_PUBLICATION_ONLINE_KEEP_STACK=1` only for diagnosis; normal
runs delete their isolated databases and retain evidence under
`evaluation/daily-publication-online/raw/<run-id>/`.

The driver refuses to overwrite a run directory. It rebuilds the deterministic
fixture from source, obtains live connector schema attestations, performs a
calibration compile, rebuilds artifacts from approved exact digests, installs
each sidecar through `snapshot-sidecar-install`, then runs the transition
experiment. Missing PostgreSQL, malformed artifacts, digest drift, dirty
Control state, callback failure, or any incorrect oracle relationship is fatal.

The Go and runtime base images are digest-pinned in `Dockerfile` and
`compose.yaml`. The repository `.dockerignore` excludes both offline and online
raw directories from build contexts.

## Evidence and claim boundary

`online-evidence.json` binds its row count, fixture class, generator/config/data
manifest hashes, and all four approved-input, Catalog, Bundle, publication,
dictionary, sidecar, schema, HOT/COLD/sidecar artifact, and direct-result
digests. For each of the three transitions it records:

- experiment-pointer switch wall time;
- first public query wall time and required cache miss;
- second public query wall time and required semantic replay;
- old task publication/result equality against its direct frozen oracle;
- unchanged old root-ledger head;
- delegated root/child equality against the transition target publication.

The switch measurement covers only the experiment router's atomic pointer
change. It explicitly excludes offline build/verify/activation and is not a
measurement of a production routing subsystem. First-query and replay timings
are labeled with this online evidence's own `rows_per_publication`; they must
not inherit the row count of a different offline campaign.

Validate a retained artifact independently with:

```sh
python3 evaluation/daily-publication/harness.py validate-online \
  --evidence evaluation/daily-publication-online/raw/<run-id>/online-evidence.json
```

To combine it with a fresh offline campaign, pass the path explicitly. The
offline driver copies the exact file into its new run directory before
summarizing so raw provenance includes it:

```sh
DAILY_PUBLICATION_ONLINE_EVIDENCE=evaluation/daily-publication-online/raw/<run-id>/online-evidence.json \
evaluation/daily-publication/run.sh
```

The combined result reports `same_dataset_distinct_attested_artifacts`,
`separate_correctness_and_scale_fixtures`, or
`same_scale_distinct_fixture` from the two row counts and dataset-manifest
hashes. Online artifacts are rebuilt against the live connector-attested view
schema and are therefore validated independently rather than required to be
byte-identical to an offline artifact pack.

## Source-controlled formal evidence

The formal 345,000-row descriptor pack is retained at
`evidence/scale-20260730-final/`. It contains the transition record, dataset and
preparation manifests, four approved inputs, four Catalogs, and four bundle
manifests. It deliberately omits the twelve HOT/COLD/sidecar payload files
(7,701,214,660 logical bytes); their names, sizes, and SHA-256 values remain
bound by the retained descriptors. Validate the exact file set, pack manifest,
and all online conditions with:

```sh
PYTHONDONTWRITEBYTECODE=1 \
  python3 evaluation/daily-publication-online/evidence/validate.py
```

The combined paper validator additionally requires the exact 345,000-row
offline pack and rejects smoke-scale or cross-fixture substitution.
