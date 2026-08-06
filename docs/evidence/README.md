# Committed harness evidence

Records about the repository's own verification, retained so a reviewer holding
the commit can check what a run actually proved rather than what its exit code
said.

This is **not** campaign evidence. Frozen publication campaigns are sealed into
`evaluation/final-v5-wsl2/evidence/`, which stays empty until an author completes
and validates one.

| File | What it records |
| --- | --- |
| `dbtest-suite-<commit>.json` | One complete DSN-enabled `go test -json` run, summarized and accepted by `evaluation/cmd/final-v5-dbtest-report`. |

## What `"accepted": true` means, and what it does not

    accepted = zero failed packages
             + zero failed tests
             + zero UNDECLARED skips
             within the harness the run was executed on

It does **not** mean the v1.5 qualification is complete. A declared skip is a
scheduled debt, not a waiver: every one carries a `scope` (the harness it could
not run on), a `deferred_until` milestone, and either the evidence that already
covers it or the external gate that must eventually run it. The report's
`outstanding_obligations` groups them by milestone, and that list is what a later
session reads to know what acceptance here did *not* establish.

## The current record is development qualification

`dbtest-suite-5cac17e.json` names commit `5cac17e`, which is the code the suite
ran against; the harness fixes that made four of those tests execute at all
landed in the commit that retained it. That is deliberate and does not need
rerunning — the runtime cutover requires a fresh full suite anyway.

Treat it as **Phase-0 development qualification**. The report that supports
`V3 RUNTIME INTEGRATION PASS` must come from a clean published integrated commit
and additionally bind `HEAD == origin`, a clean worktree, the complete
tracked-tree source-manifest SHA-256, the exact test command and the allowlist
digest.

## Deferral schedule

| Due before | Tests |
| --- | --- |
| already satisfied at this HEAD | the two `finalv5sqlcheck` probe-equivalence tests, covered by `run-sql-executability-gate.sh` against a disposable empty PostgreSQL 16.14 |
| `V3 RUNTIME INTEGRATION PASS` | `TestProvSQLLiveExternalPair`, in its own `compose.provsql.yaml` project |
| the Result-heavy 100x4 v3 canary | the three `experiment` formal-window live gates |
| contracts v1.5 freeze | `TestAttackAdapterLivePreflight`, `TestRLSAdapterLivePreflight` |
| `targeted_run_eligible` becoming true, after v1.5 freeze | the four `final-v5-activation-support` tests |
