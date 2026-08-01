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

The smoke builds the checked-in `v5-smoke-adapter`, sends it the real runner JSONL operation stream, accepts at most three tiny samples per cell, finalizes a disposable pilot, and removes it. It does not start SF10, million-dependency, 100K-row, width-500, compiler-4,500-per-deployment, or RQ5-345K work.

## Runner/adapter contract

All runner entrypoints share `evaluation/internal/experiment`. A runner owns strict config validation, deterministic cell/arm order, warmup exclusion, process replicates, operation/sample identity, publication gating, fresh-root hash uniqueness, raw overwrite protection, and failure retention. `evaluation/finalv5oracle` independently computes exact trace unions and the fixed 70% RLS budgets without importing production exposure code. An environment adapter owns deployment-specific task provisioning and one measurement operation.

The exact executable passed with `-adapter` is started once per configured process replicate. It reads one `adapter-operation-v1` JSON object per stdin line and returns exactly one complete `sample-v1` JSON object per stdout line, in order. Warmup responses are validated but not written to raw evidence. For infrastructure failures the runner writes `invalid` measured samples. Adapter stderr must be empty because arbitrary diagnostics can leak credentials; safe failures belong in `status`, `error_code`, and `reason`. The adapter SHA-256 is frozen across deployments. Adapters must use only public `query_sql` for headline TaskGate cells, provision every requested fresh root, perform the direct typed drain or AVAILABLE/deliver/download/Parquet typed drain boundary, and verify V8 and audit inclusion before setting the corresponding booleans.

Private material is not checked in. Prepare these directories after the code/config freeze:

```text
private-config/
  baseline.json scale.json artifact.json rls.json attack.json
  provsql.json compiler.json concurrency.json rq5.json
private-adapter/
  baseline scale artifact rls attack provsql compiler concurrency rq5
dataset-bindings/
  deployment-01.json deployment-02.json deployment-03.json
```

Every dataset binding must contain SHA-256 values named `dataset_sha256`, `catalog_sha256`, and `deployment_volume_id_sha256`; the volume identity must differ across deployments. Copy the example configs, replace the campaign ID and zero commit, and keep credentials in environment variables consumed by the adapter—not in configs or bindings.

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
  -adapter /absolute/private-adapter/baseline \
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
  -AdapterDir /absolute/private-adapter `
  -DatasetBindingsDir /absolute/dataset-bindings `
  -Deployments 3
```

The controller writes and hashes a Windows-host manifest, binds that digest into every deployment manifest, and shuts WSL down before and after every deployment. `run-deployment.sh` then performs publication preflight, freezes config and adapter digests, captures the Ubuntu environment and vmstat before/after, invokes every default runner, records swap/OOM/restart acceptance evidence, and leaves the 10M/100M kernel-only profile disabled unless `TASKGATE_ENABLE_SCALE_EXTREME=1` is explicitly set.

## Finalize and seal

Each experiment is independently reconstructable below `raw/<campaign>/<experiment>/`. After all three deployments, finalize and review each directory separately:

```sh
make eval-v5-final-finalize RUN_DIR=/absolute/path/to/raw/<campaign>/baseline
make eval-v5-final-evidence RUN_DIR=/absolute/path/to/raw/<campaign>/baseline
```

A passing run produces `generated/summary.json`, `summary.csv`, `paper-results.json`, `latex/evidence.tex`, `figures/`, and a sealed `evidence/manifest.json`. Updating `paper/tkde/generated/evidence.tex` remains a separate, explicit author action after all experiments are reviewed.
