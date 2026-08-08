# Committed harness evidence

Records about the repository's own verification, retained so a reviewer holding
the commit can check what a run actually proved rather than what its exit code
said.

This is **not** campaign evidence. Frozen publication campaigns are sealed into
`evaluation/final-v5-wsl2/evidence/`, which stays empty until an author completes
and validates one.

| File | What it records |
| --- | --- |
| `dbtest-suite-<commit>.json` | One complete DSN-enabled `go test -json` run, summarized and accepted by `evaluation/cmd/final-v5-dbtest-report`. Newest first: `e406536`, then `5cac17e`. |

## What `"accepted": true` means, and what it does not

    accepted = zero failed packages
             + zero failed tests
             + zero UNDECLARED skips
             + zero UNMATCHED allowances
             within the harness the run was executed on

It does **not** mean the v1.5 qualification is complete. A declared skip is a
scheduled debt, not a waiver: every one carries a `scope` (the harness it could
not run on), a `deferred_until` milestone, and either the evidence that already
covers it or the external gate that must eventually run it. The report's
`outstanding_obligations` groups them by milestone, and that list is what a later
session reads to know what acceptance here did *not* establish. The v2 report
also rejects an allowance that matched no observed skip, so a state flip cannot
leave a silent exception behind. The retained v1 reports preserve their
historical semantics and are not rewritten.

## The current record is development qualification

`dbtest-suite-e406536.json` names commit `e406536`, the QueryExecutionBindingV2
and Query Receipt V10 persistence commit, and unlike the record before it that
commit is the code the suite actually ran against: the tree was committed first
and the suite run afterwards, so the name and the run agree without a caveat.

`dbtest-suite-5cac17e.json` is retained beside it. It names commit `5cac17e`,
which is the code that suite ran against; the harness fixes that made four of
those tests execute at all landed in the commit that retained it. That mismatch
is why it was never rerun — the runtime cutover requires a fresh full suite
anyway.

Treat both as **Phase-0 development qualification**. The report that supports
`V3 RUNTIME INTEGRATION PASS` must come from a clean published integrated commit
and additionally bind `HEAD == origin`, a clean worktree, the complete
tracked-tree source-manifest SHA-256, the exact test command and the allowlist
digest. `e406536` binds none of those, and it was taken before the Gateway
delegates preparation to `physicalquery.Prepare`, so the V10 receipts it accepts
carry a synthesized preparation rather than one the Gateway produced.

## Deferral schedule

| Due before | Tests |
| --- | --- |
| already satisfied at this HEAD | the two `finalv5sqlcheck` probe-equivalence tests, covered by `run-sql-executability-gate.sh` against a disposable empty PostgreSQL 16.14 |
| `V3 RUNTIME INTEGRATION PASS` | `TestProvSQLLiveExternalPair`, in its own `compose.provsql.yaml` project |
| the Result-heavy 100x4 v3 canary | the three `experiment` formal-window live gates |
| contracts v1.5 freeze | `TestAttackAdapterLivePreflight`, `TestRLSAdapterLivePreflight`; `TestRegistryClaimsNoSupportWithoutAManifest` becomes runnable when P4 removes the v1.4 manifest |

The four positive `final-v5-activation-support` tests are no longer allowances:
the v1.4 manifest exists and they run in the two-server suite. Their 2026-08-08
fresh-live qualification remains recorded below as historical evidence.

## v1.4 fresh-live raw evidence inventory

The 2026-08-08 fresh-live rerun retained its body outside Git at
`/home/wmm/taskgate-final-v14-live.MDIXO55U`. At inventory time it contained 93
regular files with 8,840,681,978 total content bytes and no empty directories.
The repository retains
`final-v5-v14-live-raw-evidence.sha256`, a relative-path SHA-256 inventory of
every file. The inventory itself has SHA-256
`0752539b2318f831be20aba84ad7f0b9c43940c5909a7c576734a7ebfe2f3090`.

To detect loss or mutation, run `sha256sum -c` against that committed inventory
from the retained directory. This index authenticates the retained bytes but is
not a backup; the 8.3 GiB evidence body remains outside the repository.
