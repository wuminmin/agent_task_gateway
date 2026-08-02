# TaskGate final V5 WSL2 experiment framework

This is the preregistered, fail-closed home for the future TKDE V5 + Parquet + Receipt V8 campaign. It contains no publication result. Historical V4, V5 Outcome, RQ5, and ProvSQL evidence is read-only and is never copied or relabeled here.

## Safety boundary

- `pilot` always writes `publication_eligible=false` and a `PILOT-NOT-FOR-PUBLICATION` marker.
- `publication` requires the exact `TASKGATE_EXPERIMENT_CLASS`, unique campaign ID, and future frozen 40-character commit. Preflight requires Ubuntu 22.04 under WSL2, Linux storage, cgroup v2, Compose 2.24.4+, 24 GiB memory, eight processors, zero swap, 180 GiB free, and a clean measured tree.
- Raw JSONL and derived evidence use create-exclusive files. Failed and invalid samples remain in raw evidence. Task/request IDs are stored only as deployment-salted hashes; tokens, DSNs, keys, rows, and credentials are forbidden.
- Top-level `pipeline_ms` phases are non-overlapping. `diagnostic_ms` entries such as `ordinal_stream` and `bitmap_derivation` may overlap and must not be summed.
- Finalization recomputes Type-7 p50/p95 and a deterministic bootstrap from raw samples. Pilot or incomplete evidence cannot be sealed and emits no numeric LaTeX macros.

## Engineering checks

```sh
make eval-v5-final-validate
make eval-v5-final-preflight
make eval-v5-final-smoke
```

`eval-v5-final-smoke` uses `config/smoke.example.json` with `pilot_kind=synthetic_smoke`; it remains a scheduler/schema smoke and is not a Pilot. The first real-system gate uses the separate `pilot_kind=real_system` config:

```sh
make eval-v5-final-real-pilot
```

It creates a unique fresh Compose project, captures Docker/PostgreSQL/MinIO freshness evidence, provisions real tasks through public OA approval, and runs the source-controlled adapter through Direct PostgreSQL plus public `query_sql`, semantic replay, idempotent replay, AVAILABLE delivery, Parquet parsing, V8 verification, and audit-inclusion verification. A dedicated recovery arm forces the real Control `PENDING -> AVAILABLE` transaction to fail after the canonical object exists, then proves from `pg_stat_statements`, Control counters, the object store, and the unchanged V8 intent that recovery neither re-queries nor re-settles. It remains `PILOT-NOT-FOR-PUBLICATION` and currently covers only baseline/S1/tiny.

## Runner/adapter contract

All runner entrypoints share `evaluation/internal/experiment`. A runner owns strict config validation, exact source-controlled protocol/cell binding, matched-pair identity and randomized arm order, warmup exclusion, process replicates, publication gating, fresh-root hash uniqueness, raw overwrite protection, and failure retention. `evaluation/finalv5oracle` independently computes exact trace unions and the fixed 70% RLS budgets without importing production exposure code.

The one checked-in executable is `evaluation/cmd/final-v5-adapter`. The runner invokes it with `--experiment`; it reads operation JSONL and returns exactly one sample per line. Its source manifest, build command, frozen commit, and binary SHA-256 enter every evidence pack. `--capabilities` is fail-closed: a formal campaign cannot start until all nine experiment implementations report complete. At present only `baseline` reports complete; every other experiment returns structured invalid evidence.

Private material is not checked in. Prepare these directories after the code/config freeze:

```text
private-config/
  baseline.json scale.json artifact.json rls.json attack.json
  provsql.json compiler.json concurrency.json rq5.json
dataset-bindings/
  deployment-01.json deployment-02.json deployment-03.json
```

Every dataset binding must contain `dataset_sha256` and `catalog_sha256`. Docker volume identity is no longer author-supplied: `start-fresh-deployment.sh` derives it from actual Compose volume inspection and PostgreSQL system identifiers. Copy the example configs, replace the campaign ID and zero commit, and keep credentials in environment variables—not in configs or bindings.

## Commands

Offline validation:

```sh
go run ./evaluation/cmd/v5-full -config evaluation/final-v5-wsl2/config/publication.example.json -validate-only
go run ./evaluation/cmd/v5-scale -config evaluation/final-v5-wsl2/config/scale.example.json -validate-only
go run ./evaluation/cmd/v5-artifact -config evaluation/final-v5-wsl2/config/artifact.example.json -validate-only
go run ./evaluation/cmd/rls-adaptive -config evaluation/final-v5-wsl2/config/rls.example.json -validate-only
go run ./evaluation/cmd/adaptive-attacks -config evaluation/final-v5-wsl2/config/attacks.example.json -validate-only
go run ./evaluation/cmd/taskgate-provsql-pair -config evaluation/final-v5-wsl2/config/provsql-paired.example.json -validate-only
go run ./evaluation/cmd/view-scale -config evaluation/final-v5-wsl2/config/compiler-scale.example.json -validate-only
go run ./evaluation/cmd/v5-concurrency -config evaluation/final-v5-wsl2/config/concurrency.example.json -validate-only
go run ./evaluation/cmd/v5-rq5 -config evaluation/final-v5-wsl2/config/daily-publication.example.json -validate-only
```

A single formal runner has this exact shape; all three environment values are mandatory and must match the config:

```sh
TASKGATE_EXPERIMENT_CLASS=publication \
TASKGATE_SUBMISSION_COMMIT=<40-char-sha> \
TASKGATE_CAMPAIGN_ID=<unique-id> \
go run ./evaluation/cmd/v5-full \
  -config <campaign-run>/baseline/config.json \
  -deployment-id deployment-01 \
  -adapter /path/to/campaign/source-adapter/final-v5-adapter \
  -output <campaign-run>/baseline/raw/deployment-01.jsonl
```

Do not launch formal runners by hand for the final campaign. Start the three deployments from Windows PowerShell:

```powershell
evaluation/final-v5-wsl2/scripts/run-three-deployments.ps1 `
  -Distro Ubuntu-22.04 `
  -RepoPath /home/<user>/agent-scope/task_gateway `
  -CampaignId tkde-v5-<unique> `
  -FrozenCommit <40-char-sha> `
  -PrivateConfigDir /absolute/private-config `
  -DatasetBindingsDir /absolute/dataset-bindings `
  -Deployments 3
```

The controller copies the complete Windows-host manifest bytes into WSL evidence, not only its hash, and shuts WSL down between deployments. `run-deployment.sh` builds the unified adapter from the frozen commit, checks all capabilities before starting expensive work, creates the fresh Compose project itself, captures freshness/environment/vmstat evidence, invokes every runner, and records swap/OOM/restart acceptance evidence.

## Finalize and seal

Each experiment is independently reconstructable below `raw/<campaign>/<experiment>/`. After all three deployments, finalize and review each directory separately:

```sh
make eval-v5-final-finalize RUN_DIR=/absolute/path/to/raw/<campaign>/baseline
make eval-v5-final-evidence RUN_DIR=/absolute/path/to/raw/<campaign>/baseline
```

A passing run produces `generated/summary.json`, `summary.csv`, `paper-results.json`, `latex/evidence.tex`, `figures/`, and a sealed `evidence/manifest.json`. Updating `paper/tkde/generated/evidence.tex` remains a separate, explicit author action after all experiments are reviewed.

Only after all nine experiment directories pass and are sealed may the campaign-level gate run:

```sh
make eval-v5-final-campaign-finalize CAMPAIGN_ROOT=/absolute/path/to/raw/<campaign>
```
