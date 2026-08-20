# P68 pre-measurement failure

This directory is classified `DIAGNOSIS-NOT-FOR-PUBLICATION`. It is not a
formal campaign, publication evidence, a canary, or a v3 acceptance result.

The one authorized deployment built and started, but the diagnostic observer
exited before its first snapshot and before the runner started. The exact
observer error is retained in
`deployments/concurrency-expense-detail/001/cliff-observer.log`:

```
capture Business: can't scan into dest[4] (col: idx_scan): cannot scan NULL into *int64
```

The empty `cliff-observer.jsonl` has the SHA-256 of an empty file. No migration
records, samples, live-gate results, or acceptance decisions were produced.
Accordingly, this run neither reproduces nor disproves the P67 cliff.

The cause is confined to the newly added harness observer: PostgreSQL can
report an initial `NULL` for `pg_stat_user_tables.idx_scan`, while the observer
scanned it into `int64`. The follow-up source change normalizes all five
nullable table-stat counters with `COALESCE(..., 0)` and adds a focused test.
That correction was not live-retested because the task authorizes only one
diagnostic deployment.

After the launcher exited, the exact Compose project was checked and had zero
containers, zero volumes, and zero networks. The ignored local evidence tree,
including generated snapshot-index inputs, remains intact on the workstation;
the compact failure records, build/runtime identity, logs, and configuration
needed to audit this failure are committed with the ledger entry.
