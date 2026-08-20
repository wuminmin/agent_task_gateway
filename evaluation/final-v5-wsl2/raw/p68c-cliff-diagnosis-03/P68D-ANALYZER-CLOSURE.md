# P68d offline analyzer closure

Classification: `DIAGNOSIS-NOT-FOR-PUBLICATION`. This is an offline analysis of retained P68c bytes. No deployment, campaign, retry, capability change, or evidence-byte rewrite occurred.

## Provenance and analyzer verdict

Before analysis, the retained inputs were rehashed and matched the P68c ledger exactly:

- raw samples: `4622365ea19d1d7170404e0301d78187fc4021d3360895b743608ab1bb6e4622`
- Adapter stderr: `b710a6b90867c075b3a6096e6af338e1975d802e52db52b4dd1d912e45b4a27b`
- cliff observer: `d62c1eb9ce883c4dfa081ba6c8a6bc726ec21950b3d2eabcee2912a116b3f2a4`

The original `diagnosis-failure.json` remains unchanged at SHA-256 `fccbee30d485003dc177316cb35d0bbd52dee5a6c2df5a98b3011e2f4729ddfa`.

The official analyzer completed with exit 0. `analyzer-closure.json` reports 270 measured samples, one explicitly counted and excluded warmup record, 315 operation records, 1,006 observer snapshots, 48 migration timeout records, and a continuous timeout interval at order positions 268--315. The analyzer verdict is `cliff_reproduced`.

The measured sample statuses are 221 pass, 48 fail, and one invalid, so the profile-campaign terminal verdict is `fail`. A top-level v3 acceptance verdict is `not_applicable_by_concurrency_contract`, not pass or fail: all 270 records are concurrency samples, the concurrency terminal contract forbids `taskgate_acceptance_v3`, and the retained input contains zero such records. The 48 root submission timeouts did not reach a v3 finalizer decision.

## Wait decomposition and aligned state

The per-operation `migration-curve.csv` separates the two waits.

- Before the cliff, 222 measured submission waits had no timeout, mean 108.627 ms, p50 105.016 ms, p95 109.023 ms, and max 205.632 ms.
- All 48 tail operations timed out only at the submission wait (`AWAITING_SUBMISSION` to expected `AWAITING_APPROVAL`): mean 30,040.996 ms, p50 30,037.694 ms, p95 30,093.855 ms, and max 30,101.164 ms.
- No tail operation reached the activation wait. Across the preceding 222 measured operations, activation had zero timeouts, mean 128.113 ms, p50 105.157 ms, p95 205.709 ms, and max 919.342 ms.

The suspicious-table row-count curve is `state-curve.csv`. At the observer snapshot aligned immediately before order 268, Control had 39,596 tasks, 39,596 grants, 79,192 approval events, 79,192 callback-idempotency rows, and zero tasks awaiting submission. At order 268, tasks rose to 39,648 while grants stopped at 39,647, approval events and callback-idempotency stopped at 79,294, and awaiting-submission became one. At order 315, tasks reached 39,695 and awaiting-submission reached 48, while grants, approval events, and callback-idempotency remained fixed at those order-268 values.

`correlation.json` aligns each operation with the nearest preceding observer snapshot. The strongest coefficient is `control.tasks_awaiting_submission` at Pearson 0.851919 (315 observations). `control.table.tasks.seq_tuples_read` is 0.818011, but this is not independent causal evidence because the observer itself executes repeated whole-table task counts. Lock waiters peaked at five, observer-identified audit-lock waiters peaked at four, deadlocks stayed at zero, and the tail snapshots normally showed zero waiters. OA RSS was exactly 314,425,344 bytes from the first-timeout alignment through the final alignment (the run-wide maximum occurred earlier at 350,433,280 bytes), so the continuous tail is not accompanied by OA heap growth.

## Root-cause locus

Evidence grade `measured`: the failure boundary is the submitted callback transaction, not activation, Business SQL, or finalization. Once the cliff begins, new root rows remain in `tasks.state='AWAITING_SUBMISSION'`; the committed counts of `callback_idempotency`, `approval_events`, and `task_grants` stop advancing. The direct path is OA asynchronous dispatch (`internal/oademo/server.go:421`), Gateway submitted-callback mapping (`internal/gateway/callback.go:129`), and Control `ApplyApprovalCallback` (`internal/control/callback.go:242`).

Evidence grade `inferred`: the leading code-level hotspot is the single-row `audit_chain_head` critical section. Every task creation and callback transaction appends to the same audit chain; `appendAuditTx` locks the singleton head with `SELECT ... FOR UPDATE` and holds the enclosing transaction through audit insert/head update. A submitted callback that waits there has already staged callback and approval rows but exposes none of them until commit, exactly matching the observed freeze. Thirty-second observer snapshots showing few lock waiters do not measure per-request lock duration and cannot exclude a transient or queued serialization bottleneck. This diagnosis points to `audit_chain_head` and the callback transaction as the first repair target; it does not claim the evidence proves a unique implementation defect.

Evidence grade `inferred, secondary`: unbounded lifecycle retention increases the accumulated workload but is not the immediate tail signature. OA retains every draft in its process map, yet submission accesses a draft by ID and OA RSS plateaus throughout the timeout tail. Control retains tasks, grants, callbacks, and audits indefinitely; primary-key task lookup remains indexed, while the high task sequential-read correlation is confounded by observer count queries.

## Candidate repair points (not implemented)

1. `internal/control/audit.go:18-67` and `internal/control/migrations/001_initial.sql:145-151`: redesign or bound the singleton audit-head serialization while preserving one verifiable audit chain and transaction semantics.
2. `internal/control/callback.go:267-359`: reduce the submitted-callback transaction's time and lock footprint, and add explicit timing around callback claim, task-row lock, audit-head acquisition, and commit before choosing a mechanism change.
3. `internal/control/entities.go:101-182`: add an authorised lifecycle/retention policy for completed experiment task families so cumulative state does not grow without bound; any change must preserve retained audit semantics.
4. `internal/oademo/server.go:55-60,169-203,421-441`: bound draft retention and callback dispatch concurrency/backpressure, with explicit completion/error metrics; this is secondary unless finer timing moves the bottleneck to OA.

No candidate was implemented because P68d authorises analysis and precise repair options only, and explicitly forbids changes under `internal/`.
