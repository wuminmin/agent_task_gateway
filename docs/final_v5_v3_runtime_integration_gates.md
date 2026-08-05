# Final-V5 v3 runtime integration gates

## Purpose

This document is the authoritative acceptance criterion for the v3 runtime
integration: the migration that makes `FinalizeTaskGateObservationV3` the sole
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
| `BLOCKED` | Requirement cannot be met without an author decision, and the blocker is pinned by a test. |

The earlier gap notice, which recorded twenty-four requirements as
`UNSPECIFIED`, is **resolved**: the missing text was supplied authoritatively by
author decision and is transcribed below. No requirement in this document is
inferred.

A gate is marked `PASS` only where an existing test was read and found to check
the stated semantics, not merely to have a plausible name. Where the semantics
differ, or where only part of a requirement is covered, the gate stays `OPEN`
and a focused test is owed.

Gates 18, 19 and 22 each found a real defect while being written; those are
recorded with the gate rather than silently fixed.

## The gates

| # | Requirement | Required evidence / test symbol | Status |
| --- | --- | --- | --- |
| 1 | The out-of-process observer emits strict `ObserverSnapshotV2` JSON and has no v1 fallback path. | `main.TestCollectEmitsStrictObserverSnapshotV2` | PASS |
| 2 | **Snapshot-v1 rejection.** A schema-v1 `ObserverSnapshot` must be rejected by the v1.5/v3 runtime path. No fallback to v1 is permitted. | `experiment.TestV1EvidenceIsRefusedOnTheV3Path` | PASS |
| 3 | **Observer-window identity mismatch.** `before.observer_window_id != after.observer_window_id` must fail. | `experiment.TestWindowIdentitiesMustMatchAcrossThePair/observer_window_id` | PASS |
| 4 | **Classifier-manifest identity mismatch.** Before/after, Adapter, observer and finalizer classifier-manifest digests must agree exactly. Any mismatch must fail. | `experiment.TestWindowIdentitiesMustMatchAcrossThePair/classifier_manifest` (before/after, observer) + `experiment.TestFinalizerRejectsEveryAdapterDisagreement/a_substituted_classifier_manifest` (Adapter vs finalizer) | PASS |
| 5 | **Operation-binding identity mismatch.** Before/after, carried and independently derived operation-binding digests must agree exactly. Any mismatch must fail. | `experiment.TestWindowIdentitiesMustMatchAcrossThePair/operation_binding` + `experiment.TestFinalizerRejectsEveryAdapterDisagreement/a_different_operation_id` + `experiment.TestBindingDigestCoversTheWholeOperation` | PASS |
| 6 | **Atomic role-total invariant.** Observer role-wide total must equal the sum of structural rows from the same snapshot row set. Any disagreement must fail. | `experiment.TestSnapshotRejectsATotalThatDisagreesWithItsRows` + `experiment.TestUnaccountedRoleCallFails` | PASS |
| 7 | **Counter regression or disappearing row.** Any cumulative-call regression, disappearing structural key or impossible before/after transition must fail. | `experiment.TestWindowRejectsCountsGoingBackwards` (regression) + `experiment.TestGate7DisappearingStructuralKeyFails` (disappearance) | PASS |
| 8 | **`pg_stat_statements` reset mutation.** `stats_reset` changing inside the observer window must fail. | `experiment.TestWindowBindsItsInvariants/pg_stat_statements_reset` | PASS |
| 9 | **`pg_stat_statements` eviction mutation.** `dealloc` changing, or non-zero `dealloc` proving possible eviction, must fail. | `experiment.TestWindowBindsItsInvariants/entries_evicted` + the non-zero `dealloc` guard in `ObserverWindowV2.Delta` | PASS |
| 10 | **Measurement-environment mutation.** PostgreSQL version, `track`, `track_utility` or `track_planning` drift must fail. | `experiment.TestWindowBindsItsInvariants/measurement_environment` + `experiment.MeasurementEnvironment.Validate` | PASS |
| 11 | **PostgreSQL runtime mutation.** Any immutable PostgreSQL image, container, platform or server-identity mismatch must fail. | `experiment.TestWindowBindsItsInvariants/PostgreSQL_image`, `/PostgreSQL_restart` + `experiment.TestFinalizerRejectsAWindowFromAnotherRuntime` + `experiment.TestRuntimeIdentityRejectsIncompleteBindings` | PASS |
| 12 | **Gateway runtime/source mutation.** Any Gateway commit, context, source manifest, image, binary, container or formal-build identity mismatch must fail. | `experiment.TestWindowBindsItsInvariants/gateway_image`, `/gateway_source`, `/gateway_binary`, `/gateway_restart`, `/gateway_OOM` | PASS |
| 13 | **Formal-healthcheck mutation.** Any approved `/health/live` command, interval, timeout or retries mismatch must fail. | `experiment.TestFormalHealthcheckAcceptsOnlyTheApprovedDefinition` (command, interval, timeout, retries) + `experiment.TestHealthcheckIdentitySeparatesEveryDimension` + `experiment.TestWindowBindsItsInvariants/healthcheck_command` | PASS |
| 14 | **Same-total control-class substitution.** Missing one required control plus an extra different control at the same total must fail. | `experiment.TestSameTotalControlSubstitutionFails` | PASS |
| 15 | **Same-total PostgreSQL-internal-key substitution.** Missing one qualified internal key plus another key at the same total must fail. | `experiment.TestSameTotalInternalSubstitutionFails` | PASS |
| 16 | **Missing required control.** Any control class or internal key below exact expected multiplicity fails. | `experiment.TestMissingAndExtraControlsBothFail` + `experiment.TestCompiledInternalKeysMatchTheQualification` | PASS |
| 17 | **Extra or unexpected statement.** Any class/key above expected multiplicity or absent from the classifier fails. | `experiment.TestUnexpectedStatementFailsClosed` + `experiment.TestMissingAndExtraControlsBothFail` | PASS |
| 18 | **Wrong visible target.** Independently mutate the visible target's prepared binding, exact digest, strict digest, row limit, role and contract identity. Every mutation must fail finalization. | `experiment.TestGate18WrongVisibleTargetFailsFinalization` + `experiment.TestGate18And19RejectAnotherContractIdentityForATarget` | PASS |
| 19 | **Wrong companion target.** Perform the same six mutations independently on the companion target. Every mutation must fail finalization. | `experiment.TestGate19WrongCompanionTargetFailsFinalization` + `experiment.TestGate18And19RejectAnotherContractIdentityForATarget` | PASS |
| 20 | **Another workload's target.** A target belonging to another operation/cell/workload cannot classify for this operation. | `experiment.TestAnotherWorkloadsTargetIsRefused` + `experiment.TestCompilationFreezesTheTable` | PASS |
| 21 | **Semantic replay.** Targets `authorized=true`/`executed=false`; zero visible and companion delta; any `executed=true` or target statement must fail. | `experiment.TestGate21SemanticReplayAuthorizesWithoutExecuting` | PASS |
| 22 | **Idempotent replay.** The original persisted V9 receipt document is returned unchanged; no new query, binding or reservation; zero Business delta; any Business statement must fail. | `experiment.TestGate22IsBlockedByAConflictBetweenTwoV3Invariants` (pins the blocker) + `experiment.TestIdempotentReplayEvidenceRequiresEveryMember` (wrapper half) | **BLOCKED** |
| 23 | **Adapter-supplied wrong plan.** A carried plan differing from independent finalizer derivation fails. | `experiment.TestFinalizerRejectsEveryAdapterDisagreement/a_plan_claiming_another_path`, `/an_edited_internal_expectation` | PASS |
| 24 | **Adapter-supplied wrong manifest.** A carried manifest or binding differing from independent derivation fails. | `experiment.TestFinalizerRejectsEveryAdapterDisagreement/a_substituted_classifier_manifest`, `/a_substituted_classifier_binding` | PASS |
| 25 | **Adapter verdict.** The Adapter claims `pass` while the evidence carries a bad plan, target or delta; the finalizer rejects it because no Adapter verdict has acceptance authority. | `experiment.TestGate25AdapterVerdictIsNeverConsulted` | PASS |
| 26 | **Baseline arm manufactures observer evidence.** Direct PostgreSQL, native ProvSQL and empty/control arms reject TaskGate observer evidence. | `experiment.TestBaselineArmsCannotCarryObserverEvidence` | PASS |
| 27 | **SQL-bearing observer output.** No raw, normalized, base64 SQL or SQL-bearing parser object may enter observer JSON, `Sample`, logs or durable evidence. | `main.TestEmittedSnapshotCarriesNoSQLBearingField` + `experiment.TestClassifierManifestContainsNoSQL` + `queryreceipt.TestV9CarriesNoSQL` | PASS |
| 28 | **SQL leak through errors.** Errors may reveal only safe codes and deployment-local `queryid`, never SQL. | `main.TestParserFailuresDoNotLeakSQL` | PASS |
| 29 | **Legacy v1.4 accounting rejected.** v1.4/v2 accounting evidence cannot satisfy v1.5/v3 acceptance. | `experiment.TestGate29LegacyV14AccountingCannotSatisfyV3` | PASS |
| 30 | **Binding-digest mutation.** Mutation of any window, operation, manifest, classifier, target, execution, pre-state, receipt, runtime, image or footprint digest fails. | `experiment.TestBindingDigestCoversTheWholeOperation` + `experiment.TestSnapshotDigestCoversRuntimeAndResourceEvidence` + `experiment.TestRequireBindsEveryQualificationCondition` + `querybinding.QueryExecutionBindingV1.Validate` digest recomputation | PASS |

Twenty-nine gates PASS at this HEAD. **Gate 22 is BLOCKED** on an author
decision, not on work.

### Gate 22 is blocked by a conflict between two v3 invariants

The v3 model as it stands cannot finalize an exact request-ID replay at all,
because two of its own rules contradict each other on that path:

- `CompileClassifier` presence-couples attestation in both directions. A path
  performing no Attestation -- `idempotent_replay` returns before
  `datasourceEvidence` and reaches Business PostgreSQL not at all -- must name
  neither an ExpectedSchema nor a qualified footprint, and an internal manifest
  entry naming a qualification the operation does not claim is rejected.
- `ClassifierManifest.Validate` requires an entry for every class in
  `requiredManifestClasses()`, which includes
  `postgresql_internal_attestation` unconditionally. The only source of internal
  keys is the footprint.

So the manifest must carry internal keys and the operation must not claim the
qualification they came from. Nothing satisfies both.

Each way out changes what a manifest or an operation identity *means*, which is
why it is an author decision rather than a fix:

1. make the required-class set path-aware, so a non-attesting operation's closed
   world excludes the internal class -- this relaxes a closed-world rule whose
   purpose is that no observed statement goes unclassified;
2. let a non-attesting operation still name the footprint -- this weakens the
   presence coupling that keeps a replay distinguishable from an execution;
3. finalize replays through a separate acceptance path.

Weakening either invariant unilaterally is the quiet relaxation this arc exists
to prevent, so the conflict is pinned by
`TestGate22IsBlockedByAConflictBetweenTwoV3Invariants`, which fails the moment
someone resolves it. The wrapper's half of gate 22 -- the typed replay evidence
contract, stored-byte comparison, unchanged signature and digest, the three
absences and the zero Business delta -- is implemented and tested.

## Structural gates

These are not numbered gates. They are the standing structural conditions the
migration must hold at every commit after it lands, checked by source and
call-graph tests rather than by measurement.

| Condition | Test symbol | State |
| --- | --- | --- |
| No active package references the v1.4 accounting types or functions. | `experiment.TestNoActiveReferenceToV14Accounting` | Ratchet; must become a zero assertion |
| The v3 acceptance wrapper has real non-test callers from all three TaskGate paths. | `experiment.TestFinalizeObservationV3HasProductionCallers` | Reports; must become a failure |
| No Adapter file constructs `TrustedInputsV3` or `IndependentInputsV3`. | `experiment.TestAdapterCannotConstructTrustedInputs` | Not yet written |
| The V9 test-fixture package is not imported by production files. | `testfixture.TestFixtureIsNotImportedByProduction` | Not yet written |

`TestNoActiveReferenceToV14Accounting` is a **ratchet** while the cutover is in
progress. It pins the exact set of active v1.4 references that remain, parsed
from the AST rather than grepped, so a comment is discussion and only a
compilable reference counts. The count can only fall: a new reference fails, and
*removing* one also fails, with an instruction to tighten the inventory, so an
allowance cannot outlive the reason for it. **At the end of the cutover the
inventory is empty and the ratchet becomes a plain zero-reference assertion.**

## Canary prerequisite

The Result-heavy 100x4 diagnosis-only v3 canary must not run until every one of
the following holds:

1. all 30 gates pass -- currently 29, with gate 22 BLOCKED as above;
2. the full DSN-enabled suite passes, with zero failures and zero required skips;
3. the v1.4 active symbols are unreachable and the reference set is empty;
4. the finalizer production wrapper has real callers;
5. Artifact, Scale and ProvSQL all use v3;
6. the worktree is clean;
7. HEAD equals origin.

The boundary to declare when all of the above hold is:

    V3 RUNTIME INTEGRATION PASS — CONTRACTS V1.5 FREEZE PENDING
