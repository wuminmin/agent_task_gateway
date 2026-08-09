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

**All thirty-one gates are mandatory before the v3 canary is publication-grade.** A
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
| 18 | **Wrong visible target.** Independently mutate six visible target-record members across the signed-to-carried transcription edge: prepared binding, exact digest, strict digest, row limit, role and policy fingerprint. Every mutation must fail finalization; a separate mutation binding the target classification to another contract identity must also fail. | `experiment.TestGate18WrongVisibleTargetFailsFinalization` + `experiment.TestGate18And19RejectAnotherContractIdentityForATarget` | PASS |
| 19 | **Wrong companion target.** Independently perform the same six signed-to-carried target-record mutations on the companion target. Every mutation must fail finalization; the separate another-contract-identity mutation must also fail. | `experiment.TestGate19WrongCompanionTargetFailsFinalization` + `experiment.TestGate18And19RejectAnotherContractIdentityForATarget` | PASS |
| 20 | **Another workload's target.** A target belonging to another operation/cell/workload cannot classify for this operation. | `experiment.TestGate20AnotherWorkloadTargetRejected` | PASS |
| 21 | **Semantic-replay execution state only.** Signed targets are authorized but not executed, and the observer records zero visible and companion target delta. Marking either target executed or observing any target statement must fail. This gate does not compare the authorized target statement identities; gate 31 does. | `experiment.TestGate21SemanticReplayAuthorizesWithoutExecuting` | PASS |
| 22 | **Idempotent replay.** The original persisted V9 receipt document is returned unchanged; no new query, binding or reservation; zero Business delta; any Business statement must fail. | `experiment.TestGate22IdempotentReplayReturnsOriginalReceiptByteForByte` + `experiment.TestIdempotentReplayEvidenceRequiresEveryMember` (wrapper half) | PASS |
| 23 | **Adapter-supplied wrong plan.** A carried plan differing from independent finalizer derivation fails. | `experiment.TestGate23AdapterWrongPlanRejected` | PASS |
| 24 | **Adapter-supplied wrong manifest.** A carried manifest or binding differing from independent derivation fails. | `experiment.TestGate24AdapterWrongManifestRejected` | PASS |
| 25 | **Adapter verdict.** The Adapter claims `pass` while the evidence carries a bad plan, target or delta; the finalizer rejects it because no Adapter verdict has acceptance authority. | `experiment.TestGate25AdapterVerdictIsNeverConsulted` | PASS |
| 26 | **Baseline arm manufactures observer evidence.** Direct PostgreSQL, native ProvSQL and empty/control arms reject TaskGate observer evidence. | `experiment.TestGate26BaselineArmObserverEvidenceRejected` | PASS |
| 27 | **SQL-bearing observer output.** No raw, normalized, base64 SQL or SQL-bearing parser object may enter observer JSON, `Sample`, logs or durable evidence. | `main.TestGate27ObserverEmitsNoSQL` | PASS |
| 28 | **SQL leak through errors.** Errors may reveal only safe codes and deployment-local `queryid`, never SQL. | `main.TestGate28ErrorsLeakNoSQL` | PASS |
| 29 | **Legacy v1.4 accounting rejected.** v1.4/v2 accounting evidence cannot satisfy v1.5/v3 acceptance. | `experiment.TestGate29LegacyV14EvidenceRejected` | PASS |
| 30 | **Binding-digest mutation.** Mutation of any window, operation, manifest, classifier, target, execution, pre-state, receipt, runtime, image or footprint digest fails. | `experiment.TestGate30BindingDigestMutationRejected` | PASS |
| 31 | **Finalizer-reproduced target identity.** The signed-to-reproduced edge consists of exactly eight independently mutated cells: (1) `paired_novel` visible policy fingerprint; (2) `paired_novel` companion policy fingerprint; (3–5) `semantic_replay` visible exact digest, strict digest and policy fingerprint; and (6–8) `semantic_replay` companion exact digest, strict digest and policy fingerprint. Every cell must reach and fail specifically at the finalizer's direct comparison with the signed target. Row limit remains in the production comparator, but binding/pre-state/authorizer limit invariants reject a signed mismatch before this new edge; it is covered directly and is not a gate 31 mutation cell. Signed and reproduced companion presence must also agree. | `experiment.TestGate31ReproducedTargetIdentityMustMatchSignature`; direct member/presence coverage: `experiment.TestRequireReproducedMatchesSignedComparesEveryTargetIdentityMember` + `experiment.TestRequireReproducedMatchesSignedRequiresCompanionPresence` | PASS |

All thirty-one gates PASS at this HEAD.

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
| No active package references the v1.4 accounting types or functions. | `experiment.TestNoActiveReferenceToV14Accounting` | Ratchet: 2 files / 7 symbols remain; P2.4 must make this zero |
| The v3 acceptance wrapper has real non-test callers from all three TaskGate paths. | `experiment.TestFinalizeObservationV3HasProductionCallers` | All three production source callers are present; P2.5 must turn the generic report-only guard into a hard assertion |
| No Adapter file constructs `TrustedInputsV3` or `IndependentInputsV3`. | `experiment.TestAdapterCannotConstructTrustedInputs` | PASS |
| The finalizer's sources are chosen inside `package experiment`, never by a caller. | `experiment.TestNoExportedWayToChooseTheFinalizersSources`, `experiment.TestTheDeploymentConstructorSelectsNoContent` | PASS |
| The V9 test-fixture package is not imported by production files. | `testfixture.TestFixtureIsNotImportedByProduction` | PASS |

## Cutover state

The cutover is per TaskGate path. All three production source paths now use the
v3 acceptance facade. This is code-level wiring, not evidence that every path is
currently runnable or has passed a live deployment.

| Path | Accounting | v1.4 ratchet entry |
| --- | --- | --- |
| Artifact (`result-heavy`) | v3, through `RuntimeFinalizerV3.FinalizeTaskGateObservationV3` | removed |
| Scale (`dependency_e2e`) | v3, through `RuntimeFinalizerV3.FinalizeTaskGateObservationV3`; dormant | removed |
| ProvSQL (`nonce-join-group`, `taskgate` arm only) | v3 source path; dormant and fail-closed before measurement | removed |

The hash-locked ProvSQL profile manifest contains exactly nine public cells:
three scales times direct/native/taskgate. The finalizer admits only the three
TaskGate cells and crosses them with 105 exact private nonce variants, 35 per
scale. `BindingKey` narrows the descriptor set to one exact private variant
before executable candidate construction; the retained contract identity binds
release, index, private binding file, section, key, logical-query digest and
public operation ID. The Adapter supplies none of the SQL, plan, grant or
classifier derivation.

This wiring is intentionally not runnable at this HEAD. The exact frozen
multi-product ProvSQL query contains `ORDER BY`, and the sole production
`sqllowering.Lower` path rejects it as `PAGINATION_UNSUPPORTED`.
`OpenObserverWindowV3` therefore fails closed before the observer window or
measured query can run. No clause was stripped, no alternate derivation was
introduced, and no synthetic query was substituted. The
`provsql-nonce-join` profile remains `routable:false`; no registry or capability
state changed, and this cutover is not a targeted or live pass.

What the Artifact path does now, in order: the finalizer pre-registers the
classification for the frozen cell **before** the operation runs; the observer is
invoked for both phases under that window id and that classifier commitment; the
Adapter submits the receipt, the window and the statement identities it read off
the signed execution binding; and the finalizer fetches its own contract,
Catalog, qualification, runtime identity and Control Store evidence, reproduces
the execution, and accepts or refuses. The Adapter's `experiment.Sample` retains
the finalizer's own `FinalizationV3` as `taskgate_acceptance_v3`.

Three consequences worth stating rather than leaving to be rediscovered:

- **The Artifact evidence envelope changed shape.** `ArtifactVerificationEvidence`
  carries an `observer_window` (a `ObserverWindowV2` pair) in place of
  `observer_before`/`observer_after` schema-v1 snapshots, and
  `sample-v1.schema.json` moves with it. The v1 pair could not be produced on
  this path any more, and a zero-valued snapshot retained in its place would read
  as evidence that was collected.
- **The Adapter carries the pre-registration rather than deriving it.** A
  classifier manifest is built from the operation's rendered target statements,
  which is precisely the material an Adapter may not hold, so there is no version
  of this where it derives its own classification. What the carried copy
  establishes is that the operation the Gateway signed is the one whose
  classification was committed before it ran — the two are reached by different
  routes, a selector beforehand and a signed preparation afterwards. It is not a
  second derivation and must not be cited as one.
- **The artifact evidence invariant no longer re-derives acceptance.**
  `validateArtifactVerification` cannot: acceptance needs the verified receipt,
  the frozen contracts, the activated Catalog, the retained qualification and the
  Control Store, none of which a Sample carries. What it now checks is that the
  sample IS the one that was accepted — the finalizer's record is present, the
  retained window is the window it was settled over, and the sample's Business
  and resource numbers are the ones that window and that record produce.

No real-deployment run has established end-to-end acceptance for this cutover,
and ProvSQL cannot currently reach measurement because of the lowering refusal
above. Nothing in this section is evidence of a live or publication-grade pass.

## The shared target preparation — approved, in progress

The blocker below was put to the author, who **approved the extraction**. It is
the third of its kind, after `internal/physicalquery` itself and then
`internal/sqlidentity`, and needs no further authorization.

### The proof boundary, stated correctly

Both the Gateway and the finalizer call **the same frozen functions**. The
property that establishes is:

    independent reconstruction from independently sourced inputs
    using the frozen shared preparation algorithm

It is **not** "an independently implemented compiler", and neither the paper nor
the v1.5 contract may claim that. One implementation is the point — two would
drift, and the drift would read as a measurement result. The compiler's own
semantic correctness continues to be established by the frozen contracts, the
oracles, the SQL-executability gate and the result comparisons. What the v3
observer adds is that the runtime actually executed the structure and the bytes
the frozen compiler defines.

### Where it lives

`internal/physicalquery`, extended rather than a new lower package:
`go list -deps` confirms `catalog`, `queryplan`, `exposure` and `ordinal` do not
import `physicalquery`, so the preferred design has no cycle. The three stages
are now `Prepare` → `DeriveLimits` → `Derive`.

### Landed — the sealed contract

The contract is hardened before any derivation moves behind it, because it is
what the parity tests and then the finalizer will be written against.

- **`PreparedOperation` is runtime-only and refuses to serialize.** The first
  shape held SQL and identities in one struct behind `json:"-"` tags plus a
  keyword scan; both are conventions a future member can be added without.
  `MarshalJSON` now returns an error outright.
- **`PreparedOperationBindingV1` is the durable half** — the only thing that may
  enter a V9 receipt, a Sample or retained evidence. Versions, flags, counts and
  digests; projections enter as an order-sensitive digest plus a count rather
  than as column names. Its test enumerates the encoded members and fails on any
  it does not know to be SQL-free and physical-name-free.
- **Every digest is sealed, not asserted.** `Seal` computes; `Validate`
  recomputes and rejects a supplied one. Previously any 64 hex characters passed
  a format check, which would have made the whole comparison decorative.
- **Inputs are canonicalized and bound.** `PreparationInputsSHA256`,
  `GrantSHA256` and `SnapshotBindingSetSHA256` are over canonical values, so two
  callers assembling one authorization in different orders reach one identity —
  the Gateway walks a Catalog, the finalizer reads a frozen contract. What
  canonicalization must not paper over is rejected: columns granted for an
  unapproved product, a `Products` map keyed differently from the product it
  holds, publications out of order, a Catalog digest that is not a digest.
- **`CompilerIdentityV1` is typed and sealed** over the `queryplan` compiler and
  the `sqlpolicy` renderer, each with both its deliberate version and its
  behavioural digest, read from the running binary.
- **`SnapshotBinding` describes the artifact, not just its name** — the sidecar
  relation the generated SQL joins and the row count the base-fact estimate
  reads. A descriptor that only identified an artifact would force this package
  to go and look, which is the dependency the extraction removes.

### Remaining, in order

1. **Differential parity harness** in `package gateway` (the glue is unexported,
   so the test has to live there). Table-driven; for each shape it runs the
   existing Gateway derivation and `physicalquery.Prepare` from the *same*
   inputs and compares visible and companion SQL bytes, the three projections,
   the grouped and expanded flags, the plan and ordinal-program digests, the
   dictionary-set and sidecar-grant digests, the predicate-footprint identity,
   the view binding and revision, the estimated base facts, and the prepared
   operation and target bindings.

   Required shapes, each of which must map to at least one concrete case:
   simple non-grouped; grouped/aggregate; single-query non-exposure; ordinal V4;
   ordinal V5; expanded evidence; non-expanded evidence; relational Join/Union;
   semantic View; mandatory scopes; duplicate Product/view bindings; Scale
   pre-consumed ledger state; ProvSQL TaskGate; Result-heavy 100x4.

   The old and new paths may coexist **only inside this test** during the
   migration. There must never be a production flag choosing between them.
2. **Move the derivation.** From `exposure.go`: `buildPlanExposureContext`,
   `configureV2`, `configurePredicateFootprintV5`, `extendGrant`,
   `usesExpandedEvidence` and their pure helpers. From `ordinal_sidecar.go`:
   `ordinalQueryProduct` and `bindOrdinalSidecars`, with registry access replaced
   by `SnapshotBinding`. From `relational_exposure.go`: the builder half only.
   From `query.go`: `preparePlan`'s dispatch. The observation half of each file
   — everything deriving an `exposure.Observation` from a `dataconnector.Result`
   — stays in the Gateway.
3. **Gateway delegates**, wrapping `PreparedOperationV1` in its execution/result
   context. `derivePhysicalQuery` authorizes once; no second authorization.
4. **Finalizer takes `TargetPreparationInputsV1`** instead of
   `TrustedInputsV3.VisibleSQL`/`CompanionSQL`, calls `Prepare` then `Derive`
   itself, and compares against the V9 signature. The signed values are
   comparison targets, never derivation inputs.
5. **Replace the blocker** with `TestTargetPreparationIsShared`,
   `TestGatewayDelegatesTargetPreparation` and
   `TestFinalizerDerivesTargetsFromFrozenInputs`, plus AST/import guards proving
   the Gateway holds no second compiler path, the evaluation tree holds no
   reimplementation, and the Adapter constructs no trusted inputs.

Only then does the runtime cutover in the next section become possible.

## The blocker this replaces

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

**That was true until T1d, and it is not any more.** The derivation was
unexported glue inside `internal/gateway` — `planExposureContext`,
`buildRelationalExposureContext` and the ordinal sidecar binder — so the step
between `queryplan.CompileOrdinal`/`CompileRelational` and
`physicalquery.Derive` was reachable from no other package.

T1d completed the extraction. `internal/physicalquery.Prepare` and
`PrepareSemanticView` derive both statements from immutable inputs, and they are
now the **only** implementation: the Gateway calls them on its production path
and its own derivation is deleted, so there is no second derivation to drift
against. A finalizer holding frozen contract material can reach the same two
statements without asking the Gateway and without reimplementing anything.

The production facade is now called by Artifact, Scale and ProvSQL, so the old
unwired-source-call diagnosis no longer describes the tree.
`TestV3CutoverIsBlockedByTheUnwiredFinalizer` remains only as a transitional
structural regression check until P2.5 deletes it; it must not be cited as
evidence that the acceptance facade has no caller.

The remaining migration obligations are explicit: P2.4 must remove the final
two-file/seven-symbol v1.4 declaration/construction surface, and P2.5 must turn
the generic production-caller reporter into a hard guard. ProvSQL also remains
fail-closed at the shared multi-product `ORDER BY` lowering boundary described
above. Therefore the boundary

    V3 RUNTIME INTEGRATION PASS — CONTRACTS V1.5 FREEZE PENDING

must not be declared.

`TestNoActiveReferenceToV14Accounting` is a **ratchet** while the cutover is in
progress. It pins the exact set of active v1.4 references that remain, parsed
from the AST rather than grepped, so a comment is discussion and only a
compilable reference counts. The count can only fall: a new reference fails, and
*removing* one also fails, with an instruction to tighten the inventory, so an
allowance cannot outlive the reason for it. **At the end of the cutover the
inventory is empty and the ratchet becomes a plain zero-reference assertion.**
At P2.3 the pinned inventory is exactly two files and seven symbols; it is not
the zero-reference state required by the canary prerequisite.

## DSN-enabled suite — acceptance is machine-checked, not an exit code

An exit code cannot distinguish a suite that ran from a suite that skipped.
Running with `-json` so skips are observable found four DB-backed tests that had
never actually run, on a harness that could run all of them, across runs that
all exited zero.

`evaluation/cmd/final-v5-dbtest-report` is now the acceptance authority. It reads
the `go test -json` stream, emits the committed summary, and **fails on any skip
it does not declare** — with a reason that must match what the test actually
printed, so an allowance stops covering a test that starts skipping for a
different reason. Report v2 also fails on any configured allowance that matched
no observed skip; the reverse check exposes an obsolete exception when a test
starts running, is renamed, or disappears.

The retained historical summary for commit `5cac17e` is
`docs/evidence/dbtest-suite-5cac17e.json`: 96 packages, 2736 tests, **0 failed
packages, 0 failed tests, 12 declared skips, `"accepted": true`**, against
PostgreSQL `160014` on the digest-pinned image, Go 1.25.12, 60-minute package
timeout, with the SHA-256 of the raw report it was derived from.

### Fixed — four tests that had never run

- `internal/dataconnector.TestSessionPinsProduceDistinctQueryIDsLive` and both
  halves of the strict-AST C3 gate
  (`internal/sqlidentity.TestSourceDerivedDigestsMatchLivePostgreSQL`,
  `TestRuntimeTemplateDigestsAreStableOnLivePostgreSQL`) read
  `testpostgres.SchemaDSN`, which is the **control** store. It has no
  `pg_stat_statements`, so all three skipped with *"pg_stat_statements is not
  installed on this deployment"* on a harness where it demonstrably is — and C3
  is the gate the classifier's source-derived digests rest on. They now use
  `testpostgres.StatementStatsDSN`, backed by `BUSINESS_ADMIN_TEST_POSTGRES_DSN`.
- `final-v5-adapter.TestLiveCompilerPostgreSQLFixture` skipped as *"live compiler
  PostgreSQL DSN is not configured"* against a harness whose business server
  already carries the `final_v5_compiler` schema it needs.
  `TASKGATE_FINAL_V5_BUSINESS_DSN` is now exported.

Making them run surfaced two further defects they had been hiding: the C3 probe
view leaked (it had only ever run against a throwaway schema that was dropped
wholesale, so the second run against a persistent server failed on *"relation
already exists"*), and nothing serialized the three tests that reset the
server-wide `pg_stat_statements` while `go test ./...` runs packages in
parallel. Both are fixed — the probe is dropped on the way in and out, and the
resets take a session-scoped advisory lock.

### Declared skips

Each is declared in `allowedSkips` with a category and a justification that a
reviewer can check.

| Category | Tests | Why |
| --- | --- | --- |
| `separate_database_required` | the two `finalv5sqlcheck` probe-equivalence tests | They provision their own benchmark dataset and require a database that does **not** already carry the frozen `final_v5_benchmark` schema, which `db/init` installs here. `scripts/db-test-env.sh env` printed their DSN and `test` did not, and that asymmetry was hiding a real incompatibility rather than an oversight: exporting it turns a visible, explained skip into `schema "final_v5_benchmark" already exists`, which says nothing about the probe rename under test. **Allowlisted only because `run-sql-executability-gate.sh` passes on this same HEAD** against its disposable empty PostgreSQL 16.14 — `final-v5-contracts-v1.4`, 28 artifacts, 71 rendered cells, 0 failed. |
| `separate_deployment_required` | `TestProvSQLLiveExternalPair`; `TestAttackAdapterLivePreflight`; `TestRLSAdapterLivePreflight`; the three `experiment.formal_window_live` gates | The ProvSQL pair needs `compose.provsql.yaml`, whose `final-v5-direct-postgres` binds `127.0.0.1:25534` — the port this harness's business server uses — and whose ProvSQL server is a source-built image; the two projects cannot run side by side. The rest need a full formal topology (OA, Gateway, Control store, MinIO) that this two-server harness does not start. |
| `premise_excluded_by_state` | `TestRegistryClaimsNoSupportWithoutAManifest` | The v1.4 tree carries its current-release manifest, so the repository-state test's premise is false. A state-independent fixture covers the invariant now; P4 removes the manifest and makes this gate runnable before v1.5 freeze. |

The retained v1 reports still contain the former `evidence_not_yet_produced`
allowances for four positive activation-support tests. They are historical
records, not the current allowlist: after `017e73a` those tests run and pass.

## Canary prerequisite

The Result-heavy 100x4 diagnosis-only v3 canary must not run until every one of
the following holds:

1. all 31 gates pass;
2. the full DSN-enabled suite passes, with zero failures and zero required skips;
3. the v1.4 active symbols are unreachable and the reference set is empty;
4. the finalizer production wrapper has real callers;
5. Artifact, Scale and ProvSQL all use v3;
6. the worktree is clean;
7. HEAD equals origin.

The boundary to declare when all of the above hold is:

    V3 RUNTIME INTEGRATION PASS — CONTRACTS V1.5 FREEZE PENDING
