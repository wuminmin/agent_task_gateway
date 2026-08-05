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

Every gate now carries its own **stable test symbol**, named for the gate, so
this document is checkable by symbol rather than by prose. Each such test
asserts its own requirement rather than delegating to a similarly-named existing
test. The older tests remain: they were written against the implementation and
read as documentation of it, while these are the contract.

A gate is marked `PASS` only where its named test exists and is green at this
HEAD.

Gates 18, 19 and 22 each found a real defect while being written; those are
recorded with the gate rather than silently fixed.

## The gates

| # | Requirement | Required evidence / test symbol | Status |
| --- | --- | --- | --- |
| 1 | The out-of-process observer emits strict `ObserverSnapshotV2` JSON and has no v1 fallback path. | `main.TestCollectEmitsStrictObserverSnapshotV2` | PASS |
| 2 | **Snapshot-v1 rejection.** A schema-v1 `ObserverSnapshot` must be rejected by the v1.5/v3 runtime path. No fallback to v1 is permitted. | `experiment.TestGate02SnapshotV1Rejected` | PASS |
| 3 | **Observer-window identity mismatch.** `before.observer_window_id != after.observer_window_id` must fail. | `experiment.TestGate03WindowIdentityMismatchRejected` | PASS |
| 4 | **Classifier-manifest identity mismatch.** Before/after, Adapter, observer and finalizer classifier-manifest digests must agree exactly. Any mismatch must fail. | `experiment.TestGate04ClassifierManifestMismatchRejected` | PASS |
| 5 | **Operation-binding identity mismatch.** Before/after, carried and independently derived operation-binding digests must agree exactly. Any mismatch must fail. | `experiment.TestGate05OperationBindingMismatchRejected` | PASS |
| 6 | **Atomic role-total invariant.** Observer role-wide total must equal the sum of structural rows from the same snapshot row set. Any disagreement must fail. | `experiment.TestGate06RoleTotalMustEqualStructuralSum` | PASS |
| 7 | **Counter regression or disappearing row.** Any cumulative-call regression, disappearing structural key or impossible before/after transition must fail. | `experiment.TestGate07CounterRegressionOrDisappearanceRejected` | PASS |
| 8 | **`pg_stat_statements` reset mutation.** `stats_reset` changing inside the observer window must fail. | `experiment.TestGate08StatsResetMutationRejected` | PASS |
| 9 | **`pg_stat_statements` eviction mutation.** `dealloc` changing, or non-zero `dealloc` proving possible eviction, must fail. | `experiment.TestGate09DeallocMutationRejected` | PASS |
| 10 | **Measurement-environment mutation.** PostgreSQL version, `track`, `track_utility` or `track_planning` drift must fail. | `experiment.TestGate10MeasurementEnvironmentMutationRejected` | PASS |
| 11 | **PostgreSQL runtime mutation.** Any immutable PostgreSQL image, container, platform or server-identity mismatch must fail. | `experiment.TestGate11PostgreSQLRuntimeMutationRejected` | PASS |
| 12 | **Gateway runtime/source mutation.** Any Gateway commit, context, source manifest, image, binary, container or formal-build identity mismatch must fail. | `experiment.TestGate12GatewayRuntimeMutationRejected` | PASS |
| 13 | **Formal-healthcheck mutation.** Any approved `/health/live` command, interval, timeout or retries mismatch must fail. | `experiment.TestGate13FormalHealthcheckMutationRejected` | PASS |
| 14 | **Same-total control-class substitution.** Missing one required control plus an extra different control at the same total must fail. | `experiment.TestGate14SameTotalControlSubstitutionRejected` | PASS |
| 15 | **Same-total PostgreSQL-internal-key substitution.** Missing one qualified internal key plus another key at the same total must fail. | `experiment.TestGate15SameTotalInternalKeySubstitutionRejected` | PASS |
| 16 | **Missing required control.** Any control class or internal key below exact expected multiplicity fails. | `experiment.TestGate16MissingRequiredControlRejected` | PASS |
| 17 | **Extra or unexpected statement.** Any class/key above expected multiplicity or absent from the classifier fails. | `experiment.TestGate17UnexpectedStatementRejected` | PASS |
| 18 | **Wrong visible target.** Independently mutate the visible target's prepared binding, exact digest, strict digest, row limit, role and contract identity. Every mutation must fail finalization. | `experiment.TestGate18WrongVisibleTargetFailsFinalization` + `experiment.TestGate18And19RejectAnotherContractIdentityForATarget` | PASS |
| 19 | **Wrong companion target.** Perform the same six mutations independently on the companion target. Every mutation must fail finalization. | `experiment.TestGate19WrongCompanionTargetFailsFinalization` + `experiment.TestGate18And19RejectAnotherContractIdentityForATarget` | PASS |
| 20 | **Another workload's target.** A target belonging to another operation/cell/workload cannot classify for this operation. | `experiment.TestGate20AnotherWorkloadTargetRejected` | PASS |
| 21 | **Semantic replay.** Targets `authorized=true`/`executed=false`; zero visible and companion delta; any `executed=true` or target statement must fail. | `experiment.TestGate21SemanticReplayAuthorizesWithoutExecuting` | PASS |
| 22 | **Idempotent replay.** The original persisted V9 receipt document is returned unchanged; no new query, binding or reservation; zero Business delta; any Business statement must fail. | `experiment.TestGate22IdempotentReplayReturnsOriginalReceiptByteForByte` + `experiment.TestIdempotentReplayEvidenceRequiresEveryMember` (wrapper half) | PASS |
| 23 | **Adapter-supplied wrong plan.** A carried plan differing from independent finalizer derivation fails. | `experiment.TestGate23AdapterWrongPlanRejected` | PASS |
| 24 | **Adapter-supplied wrong manifest.** A carried manifest or binding differing from independent derivation fails. | `experiment.TestGate24AdapterWrongManifestRejected` | PASS |
| 25 | **Adapter verdict.** The Adapter claims `pass` while the evidence carries a bad plan, target or delta; the finalizer rejects it because no Adapter verdict has acceptance authority. | `experiment.TestGate25AdapterVerdictIsNeverConsulted` | PASS |
| 26 | **Baseline arm manufactures observer evidence.** Direct PostgreSQL, native ProvSQL and empty/control arms reject TaskGate observer evidence. | `experiment.TestGate26BaselineArmObserverEvidenceRejected` | PASS |
| 27 | **SQL-bearing observer output.** No raw, normalized, base64 SQL or SQL-bearing parser object may enter observer JSON, `Sample`, logs or durable evidence. | `main.TestGate27ObserverEmitsNoSQL` | PASS |
| 28 | **SQL leak through errors.** Errors may reveal only safe codes and deployment-local `queryid`, never SQL. | `main.TestGate28ErrorsLeakNoSQL` | PASS |
| 29 | **Legacy v1.4 accounting rejected.** v1.4/v2 accounting evidence cannot satisfy v1.5/v3 acceptance. | `experiment.TestGate29LegacyV14EvidenceRejected` | PASS |
| 30 | **Binding-digest mutation.** Mutation of any window, operation, manifest, classifier, target, execution, pre-state, receipt, runtime, image or footprint digest fails. | `experiment.TestGate30BindingDigestMutationRejected` | PASS |

All thirty gates PASS at this HEAD.

### Gate 22's blocker was resolved by author decision: a path-aware class set

Gate 22 was previously `BLOCKED`. The v3 model could not finalize an exact
request-ID replay at all, because two of its own rules contradicted each other on
that path:

- `CompileClassifier` presence-couples attestation in both directions. A path
  performing no Attestation -- `idempotent_replay` returns before
  `datasourceEvidence` and reaches Business PostgreSQL not at all -- must name
  neither an ExpectedSchema nor a qualified footprint, and an internal manifest
  entry naming a qualification the operation does not claim is rejected.
- `ClassifierManifest.Validate` required an entry for every class in
  `requiredManifestClasses()`, which included
  `postgresql_internal_attestation` unconditionally. The only source of internal
  keys is the footprint.

So the manifest had to carry internal keys and the operation had to not claim the
qualification they came from. Nothing satisfied both.

**The author approved the path-aware class set**, forward-versioned rather than
applied to v1 in place. The reasoning is that this was never a safety rule: v1
conflated *every class the Gateway can ever produce* with *every class THIS
execution path may produce*. An idempotent replay's legitimate closed world is
the empty set, and an empty set is a strict, content-addressed, operation-bound
contract -- the strongest one available on that path, because with no entry to
match, every Business statement in the window is `V3Unexpected` and the all-zero
plan accepts none.

The active versions are now:

    taskgate-final-v5-observer-classifier-manifest-v2
    taskgate-final-v5-compiled-classifier-v2

Version 1 of each remains historical development evidence and does not enter v1.5
acceptance. `ClassifierManifest.Validate` rejects a v1 document **by name**, so
it fails with the reason rather than being silently reinterpreted under rules
that mean something else.

The rule that replaced `requiredManifestClasses()` is derived from the
independently derived `GatewayControlPlanV3`, in both directions:

| Plan | Manifest |
| --- | --- |
| `plan.Expected()[class] > 0` | must declare that class, exactly |
| `plan.Expected()[class] == 0` | must not declare it at all |
| `postgresql_internal_attestation` | manifest keys `==` `plan.InternalExpectation` keys, per key, each bound to `plan.AttestationFootprintSHA256` |
| `targeted_visible` / `targeted_companion` | manifest cardinality `==` the plan's expectation |

The second row is what v1 had backwards. An entry for a class the path cannot
produce is not harmless surplus: it makes that statement *classifiable*, so a
control statement appearing where none should would be counted as a known class
instead of landing in the unexpected sink.

The resulting shape per path:

| Path | Declared classes |
| --- | --- |
| `paired_novel` | BEGIN, COMMIT, safety pin, representation pin, timeout pin, datasource identity, both view attestations, qualified internal keys, visible, companion |
| `single_query` | as above minus the representation pin and the companion |
| `semantic_replay` | datasource identity, both view attestations, qualified preflight internal keys |
| `idempotent_replay` | none — `Entries = []` |

The manifest may be structurally empty **only** where the independently derived
plan expects zero in every class; an empty manifest compiled against any other
plan fails, and so does an empty manifest merely relabelled with another path
kind.

Three further changes fall out of this:

1. `ClassifierManifest` carries `PathKind`, and `CompileClassifierV2` requires
   `operation.PathKind == plan.PathKind == manifest.PathKind`. A manifest is only
   meaningful for one execution path.
2. `GatewayControlPlanV3` gained a domain-separated SHA-256, bound into the
   compiled-classifier digest alongside the operation identity and the manifest
   digest. Under v1 the class set was universal, so a plan digest protected
   nothing; under v2 the plan settles the class set, and without it the same
   operation could present one manifest under a plan expecting a class and
   another manifest under a plan expecting none of it, with every other digest
   agreeing.
3. `FinalizeObservationV3` branches **before** Catalog and footprint processing,
   on `dimensionsFor(pathKind).requiresSchema` rather than by naming a path. A
   non-attesting path supplying a Catalog path, a footprint or reproduced target
   SQL is *rejected*, not ignored: accepting it would finalize a replay against
   schema and qualification material belonging to some other request.
   `CarriedEvidenceV3.VisibleStatement` became a pointer for the same reason --
   a zero-valued execution identity must not be able to stand in for the absence
   of one.

The idempotent replay still requires, through the unchanged wrapper: the
current observer runtime, PostgreSQL, Gateway and healthcheck identities stable
across the window; reset/dealloc/restart/OOM state stable; the persisted receipt
bytes returned unchanged; the persisted signature unchanged; no new query,
reservation or execution-binding row; and an observer delta of exactly zero.

## Structural gates

These are not numbered gates. They are the standing structural conditions the
migration must hold at every commit after it lands, checked by source and
call-graph tests rather than by measurement.

| Condition | Test symbol | State |
| --- | --- | --- |
| No active package references the v1.4 accounting types or functions. | `experiment.TestNoActiveReferenceToV14Accounting` | Ratchet; must become a zero assertion |
| The v3 acceptance wrapper has real non-test callers from all three TaskGate paths. | `experiment.TestFinalizeObservationV3HasProductionCallers` | Reports; must become a failure |
| No Adapter file constructs `TrustedInputsV3` or `IndependentInputsV3`. | `experiment.TestAdapterCannotConstructTrustedInputs` | PASS |
| The V9 test-fixture package is not imported by production files. | `testfixture.TestFixtureIsNotImportedByProduction` | PASS |

## The runtime cutover is BLOCKED on a missing shared target derivation

Gate 22 is resolved, so the classifier no longer stands in the way of the
cutover. What does is a prerequisite that has never existed.

`FinalizeObservationV3` requires `VisibleSQL` and `CompanionSQL` — **the physical
statements the operation actually executed** — on every path that reaches
Business PostgreSQL. It needs them for two separate things:

- `deriveTargets` builds the manifest's target entries from them. Without them
  the classifier has no visible or companion key at all, so gates 18, 19 and 20
  have nothing to compare against;
- `requireStatementIdentities` compares them against the Gateway's signed
  execution binding. That comparison is the point: it is what catches a receipt
  re-sealed around a different statement.

Only three parties could supply them today, and each is excluded:

1. **the Gateway** — the party whose claim is being checked;
2. **the Adapter** — `TestAdapterCannotConstructTrustedInputs` exists precisely
   to forbid it. An Adapter that supplied the target SQL would be supplying the
   material its own claim is checked against, which is the same defect the
   author's manifest decision rejected in a different guise;
3. **a reimplementation in the evaluation tree** — which the
   `internal/physicalquery` package doc forbids in terms: *"if the evaluation
   reimplemented the derivation, the two would drift and the drift would look
   like a measurement result."*

The derivation is unexported glue inside `internal/gateway`.
`internal/queryplan` exports `CompileOrdinal` and `CompileRelational`, and
`internal/physicalquery` exports `Derive`; the step **between** them — agent SQL
plus Catalog products, to an exposure plan, to the compiled visible and companion
statements — lives in `planExposureContext`, `buildRelationalExposureContext` and
`Service.derivePhysicalQuery`, which no other package can reach.

**The resolution is the same extraction that produced `internal/physicalquery`:**
lift that glue into a shared package both the Gateway and the finalizer call, so
the finalizer reaches the same two statements from frozen contract material and
signed pre-state rather than being told them. It is a forward extraction, not a
rewrite: `physicalquery` was moved out of the evaluation tree for exactly this
reason, and `sqlidentity` after it.

Until then the cutover cannot land without either weakening the Adapter guard or
reimplementing the derivation. The conflict is pinned by
`experiment.TestV3CutoverIsBlockedByTheUnsharedTargetDerivation`, which fails the
moment the derivation becomes shareable — which is when the real cutover gets
written.

Everything downstream of the cutover therefore remains unreached: the v1.4
retirement ratchet is still non-empty, the three TaskGate paths still have no v3
production caller, and the boundary
`V3 RUNTIME INTEGRATION PASS — CONTRACTS V1.5 FREEZE PENDING`
must not be declared.

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

1. all 30 gates pass;
2. the full DSN-enabled suite passes, with zero failures and zero required skips;
3. the v1.4 active symbols are unreachable and the reference set is empty;
4. the finalizer production wrapper has real callers;
5. Artifact, Scale and ProvSQL all use v3;
6. the worktree is clean;
7. HEAD equals origin.

The boundary to declare when all of the above hold is:

    V3 RUNTIME INTEGRATION PASS — CONTRACTS V1.5 FREEZE PENDING
