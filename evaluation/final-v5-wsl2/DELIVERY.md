# Final V5 WSL2 experiment framework delivery

Implementation branch: `codex/tkde-v5-final-experiment-harness`. The three exact local commit SHAs are reported with the final handoff. The framework adds production-path observational measurement, strict configuration and sample types, environment/preflight scripts, preregistered matrices, runner/adapter entrypoints, an independent trace-union oracle, raw reconstruction, sealing protection, and a tiny non-publication smoke.

- Instrumentation commit: `6ebd3e5`
- Runner/evidence framework commit: `0318af7`
- Protocol documentation commit: the commit containing this delivery file; its exact SHA is reported in the final handoff.

Validation completed in this development session:

- `go test ./... -count=1`
- `go test -race ./internal/resultartifact ./internal/viewcompiler ./internal/control ./internal/gateway ./evaluation/internal/experiment ./evaluation/finalv5oracle -count=1`
- `go vet ./...`
- `git diff --check`
- `make eval-v5-final-validate`
- `make eval-v5-final-smoke`
- `bash -n evaluation/final-v5-wsl2/scripts/*.sh`

The PowerShell controller was inspected but not executed because `pwsh` is unavailable in the development environment. No publication-scale command is authorized by this file.

Current operational boundaries:

- WSL2 Ubuntu 22.04 and a single Gateway instance are the publication claim boundary.
- The unified source-controlled adapter and reproducible build binding are implemented. Only the real baseline/S1/tiny path is complete; scale, artifact, RLS, attack, ProvSQL, compiler, concurrency, and RQ5 implementations remain fail-closed.
- The current product still materializes result rows in memory. A 100K-row failure/OOM remains a failed sample; the harness does not reduce scale or increase limits.
- The 10M/100M profile is opt-in and must remain labeled `kernel_only=true`.
- ProvSQL representation semantics differ from TaskGate and do not support a global winner claim.

Formal publication experiments executed: NO
Paper numeric results updated: NO
Historical evidence relabeled: NO
Measurement instrumentation ready: YES
Experiment orchestration/evidence skeleton ready: YES
Real source-controlled baseline adapter ready: YES
Real baseline Pilot executed: NO
All nine real experiment adapters ready: NO
Formal publication campaign ready to launch: NO
