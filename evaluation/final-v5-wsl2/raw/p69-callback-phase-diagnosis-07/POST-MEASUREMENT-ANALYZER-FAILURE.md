# P69 -07 post-measurement analyzer failure

This directory is classified `DIAGNOSIS-NOT-FOR-PUBLICATION`. It is not a
formal campaign, publication evidence, a canary, or a v3 acceptance result.

The single additional pre-measurement restart granted after the `-05`
environment failure ran as `p69-callback-phase-diagnosis-07` from the clean,
pushed `07ad8dd12ae3875cbb674e58089cfcafb2373d6b` tree, using the task-local
Docker CLI configuration bound to the native Buildx/Compose executables, the
P45 private binding (`3bb2771fa07b3cd7b0e0d806cf84af41d05628b958f425310368b854b77b7526`,
mode 0600), and the closed P68 shape: pilot class, one
`concurrency-expense-detail` deployment, one process replicate, five warmups
and thirty measured samples in each of nine cells, four-phase callback timing
gate enabled. The formal Gateway image is
`sha256:d53e12a06d8b3e26502040870cd957d8d1ae5c4527b7e5214e30a6d31bf94799`
(build manifest `56de3d0f94e24202af69a4a88d68c6cde769295e0c94f65ea276521cf9efd074`).
Profile activation completed, the P69 timing marker reached the Gateway, and
the runner entered measurement.

## Measurement completed and is retained

The live diagnostic stream contains 78,841 migration-wait records over all
315 operations: 78,791 reached their target and 50 timed out. The 50
timeouts are exactly the consecutive measured order positions 266 through
315 (P68c: 268 through 315). Every timeout is for a root task waiting for
`AWAITING_APPROVAL`, with last state `AWAITING_SUBMISSION`, no last polling
error, and elapsed time of about 30,000 to 30,100 ms; the first timeout was
observed at `2026-08-23T06:51:47Z`, the last at `2026-08-23T07:16:19Z`. The
retained raw stream has 272 records: all 270 measured positions 46 through
315 (220 pass, 50 fail at exactly 266 through 315) plus two invalid warmup
records. The observer retained 1006 snapshots; Control `tasks_awaiting_submission`
rose from 0 to 33 between 06:38 and 07:08 while task growth froze near 39,400,
with zero lock waiters and zero deadlocks throughout. The cumulative-state
cliff was therefore reproduced a third time. This is direct diagnostic
evidence, not an analyzer verdict.

## Why the analyzer failed

The official analyzer exited 1 before any summary with the exact message
`callback phase data covers 266 operations, want 315`. The retained Gateway
log holds 39,396 `taskgate-control-submitted-callback-phase-timing-v1`
records for 39,396 distinct tasks, every one with `final_result=committed`;
the last record is at `2026-08-23T06:51:16Z`, after which no submitted
callback completed until teardown. Operations 1 through 266 are fully
covered; operations 267 through 315 -- one root task each, exactly the
wedged tasks -- have no phase record at all.

This is a property of the instrumentation, not of the capture: the log
begins at the Gateway's first line, and `internal/control/callback_phase_timing.go`
emits one record per callback only from `finish()`, i.e. when the callback
transaction commits, replays, or errors. A callback that hangs inside a
phase never reaches `finish()` and emits nothing. The analyzer's verdict in
turn attributes the stuck phase from the phase durations of the timed-out
operations themselves, so with zero records for those operations it cannot
produce a `stuck_phase` even if the coverage closure were relaxed.

The completed records carry no precursor: across all 39,396 callbacks the
maximum durations are `callback_claim` 1.4 ms, `task_row_lock` 1.4 ms,
`audit_chain_head` 8.6 ms and `commit` 190.3 ms, and the last minutes before
06:51 stay in single-digit milliseconds. The wedge is abrupt. The retained
data therefore cannot say whether the 49 missing callbacks hung inside a
Gateway phase or never reached the Gateway.

## What was and was not done

- `runner_status=1` is the expected terminal shape when the cliff reproduces;
  `analyzer_status=1` is the failure recorded here. `timeouts` and
  `stuck_phase` are `unavailable` because no gate summary was written.
- No retained byte was rewritten; `diagnosis.json` keeps `status=running`
  and this typed failure is recorded in `diagnosis-failure.json`.
- The launcher's credential gates on the Adapter stderr and the Gateway phase
  log both passed (17 sensitive values, all five gates 0); the compact files
  committed from this directory were scanned again against every sensitive
  `.env` value (see `credential-audit.json`).
- Large first-hand bytes stay ignored on this machine and were copied to
  `/mnt/d/wsl-data/taskgate-evidence/p69-callback-phase-diagnosis-07/` with a
  SHA-256 manifest: raw samples `9eb4431c43b6e244f185ab688f63acd9f163fe979d672f0471528a25d9b65ee8`
  (91,157,660 bytes), Adapter stderr `ab4d45bde91d2cfabf01f4f1ab8c950682f2ebce469d195b520efe6de017da2e`
  (57,560,226 bytes), Gateway phase log `161aeb6912aa97fa52fef3e1a506e894ed846e9bbbafdd812b4711ab31ca218d`
  (36,418,457 bytes), observer `e62414666dc55dd55ad007aadb7399cadf6cc4bc449d07ee54c8b10a263fa75f`
  (3,305,886 bytes).
- Exact Compose project `taskgate-final-v5-deployment-01-41d2c86fdbd2e511df06`
  containers/volumes/networks after exit: 0/0/0.
- No `internal/`, runner, harness, contract, frozen byte, capability, formal
  campaign, or tag was changed. Per the handoff rules a post-measurement
  failure stops here; the next step is an author decision.

## Open decision for the author

The measurement has been reproduced three times and is retained. Producing
a `stuck_phase` needs one of: (a) an `internal/` instrumentation change that
also emits in-flight traces on timeout, cancellation, or shutdown with the
current phase marked; (b) an evaluation-only analyzer change that stops
requiring records for operations whose callback never completed and reports
the last completed phase boundary instead; (c) a different probe of whether
the wedged callbacks reached the Gateway at all (e.g. OA-side callback
dispatch evidence). None of these is authorized by this task.
