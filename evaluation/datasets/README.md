# Dataset preparation

The repository does not redistribute benchmark data. The checked workloads are
TPC-derived, using a small fixed query subset over TPC-H/TPC-DS-shaped schemas.
Generate SF1 and SF10 with the licensed benchmark kits or another documented
generator, import each generated dataset into a dedicated PostgreSQL 16
database, and retain the generator version and seed. Do not claim a standard
TPC-H or TPC-DS result unless the official generator, query templates, run
rules, and disclosure requirements are independently satisfied.

After import, install the evaluation views as the database owner:

```sh
psql "$ADMIN_DSN" -f evaluation/datasets/install-tpch-views.sql
psql "$ADMIN_DSN" -f evaluation/datasets/install-tpcds-views.sql
```

Grant a non-owner, non-superuser direct role only the physical benchmark tables
needed by the workload. Grant the native and Gateway roles `USAGE` on
`reporting` and `SELECT` only on the corresponding views. Keep all roles
without `BYPASSRLS`. Use distinct database copies (or rigorously reset caches)
for baseline isolation.

Record the generated files before import:

```sh
TPC_GENERATOR_NAME=tpc-h \
TPC_GENERATOR_VERSION=3.0.1 \
TPC_GENERATOR_SEED='<recorded seed>' \
evaluation/datasets/record-manifest.sh tpch 1 /data/tpch-sf1 \
  evaluation/environment/tpch-sf1.sha256-manifest.local
```

Set the matching `EVAL_*_DATASET_MANIFEST` variable to the manifest's container
path (for the example above,
`/workspace/evaluation/environment/tpch-sf1.sha256-manifest.local`) and set
`EVAL_*_DATASET_SHA256` to the exact 64-character lowercase SHA-256 printed by
the script. The combined full-run preflight reads every SF1/SF10 manifest and
verifies its digest before any measurements begin.

For full runs, also copy `evaluation/environment/reference.json` to an ignored
repository-local manifest, replace every placeholder with measured environment
and post-import dataset facts, and point `EVAL_ENVIRONMENT_MANIFEST` at that
container path. The runner records the manifest path and SHA-256 in `run.json`.
