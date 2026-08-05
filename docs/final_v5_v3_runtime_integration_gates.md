# Final-V5 v3 runtime integration gates

## Purpose

This document is the authoritative acceptance criterion for the v3 runtime
integration: the migration that makes `FinalizeObservationV3` the sole
acceptance authority for TaskGate observations and removes the v1.4 accounting
from the active runtime.

It exists because the list was previously referenced by
`docs/final_v5_artifact_autonomous_status.md` and defined nowhere. An acceptance
criterion for publication evidence that lives only in a session's working memory
is not an acceptance criterion; a reviewer holding the commit could not check
what "gate 22" meant, and neither could a later session.

**All thirty gates are mandatory before the v3 canary is publication-grade.** A
Result-heavy 100x4 diagnosis run whose gates are not all passing is a
diagnosis-only measurement and must not be cited as evidence for the v3
accounting, for the Artifact capability, or for any numeric cell in the paper.
Numbering is stable and IDs are never reused or renumbered.

At v1.5 freeze this document gains a machine-readable equivalent whose digest is
indexed in the contract release, so the gate set becomes checkable by digest
rather than by reading prose.

## Status vocabulary

| Status | Meaning |
| --- | --- |
| `PASS` | Required evidence exists and is green at this HEAD. |
| `OPEN` | Requirement is stated; evidence does not yet exist. |
| `UNSPECIFIED` | **The requirement text was not supplied.** See the gap notice. |

## Gap notice — 24 of 30 requirements were not supplied

The instruction that established this document stated that the authoritative
30-item list was supplied with it and was to be committed verbatim. The material
actually supplied contained the requirement text for gates **18, 19, 21, 22 and
25** only. Gate **1** is recoverable from the continuation record, which
describes it as "observer emits strict v2 JSON — done in I1-B".

The remaining twenty-four requirements — 2–17, 20, 23, 24 and 26–30 — were not
supplied in any form, and no revision of this repository has ever contained
them: `git rev-list --all` grepped for the list returns only the continuation
record's own summary line, which enumerates *which* IDs are covered without
saying what any of them require.

They are therefore recorded below as `UNSPECIFIED` rather than invented.
Reconstructing an acceptance criterion by guessing what a gate probably meant
would produce exactly the defect this arc exists to remove — a check that agrees
with whoever wrote it rather than testing an independent claim — and a plausible
wrong gate is worse than a visibly absent one, because it reads as coverage.

The continuation record's claim that gates 2–17, 20, 23, 24 and 26–30 are
"already covered by tests" is retained here as an unverified prior claim. It
cannot be confirmed while the requirement text is missing: a test cannot be
matched to a requirement nobody can read.

**To close this gap:** supply the 24 requirement texts, and they will be filled
in against their existing IDs without renumbering.

## The gates

| # | Requirement | Required evidence / test symbol | Status |
| --- | --- | --- | --- |
| 1 | The out-of-process observer emits strict `ObserverSnapshotV2` JSON and has no v1 fallback path. | `main.TestCollectEmitsStrictObserverSnapshotV2`; `experiment.ObserverSnapshotV2.Validate` | PASS (I1-B, `e3622a5`) |
| 2 | *Not supplied.* | — | UNSPECIFIED |
| 3 | *Not supplied.* | — | UNSPECIFIED |
| 4 | *Not supplied.* | — | UNSPECIFIED |
| 5 | *Not supplied.* | — | UNSPECIFIED |
| 6 | *Not supplied.* | — | UNSPECIFIED |
| 7 | *Not supplied.* | — | UNSPECIFIED |
| 8 | *Not supplied.* | — | UNSPECIFIED |
| 9 | *Not supplied.* | — | UNSPECIFIED |
| 10 | *Not supplied.* | — | UNSPECIFIED |
| 11 | *Not supplied.* | — | UNSPECIFIED |
| 12 | *Not supplied.* | — | UNSPECIFIED |
| 13 | *Not supplied.* | — | UNSPECIFIED |
| 14 | *Not supplied.* | — | UNSPECIFIED |
| 15 | *Not supplied.* | — | UNSPECIFIED |
| 16 | *Not supplied.* | — | UNSPECIFIED |
| 17 | *Not supplied.* | — | UNSPECIFIED |
| 18 | **Wrong visible target.** Independently mutate the visible target's exact digest, strict AST digest, row limit, prepared target binding and role. Every mutation must fail finalization. | `experiment.TestGate18WrongVisibleTargetFailsFinalization` | OPEN |
| 19 | **Wrong companion target.** Perform the same five mutations independently on the companion target. Every mutation must fail finalization. | `experiment.TestGate19WrongCompanionTargetFailsFinalization` | OPEN |
| 20 | *Not supplied.* | — | UNSPECIFIED |
| 21 | **Semantic replay.** A verified V9 receipt with `path_kind=semantic_replay` must carry targets `authorized=true, executed=false`, observer visible and companion deltas of 0, and any target execution must fail finalization. | `experiment.TestGate21SemanticReplayAuthorizesWithoutExecuting` | OPEN |
| 22 | **Idempotent replay.** The original receipt is returned byte-for-byte, no new execution binding row is written, the observer Business delta is 0, and any statement at all must fail finalization. | `experiment.TestGate22IdempotentReplayReturnsOriginalReceiptByteForByte` | OPEN |
| 23 | *Not supplied.* | — | UNSPECIFIED |
| 24 | *Not supplied.* | — | UNSPECIFIED |
| 25 | **Adapter verdict is never consulted.** Given evidence in which the Adapter claims `pass` while carrying a bad plan, a bad target or a bad delta, the production finalizer must reject it without reference to the claimed verdict. | `experiment.TestGate25AdapterVerdictIsNeverConsulted` | OPEN |
| 26 | *Not supplied.* | — | UNSPECIFIED |
| 27 | *Not supplied.* | — | UNSPECIFIED |
| 28 | *Not supplied.* | — | UNSPECIFIED |
| 29 | *Not supplied.* | — | UNSPECIFIED |
| 30 | *Not supplied.* | — | UNSPECIFIED |

## Structural gates

These are not numbered gates. They are the standing structural conditions the
migration must hold at every commit after it lands, checked by source and
call-graph tests rather than by measurement.

| Condition | Test symbol |
| --- | --- |
| No active package references the v1.4 accounting types or functions. | `experiment.TestNoActiveReferenceToV14Accounting` |
| `evaluation/internal/legacyv14` is imported only by legacy tools and rejection tests. | `experiment.TestLegacyV14IsImportedOnlyByLegacyToolsAndRejectionTests` |
| The `FinalizeObservationV3` production wrapper has real non-test callers from all three TaskGate paths. | `experiment.TestFinalizeObservationV3HasProductionCallers` |

## Canary prerequisite

The Result-heavy 100x4 diagnosis-only v3 canary must not run until every one of
the following holds:

1. all 30 gates pass;
2. the full DSN-enabled suite passes;
3. the v1.4 active symbols are unreachable;
4. the finalizer production wrapper has real callers;
5. Artifact, Scale and ProvSQL all use v3;
6. the worktree is clean;
7. HEAD equals origin.

Condition 1 is currently unsatisfiable for the reason given in the gap notice.
