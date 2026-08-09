# Final-V5 capability/evidence provenance (P2.7)

This is the P2.7 honesty check at commit `a1db29767a65d9c3a494597bc9d0afc1dc3924b1`.
It separates the source-controlled `--capabilities` result from evidence that may
support a publication claim.  A true source-controlled capability means that
`evaluation/cmd/final-v5-adapter/capability.go` can resolve every frozen cell
through its accepted handler or fixture.  It does not mean that the formal
publication campaign has run.

The accounting migration boundary is I1-B,
`e3622a57fe212f8caaa7857bbe39978b3d867927` (the observer began emitting
`ObserverSnapshotV2` authoritatively).  A retained run before that commit cannot
be used as current v3 accounting evidence.  The current source-controlled
answer remains six true capabilities: RLS, adaptive attacks, ProvSQL, compiler,
concurrency, and RQ5; Baseline, Scale, and Artifact remain false.

## Provenance table

| Capability | Source-controlled basis and frozen coverage | Traceable run or retained artifact | Accounting/path conclusion | Publication status and required action |
| --- | --- | --- | --- | --- |
| RLS | `realPublicationCellImplemented("rls", ...)` → `validRLSCell`; six cells (`adaptive-100-v1`, `policy-denied-control` × `rls`, `unlimited`, `bounded`) in `evaluation/cmd/final-v5-adapter/capability.go` | `evaluation/finalv5rls/corpus-v1.json`, `evaluation/final-v5-wsl2/config/rls.example.json`, and `TestRLSAdapterLivePreflight` are source/config/test inputs. No retained complete publication run with a commit-bound result was found. The capability gate was introduced in `59a9ee4462f6f23c5469ddb1feb643bad41e0630`, before I1-B. | Handler support is not evidence that the RLS TaskGate arm reached the current v3 finalizer. RLS is not one of the three P2 runtime-finalizer caller closures. | Not publication evidence. Re-run the complete RLS matrix against the v3 runtime and include it in P5.3/P5.5. |
| Adaptive attacks | `finalv5attack.Load` plus `validAttackMode`; 15 frozen A–E cells in `capability.go` | The related security corpus run is `evaluation/security/raw/evidence-20260722-v1-attack/run.json`: `git_revision=722c9d0f66f47394d799daf4488a7f08b644f7a9`, `exit_code=0`, raw SHA `3d1bd49879cce4a865d3b5b594b9889bfe3da790a5f71d62f147502b443d9c56`. It is a policy/attack corpus run, not the frozen A–E three-deployment capability campaign. The aggregate `evaluation/security/results.json` is `status=partial`, with fuzz below requirement. Both precede I1-B. | This run does not establish v3 cumulative accounting, and it is not interchangeable with the formal adaptive-attack matrix. | Not publication evidence. Re-run the frozen A–E matrix through v3 in P5.5; retain the old corpus only as historical security evidence. |
| ProvSQL | `validProvSQLCell`; nine cells (`nonce-join-group` × `1k`, `10k`, `45k` × `direct`, `provsql`, `taskgate`) | No accepted deployment/publication run for this capability is retained. P2.3 commit `a8cbc800923429d2d2018d7732c09f35b70c9624` moves only the TaskGate arm to v3 and leaves the profile dormant/unroutable. P2.6a's `TestTheProductionProvSQLResolverPreparesEveryValidatedExactFixtureVariant` and `TestExactProvSQLPreparedPairAgainstPostgreSQL` are current source/preparation evidence; the latter uses an explicitly synthetic private binding and is not a live/publication pass. Any earlier green TaskGate result was on the pre-v3 accounting path. | The current v3 source wiring exists, but no v3 deployment measurement has been accepted. The direct and native ProvSQL arms are not TaskGate accounting. | Marked “needs v3 retest.” Run the exact private-bound ProvSQL matrix in P5.2/P5.4; do not reuse the old green result. |
| Compiler | `compilerfixture.IsFrozenCell`; 11 cells (`view-depth`, `join-sources`, `limit-controls`) in `capability.go` | Current post-I1-B source evidence includes the v7 compiler identity and compatibility gate in `d01a35ccddf07e68cb6cfa95ddfe1313a33b9108`, plus the P2.6b exposure rerun at `a1db297` (RQ2/exposure preparation and execution). No retained complete five-process formal compiler campaign is present. | Compiler measurement is latency-only and does not itself prove TaskGate observer accounting; current v7 preparation evidence must not be promoted to a formal capability campaign. | Not publication evidence. Re-run the complete compiler matrix from the frozen measured tree in P5.4. |
| Concurrency | `concurrencyfixture.Lookup`; nine cells (`shared-root` widths 10/50/100/500 with two modes, plus `serial-control`) | `evaluation/security/results.json` records the older concurrency component as passed but has no capability-campaign identity and is pre-I1-B. P2.6b's current exposure rerun records the named live settlement test `TestConcurrentTaskFamilySettlementCannotOverspend` as PASS, but that is not the nine-cell/30-fresh-root publication matrix. | Current v3 settlement has a bounded live check; it does not establish the complete formal concurrency capability or its publication matrix. | Not publication evidence. Re-run the complete fresh-root concurrency matrix in P5.5, including offered-concurrency observation. |
| RQ5 | `rq5fixture.IsCell` for both `build` and `retained` modes across the required cycles | `evaluation/v5-outcome/evidence.json` is traceable: implementation base `030c1f6c42abf67dac3fb512ca7206bf694827ec`, submission `85885a542cf5aa0968b1d22c71ea3562a5b07406`, raw log SHA `c5281414953cb4871f50303cbd11dc305e6acd4017ce37af63b010958468e706`, 11 tests passed and 0 skipped. Both commits predate I1-B. | The evidence is a historical V5 receipt/artifact recovery run, not evidence that the current v3 runtime finalizer accepted the RQ5 cycles. | Marked “needs v3 retest.” Re-run the build/retained cycles from the current v3 measured tree in P5.5. |

## Decision

The six `true` values are retained as source-controlled implementation coverage,
not promoted to publication claims. The pre-I1-B rows (adaptive attacks,
ProvSQL's earlier TaskGate evidence, and RQ5; RLS has no qualifying retained run)
remain explicitly due for v3 retest. Compiler and concurrency have current
post-I1-B supporting checks, but still lack their complete formal publication
campaigns. No capability, registry, activation state, contract release, tag, or
evidence byte is changed by this table. Formal build, N4, 100×4 canary, campaign,
and v1.5 freeze remain outside P2.7 and were not run.
