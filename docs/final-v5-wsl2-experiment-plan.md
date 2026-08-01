# Final V5 WSL2 experiment plan

This is the author execution plan for the final TaskGate TKDE V5 campaign. The framework-development session may run unit tests, validation, and a tiny pilot only. It must not execute publication-scale cells or update paper numbers.

## Questions and claim boundary

The campaign measures: full public TaskGate versus direct PostgreSQL; dependency, Outcome-radix, and Parquet scaling; real PostgreSQL RLS versus unlimited/bounded TaskGate over a frozen 100-query adaptive trace; adaptive attacks A–E; a nonce-connected ProvSQL pairing; View/compiler scaling; shared-root safety and natural CAS contention; and a V5/Parquet/V8 retained-publication RQ5 path. `evaluation/finalv5oracle` computes the trace's exact U_R/U_D/U_O unions and fixed 70% budgets without importing production exposure code. Claims are limited to WSL2 Ubuntu 22.04, PostgreSQL 16, immutable reporting publications, one Gateway instance, and recorded warm/cold policies.

Hypotheses are frozen in `protocol/hypotheses-v1.yaml`. The RLS budget is independently calculated before bounded execution as floor(70% of each complete-trace union), with a minimum of one in every nonzero dimension. Attack E thresholds and the Outcome ceiling are frozen before execution.

## Measurement boundaries

Direct PostgreSQL includes sending SQL, complete typed drain, and canonical multiset hashing. Full TaskGate starts at public `query_sql`, waits for AVAILABLE, calls `deliver_result`, downloads/decrypts/typed-parses all Parquet rows, and computes the same hash. `client_available_ms` and `client_full_drain_ms` are both retained.

The non-overlapping Gateway phases are prepare, execute-and-derive, artifact stage, Control settlement, artifact publication, response finalization, and server total. Detailed diagnostic timers overlap where documented. ProvSQL has a common generation boundary and a separately reported full TaskGate boundary. Outcome-radix is a Merkle/Control microbenchmark, never SQL E2E. Extreme 10M/100M profiles are kernel/storage-only.

## Three fresh deployments

1. Freeze the final code and configuration in a new full commit. Do not modify measured paths afterward.
2. Prepare immutable datasets, Catalogs, independent oracle outputs, fresh TaskGate root pools, RLS role/policy, and nonce rows. Hash all inputs. Keep credentials private.
3. Build the credential-free site adapters, freeze their SHA-256 values, and prepare per-deployment dataset bindings containing the frozen dataset/Catalog digests and a unique deployment-volume identity hash. From Windows PowerShell run `run-three-deployments.ps1` with the distro, WSL repository path, unique campaign ID, frozen commit, private config directory, adapter directory, and dataset-binding directory.
4. Before each deployment the controller performs `wsl.exe --shutdown`; WSL preflight and environment capture then run before any sample.
5. Use seeded randomized arm/cell order. Baseline and ProvSQL use five warmups and 30 measured samples; every novel sample gets a fresh root. Compiler uses one untimed warmup, 100 compiles/cell, five fresh processes, fixed GOMAXPROCS. Concurrency uses 30 fresh roots per width/mode. RQ5 uses four build–verify–activate cycles per deployment.
6. Preserve every failure, timeout, invalid cell, accepted adapter stdout sample, observer snapshot, raw sample, exact config, adapter digest, and manifest. Adapters must keep stderr empty and encode safe diagnostics in the sample because arbitrary stderr can leak credentials. Capture vmstat after, Docker events/restarts, OOM counters, and image IDs.
7. Shut WSL down before the next deployment.

## Author run order

Run offline validation, then pilot. After review/freeze, execute deployment environment capture followed by: full baseline/S1–S6; dependency and artifact scale; Outcome radix microbenchmark; RLS; attacks; paired ProvSQL; compiler; concurrency; and RQ5. The 10M/100M kernel-only profile is a separate explicit opt-in and is not part of default E2E.

Within a private deployment configuration, validate each runner with the commands in `evaluation/final-v5-wsl2/README.md`. The PowerShell/deployment scripts then invoke baseline, scale, artifact, RLS, attack, ProvSQL, compiler, concurrency, and RQ5 runners in the preregistered order. The exact JSONL adapter contract is in `schema/adapter-operation-v1.schema.json`; the runner, not the adapter, controls randomized order and evidence identity. RLS must prove the role is non-owner and lacks BYPASSRLS. ProvSQL must use `n.join_key = orders.nonce_join_key` plus a caller nonce predicate. Width 500 is invalid unless all clients reach the barrier and the observer proves offered service/queue concurrency.

Engineering validation and pilot commands are:

```sh
make eval-v5-final-validate
make eval-v5-final-preflight
make eval-v5-final-smoke
```

The only supported full-campaign entry is the PowerShell controller shown in `evaluation/final-v5-wsl2/README.md`; it supplies the three mandatory publication environment variables and calls `run-deployment.sh` after each WSL shutdown. After all three deployments, run the following once per experiment directory:

```sh
make eval-v5-final-finalize RUN_DIR=/absolute/path/to/raw/<campaign>/<experiment>
make eval-v5-final-evidence RUN_DIR=/absolute/path/to/raw/<campaign>/<experiment>
```

There is intentionally no ordinary Make target that launches all long experiments. Do not modify code, configs, adapters, datasets, Catalogs, or measured paths between the frozen commit and final sealing.

## Failure and invalid rules

A result-hash, FactSet, Receipt V8, artifact-intent, availability-audit, dataset, Catalog, or frozen-source mismatch fails the affected cell/campaign. Semantic replay with nonzero Business SQL fails. Budget overspend, cross-epoch replay, or a rejected request with a result/AVAILABLE/success audit fails. Unsupported SQL is a fail-closed observation, not equivalence. Product-limit/OOM at 100K rows remains a failure at 100K. Unobserved offered concurrency is invalid. No outlier or slow sample is deleted.

Publication finalization additionally requires exactly three fresh deployments, all required cells, clean measured paths, zero `pswpin/pswpout` deltas, no OOM, no unexpected restart, and complete raw reconstruction. Otherwise status is `incomplete` or `fail`; paper-ready macros are withheld.

## Evidence lifecycle

Mutable run data lives only under `evaluation/final-v5-wsl2/raw/<campaign>/<experiment>/`; each experiment has its own frozen config, raw deployment JSONL, environment/deployment manifests, generated output, and evidence manifest. A passing publication experiment can be sealed only after all three deployments. Pilot data contains `PILOT-NOT-FOR-PUBLICATION`, is never publication eligible, and cannot be sealed. Existing historical evidence directories are outside this lifecycle and remain untouched.

Only after all three deployments pass and the author reviews the sealed pack may the paper be updated. Until then: **Current V5 publication-scale performance remains pending the final frozen WSL2 campaign.**
