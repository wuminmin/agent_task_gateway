# P68c replacement2 diagnostic result

This directory is classified `DIAGNOSIS-NOT-FOR-PUBLICATION`. It is not a
formal campaign, publication evidence, a canary, or a v3 acceptance result.

The authorised replacement deployment used the original reviewed P68 shape:
pilot class, one `concurrency-expense-detail` deployment, one process
replicate, five warmups and thirty measured samples in each of nine cells. It
built the formal Gateway from the clean, pushed
`0faa14a9d654fe45723f5aae314fbe5c5893ea37` tree. The resulting image ID is
`sha256:16af7223926fd0faeeb37a9b6736ec9baca01d88463cf5937244540c81d3912d`;
the formal build manifest SHA-256 is
`f41ee92a07cffc910548f87105750bfa6ef96dc7803c095f0dc7f545a101743a`.
Profile activation completed and the runner entered measurement.

The live diagnostic stream contains 79,342 migration-wait records over all
315 operations: 79,294 waits reached their target and 48 timed out. The 48
timeouts are exactly the consecutive measured order positions 268 through
315. Every timeout is for a root task waiting for `AWAITING_APPROVAL`, with
last state `AWAITING_SUBMISSION`, no last polling error, and elapsed time
between 30,002.611 ms and 30,101.164 ms. The retained raw sample stream covers
all 270 measured positions 46 through 315 and records the same exact 268--315
tail as `real_concurrency_measurement_failed`. This is direct diagnostic
evidence that the cumulative-state cliff was reproduced; it is not an
analyzer verdict.

The raw stream has 271 records rather than the analyzer's closed 270-record
input: in addition to all measured records it retains one invalid warmup at
order position 1 (`offered_concurrency_not_observed`). Measured statuses are
221 pass, 48 fail, and one invalid (order position 194). Total statuses,
including the retained warmup, are 221 pass, 48 fail, and two invalid.

The read-only observer retained 1,006 snapshots from
`2026-08-20T08:01:15.412310653Z` through
`2026-08-20T16:25:20.365079471Z`. Its maxima are 39,695 Control tasks, 48
tasks awaiting submission, five lock waiters, four audit-lock waiters, zero
deadlocks, and 350,433,280 bytes OA RSS. The last snapshot has 39,695 tasks,
39,647 grants, 7,002 active tasks, 32,645 archived tasks, 48 awaiting
submission, zero lock waiters, and zero deadlocks.

After measurement, the official diagnosis analyzer exited with:

```
diagnosis samples are not measured non-publication concurrency records
```

The analyzer rejects every input record with `sample.warmup=true` before its
270-record density check. Consequently it did not create the official
diagnosis summary, migration curve, state curve, or correlation document.
The original launcher-created `diagnosis.json` remains unmodified at
`status=running`; `diagnosis-failure.json` records the later, independently
verified terminal facts without rewriting that first-hand file.

This failure occurred after measurement began, so the task's one permitted
pre-measurement harness repair and restart does not apply. No retry was made
and no code was changed. There is no analyzer closure, v3 acceptance verdict,
or authorization to identify or repair an `internal/` culprit from this run.

The exact locally retained inputs are bound as follows:

- raw samples: 92,651,596 bytes, SHA-256
  `4622365ea19d1d7170404e0301d78187fc4021d3360895b743608ab1bb6e4622`;
- Adapter stderr diagnostics: 49,913,108 bytes, SHA-256
  `b710a6b90867c075b3a6096e6af338e1975d802e52db52b4dd1d912e45b4a27b`;
- cliff observer snapshots: 3,306,407 bytes, SHA-256
  `d62c1eb9ce883c4dfa081ba6c8a6bc726ec21950b3d2eabcee2912a116b3f2a4`.

The raw samples and Adapter stderr remain ignored on the workstation; their
credential scan and compact first-hand configuration, build, activation,
failure, and observer records are committed. After launcher exit, the exact
Compose project had zero containers, zero volumes, and zero networks. No
formal campaign, capability change, frozen-byte change, release, or tag was
performed.
