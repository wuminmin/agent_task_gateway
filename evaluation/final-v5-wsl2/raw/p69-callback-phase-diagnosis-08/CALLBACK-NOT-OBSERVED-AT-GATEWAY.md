# P69 -08 diagnosis closure: callback not observed at Gateway

This directory is classified `DIAGNOSIS-NOT-FOR-PUBLICATION`. It is not a
formal campaign, publication evidence, a canary, or a v3 acceptance result.

`p69-callback-phase-diagnosis-08` ran from the clean, pushed
`b91f0d271ad0856dad0a019273c41ea31faafff8` tree (P70 in-flight trace
instrumentation), same closed P68 shape, P45 binding
`3bb2771fa07b3cd7b0e0d806cf84af41d05628b958f425310368b854b77b7526`, formal
Gateway image `sha256:dcce4ba565060a5c19212894386d0f2c397d062a82d0914ae3707d3a55a94297`.
The launcher closed the round: runner status 1 (expected when the cliff
reproduces), analyzer status 0, credential gates pass, exact project
containers/volumes/networks 0/0/0.

## Measurement

78,946 migration-wait records over 315 operations: 79,902 reached, 44 timed
out, all at consecutive measured order positions 272 through 315, every one a
root task waiting for `AWAITING_APPROVAL` in last state `AWAITING_SUBMISSION`
with no polling error; first timeout `2026-08-23T17:46:55Z`, last
`2026-08-23T18:08:27Z`. Retained raw samples: 271 records, 270 measured
(222 pass, 44 fail, 4 invalid) plus one excluded warmup. Observer: 1,005
snapshots; Control tasks 39,596 at 17:38:50 with 0 awaiting submission,
39,966 / 15 at 17:53:50, 39,995 / 44 at 18:08:20; lock waiters and deadlocks 0
throughout. The cumulative-state cliff was therefore reproduced a fourth
time (`-07`: positions 266-315, 50 timeouts).

## What the new instrumentation showed

- Gateway phase records: 39,951, one per completed submitted callback, all
  `final_result=committed`; last record `17:46:11Z`, i.e. the Gateway stopped
  receiving submitted callbacks at the wedge. **Zero in-flight snapshots**
  were ever emitted: at no point was any submitted-callback transaction open
  for 10 s or more, and the 30 s pool snapshots (920 of them) never showed a
  trace in flight at the sample instant (max 1, oldest 4.6 ms).
- Control pool: max 6 of 32 connections in use, 0 pool waits, 0 exhausted
  samples. Handlers were not blocked on pool acquisition either.
- OA container log: one line (`oa demo listening`) for the whole run, zero
  `OA callback failed` entries. OA's dispatcher logs every failed attempt
  after its 5 s client timeout, so the 44 missing callbacks were never sent
  and never failed: they were never attempted.

Official analyzer verdict: `callback_not_observed_at_gateway`
(`stuck_phase=not_observed_at_gateway`, attribution
`no_gateway_record_for_timed_out_callbacks`, `unobserved_timeout_operations=44`,
`in_flight_timeout_operations=0`, pre-cliff phase p95 41.3 ms).

## Interpretation (inference, not proof)

The `audit_chain_head FOR UPDATE` hypothesis from P68d is now contradicted by
direct observation: no callback is stuck inside a Gateway phase, and the
Gateway is idle while tasks wedge. The stall sits upstream of the Gateway's
callback handler, in the segment adapter OA-submit -> OA `submitDraft` ->
OA `dispatch` -> OA `sendCallback`. The retained evidence cannot yet
distinguish (i) the adapter's submit request not producing a `submitted`
dispatch, from (ii) OA accepting the submit but never dispatching. Neither
OA's submit handler outcome nor the adapter's OA submit HTTP status is
recorded per task today.

## Retained bytes

Large first-hand files stay ignored and are copied to
`/mnt/d/wsl-data/taskgate-evidence/p69-callback-phase-diagnosis-08/` with a
SHA-256 manifest: raw samples `9856083ff8721f1fd1ff20feddec767811e43869179c045b61353ef97bdb7ed6`
(93,508,779 bytes), Adapter stderr `64a46491526739c9726f244f64b3ab71e38db21aa622db66512f76c6b6cb7b7e`
(58,367,387), Gateway phase log `b9fb9dc0dbaee2401b3da3339cd9ecb5bb01102cfb3ac29c53fdbb21ad8a8a76`
(37,261,122), observer `44f87cb3e82f1c28e629176701be5683f972e9b93aecbc036e10a26ef292e24f`
(3,302,667). The Gateway log was captured before the stack was stopped, so
the shutdown dump (which would carry only a pool snapshot here) is not in it.
No `internal/`, runner, contract, frozen byte, capability, formal campaign,
or tag was changed by this round; the next step is an author decision on
OA-side / adapter-side observation.
