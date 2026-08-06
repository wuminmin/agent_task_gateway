# Final-V5 Artifact completion — continuation record

Working worktree `/home/wmm/worktrees/taskgate-artifact-rerun`, branch
`tkde-artifact-rerun`. The primary worktree `/home/wmm/agent-scope/task_gateway`
stays on `main @ 804d65d` and is never touched.

## Current HEAD

`468dbe4` — worktree clean, equal to `origin/tkde-artifact-rerun` at session
start (`b50637e`) and ahead by the T1a.2 commit. Tags `final-v5-contracts-v1` …
`v1.4` unmoved (`v1.4` still `af15ee1` → `36b04ba`, matching origin); no v1.5
tag; the primary worktree is still `main @ 804d65d` and clean. Docker 29.1.3 and
`scripts/db-test-env.sh verify` pass against the digest-pinned PostgreSQL 16.14
pair (`server_version_num=160014`, 31 `taskgate_ordinal` sidecar relations).

**Gate 22 is resolved and all thirty gates PASS.** The **runtime cutover is
still not done**: the extraction it depends on is in progress and is described
under "The extraction — T1a.2 done, T1b in progress" below. See also "Gate 22
resolved" and "The cutover blocker", and
`docs/final_v5_v3_runtime_integration_gates.md` for both in full.

### The extraction — T1a.2 done, T1b in progress

**T1a.2 is complete** (`468dbe4`). `physicalquery.PreparedOperation` was
mutable: every load-bearing member was exported, so "prepare correctly, rewrite
the SQL or the policy grant on the returned value, execute, sign the old
binding" was available to any caller and left no trace. A coherent binding
proved the binding was coherent and nothing about the object the Gateway was
about to hand the Connector, which is the claim a V9 receipt actually makes.

Construction is now closed. Every member is unexported; `NewPreparedOperation`
is the only constructor and derives every digest from an `OperationDraft` rather
than accepting one; the accessors deep-copy; `ExecutableStatements` is the only
route to the SQL and validates first, so an unvalidated prepared operation is
not a reachable state. `Validate` recomputes both target bindings, the three
field-list digests, the policy grant, the sidecar grants, the ordinal program,
the dictionary set, the source-publication mapping, the predicate footprint and
the base-fact estimate **from the runtime members**, and names the one that
moved. `RequireInputs` closes the other half — binding-to-inputs — for a caller
that holds the source material.

Two members were missing from `PreparedOperationBindingV1` entirely and were
added. `PolicyGrantSHA256` identifies the sqlpolicy grant preparation *builds*,
which is what a statement is admitted against; `GrantSHA256` only identifies the
task authorization preparation *reads*, so evidence naming just the latter left
the sidecar widening between them undescribed. `SourcePublicationsSHA256` covers
the alias-to-publication mapping the Gateway reattaches its verified snapshot
handles through. The dictionary-set digest is the manifest's own rather than one
local to the package, because that value is what the Control Store keys ordinal
observations on.

Nothing durable was invalidated: no production caller produces a
`PreparedOperationBindingV1` yet, and every existing reference to
`PreparedOperationBindingSHA256` is a test fixture with a synthetic digest.

**T1b-A — complete. T1b-B — not started; it awaits `physicalquery.Prepare`.**
The distinction is the whole point of this entry: the harness being built is not
the differential verification being done, and this record must not be read as
claiming old-vs-new parity has passed. Nothing has been compared against a new
implementation, because there is no new implementation yet.

`internal/gateway/preparation_parity_test.go` holds
`legacyPrepareForParity`, which replays the exact production sequence
(`prepareTaskPlan` → `policyGrant` → `extendGrant` → `extendOrdinalPolicyGrant`)
and reads off a 26-member `preparationShape`; `requireSameShape` names every
member two preparations disagree on. The legacy derivation is reachable from
`_test.go` only, and no production path chooses between implementations.

All fifteen named shapes are covered and green: the thirteen table cases
(`simple_non_grouped_product`, `grouped_aggregate`, `single_query_non_exposure`,
`ordinal_v4`, `ordinal_v5`, `expanded_evidence`, `non_expanded_evidence`,
`relational_join`, `relational_union`, `duplicate_product_binding`,
`mandatory_scope`, `provsql_taskgate`, `result_heavy_100x4`), plus
`TestTheSemanticViewShapePrepares` (both projection and aggregate-barrier forms)
and `TestAConsumedLedgerMovesTheDerivationAndNotThePreparation`.

The last two are separate tests rather than table entries for reasons worth
recording. The semantic View does not enter through `prepareTaskPlan`'s Catalog
lookup: production resolves the task's view binding from the Control Store and
then calls `prepareSemanticViewPlan` with it, and preparation itself performs no
store I/O — so the harness supplies the resolved binding the same way the
extracted package will receive it. The Scale pre-consumed state is a property of
the preparation/derivation split rather than of preparation: the prepared target
binding deliberately excludes the row limit, so a consumed ledger must move the
`Derive` decisions and the executed bytes while leaving the prepared shape
identical. Both halves are asserted.

Old-vs-new equality is not the only oracle. Each case is additionally checked
against evidence neither implementation produces: the prepared dictionary
universe must be the Catalog's own publication set with matching dictionary and
manifest digests and Catalog-published sidecar relations; both prepared
statements must be admitted by the policy grant preparation itself produced; the
grant must be widened only by what metering requires; a duplicated product must
resolve to one dictionary member, one sidecar grant and one publication. Two
mutation suites then require every load-bearing input to move the prepared shape
or fail closed — thirteen input changes and twelve binding-input changes, with
`failsClosed` stated per entry rather than accepting "moved or refused" for all
of them.

**A real limit was found and is pinned, not routed around.** A V5 preparation
cannot build a predicate footprint for a UNION DISTINCT whose branches carry
filters: `queryplan.PredicateBindings` keys its products by product name while a
branch qualifies its columns by branch role, so `left_branch.expense_type`
resolves to nothing and the preparation fails closed with `POLICY_DENIED`. The
same plan prepares under V4, which accounts no footprint.
`TestUnionBranchFilteredPredicatesFailClosedUnderV5` asserts both halves, so the
gap cannot be mistaken for a union that is simply unpreparable. The V5 Union
parity case therefore uses the unfiltered form; when the qualified-column work
lands, that test fails and forces the case to be promoted back.

#### The frozen workloads are not all clear of it

The coverage check over the frozen contracts
(`evaluation/finalv5contracts/union_coverage_test.go`) returns a **mixed**
result, and the negative half is the important one.

**Artifact is clean. Scale dependency-e2e is clean.** No cell in
`contracts/artifact-v1.json`, and no `dependency-e2e` cell in
`contracts/scale-v1.json`, submits a branch-filtered UNION DISTINCT. The
Artifact mainline is therefore not blocked on the qualified-column work.

**ProvSQL is not clean.** `contracts/baseline-v1.json` workload **S5** carries
six active BDG arms — SF1 and SF10 × {novel, semantic_replay,
idempotent_replay} — whose `execute_plan` template
`sql/contracts/S5-bdg-plan.json` is exactly the unsupported shape: a
`union_distinct` over `provsql_orders` with `partition_key` and `orderkey`
filters on both `left_branch` and `right_branch`. `provsql_orders` routes to
budget profile `final-v5-provsql-low-v1`, which is `taskgate-exposure-v5`, so
**these six arms cannot execute as written.**

Nothing has silently passed: all eight S5 cells are `PENDING_IMPLEMENTATION`
with `generated_digest_review: NOT_GENERATED`, so no measurement has been taken
against a shape the runtime refuses. The two `direct` arms are raw PostgreSQL
and are unaffected — they never reach the Gateway's preparation.

`TestTheBranchFilteredUnionDependencySetIsExactlyBaselineS5` pins the affected
set exactly, in both directions: a new cell acquiring the dependency fails, and a
cell losing it fails with instructions to empty the list, delete the test and
promote the parity case. Asserting only that Artifact and Scale are clean would
have let the dependency spread unnoticed and left the affected set recorded
nowhere.

**Consequence for v1.5.** The contract release may claim support only for the
relational fragment actually covered — two-branch UNION DISTINCT **without**
branch filters, and Join — and must not be written as "arbitrary V5 Union".
Baseline S5 needs one of: the qualified-column refactor, a restatement of the S5
plan that puts its predicates outside the branches, or an explicit exclusion of
S5 from the V5 BDG arms. That is a Final-V5 baseline obligation, not an Artifact
blocker, and it is recorded here rather than left in a test.

What T1b-A does **not** contain is the old-vs-new comparison itself, because
`physicalquery.Prepare` does not exist. `requireLegacyParity` is defined beside
the legacy capture as the single hook T1b-B's test calls, so the two halves of
the comparison cannot drift into reading different properties. No golden was
generated by running the legacy implementation.

Stage state, stated so it cannot be over-read:

| stage | state |
| --- | --- |
| T1a.2 | complete |
| T1b-A — legacy capture, named-shape matrix, mutation matrix | complete |
| T1b-B — old vs `physicalquery.Prepare` differential | **complete for all thirteen table shapes; `notYetExtracted` is empty** |
| T1c — move the derivation into `internal/physicalquery` | **single-product and relational done; the semantic View entry point remains** |
| T1d/T1e/T1f — Gateway delegation, finalizer reconstruction, positive guards | after T1c finishes |

### T1c/T1b-B — the single-product derivation is extracted and verified

`internal/physicalquery/prepare.go` holds `Prepare(inputs)` and
`PrepareWith(inputs, compiler)`. Everything they read is in `PreparationInputs`:
no registry, no store, no clock. The snapshot artifacts arrive as
`SnapshotBinding` descriptors, which the Gateway resolves from its verified
registry and the finalizer will resolve from retained Publication evidence, so
neither side depends on the other's copy.

`internal/gateway/preparation_differential_test.go` is T1b-B.
`TestExtractedPreparationMatchesTheGateway` compares the Gateway's derivation
against `Prepare` across the whole 26-member shape and **passes byte-identically
for ten of the thirteen table shapes**: `simple_non_grouped_product`,
`grouped_aggregate`, `single_query_non_exposure`, `ordinal_v4`, `ordinal_v5`,
`expanded_evidence`, `non_expanded_evidence`, `mandatory_scope`,
`provsql_taskgate`, `result_heavy_100x4`. Visible and companion SQL bytes,
field ordering, the ordinal program, the dictionary universe, the sidecar
grants, the predicate footprint, the estimated base facts, the widened policy
grant and the prepared operation and target bindings all agree.

The online relational derivation followed, and **all thirteen table shapes now
pass**: `relational_join`, `relational_union` and `duplicate_product_binding`
were promoted out of `notYetExtracted`, which is now empty. `Prepare` still
refuses a shape it does not implement with `ErrShapeNotExtracted` rather than
producing something, and the guard fails in both directions — a listed shape
that starts preparing must be promoted, and an unlisted shape that refuses fails
outright — so the extraction cannot be called complete while a shape is quietly
excluded.

The relational path differs from the single-product one in what supersedes what,
and getting that backwards would have changed the executed bytes. In the
single-product case the ordinal compilation replaces the visible statement,
because the ordinal compiler recompiles the same scan with provenance handles.
In the relational case the relational compiler produces both statements at once
and the ordinal binding wraps only the provenance one, so the visible statement
stays the relational compiler's. The metering closure is likewise per product
rather than global: widening both products of a Join by the union of their
closures would grant each one columns only the other requires.

Promoting the relational shapes surfaced one more real divergence.
`preparationPolicyGrant` had sorted the approved products; the Gateway iterates
them in the order the task grant declares. Order does not affect authorization
— sqlpolicy resolves a product by logical name — and `PolicyGrantSHA256`
canonicalizes before digesting, so the identity is order-insensitive either way.
What sorting *did* change was the value handed to the policy engine, and a
derivation being extracted must not alter that as a side effect. The sort is
gone.

One further harness alignment, not a divergence: the Gateway holds two
normal-form members and fills whichever its plan shape uses — a `NormalForm` for
a single-product plan, an `AlgebraNormalFormV2` for a relational one. The
extraction carries one member, because the profile and the plan shape already
determine which normalizer produced it. The digests were identical; the harness
now places the single member where the legacy shape keeps it.

The prepared bindings are compared as the value the Gateway will actually sign.
`physicalquery`'s binding and the Gateway's `preparedOperation.digest()` are
different constructions and were never meant to be equal, so the differential
computes the Gateway's binding from the **new** implementation's statements and
requires it to equal the legacy one.

**The semantic View is characterized but not yet differentially verified, and an
empty `notYetExtracted` must not be read as saying otherwise.** That list covers
the thirteen table shapes; the View is tracked outside the table because it does
not enter through `prepareTaskPlan`'s Catalog lookup.
`TestTheSemanticViewShapePrepares` still exercises only the Gateway's own path.
Its extraction needs a second entry point — production resolves the task's view
binding from the Control Store and then calls `prepareSemanticViewPlan` with the
resolved binding, the compiled artifact and the composition — and preparation
still performs no store I/O, so all three arrive as values. Until that entry
point exists and its parity passes, T1c is not finished and T1d must not start.

What that entry point has to take, established by inspection of
`Service.prepareSemanticViewPlan` at this HEAD:

- the resolved view binding's `Digest` and its
  `Expectation.ExpectedRevisionDigest`, as values;
- the compiled `viewcompiler.Artifact` (its `Plan`, `BaseProducts`, `Outputs`
  and `CanonicalPlanDigest`) and the `viewcompiler.Composition`;
- the agent-authored outer plan, which the V5 predicate binding reads its
  filters from;
- the Catalog's `Scopes`, which `CatalogView` does not carry yet.
  `semanticViewGovernanceFor` needs them, through `catalogScopeByName` and
  `equivalentScopePolicies`, to prove scope propagation from root to terminal.

`internal/viewcompiler` depends only on `exposure`, `queryplan` and
`sqllowering`, so `physicalquery` importing it introduces no cycle.

Three pieces move with it: `semanticViewGovernanceFor` (~100 lines, the
source/sensitivity/stable-role/scope envelope proof), `semanticPlanRequiredColumns`
(the exact terminal column closure, which is what keeps a leaf-filter subquery
from projecting every Catalog field), and the V5 predicate-binding block that
builds `PredicateFilterBinding`s and `PredicateFieldBinding`s from the artifact
outputs via `digestViewEvidence`. The terminal products must continue to reach
the internal policy grant only -- never the task's public approved products --
which `TestTheSemanticViewShapePrepares` already asserts on the legacy side and
which the differential must preserve.

Three further properties are asserted on the new side alone, because they are
what the finalizer's independent reconstruction rests on: preparing twice from
one input set produces the same binding; a `Service` with no registry produces
the same preparation as one with; and `RequireInputs` proves each prepared
operation binds the inputs it was prepared from before its shape is compared.

**The differential found two real contract defects, both fixed.**

`PreparedOperationBindingV1` required a fact-field digest of every operation,
which made an unaccounted query impossible to seal at all — a plain query
projects no fact field, because there is no exposure accounting to project one
for. The fact and provenance digests are now each present exactly when their
count is non-zero, checked in both directions.

The binding carried no canonical plan identity. `PlanSHA256` digests the
QueryPlan as submitted; what the exposure ledger and the Query Execution Binding
identify a query by is the profile's normal form — V2's under V2/V3/V4, V4's
under V5. `NormalFormSHA256` now carries it, and `Prepare` computes it in the
order the profiles apply: normalize against the approved surface, then let V5
replace that with the V4 form built against the ordinal product. Taking the V2
form for a V5 query would have identified it by a form its ledger does not key
on.

One harness correction, not a divergence: for a plain query the legacy shape
recorded no projection, because a non-exposure query builds no exposure context.
The Gateway does compute that projection — `queryPlanResultNames` is what names
the stored result's columns — so the harness now reads it from there. The
comparison is against what production actually projects rather than against a
nil the legacy shape happened to leave.

### Gate 22 resolved — the path-aware classifier class set

The author approved a path-aware class set, forward-versioned to
`taskgate-final-v5-observer-classifier-manifest-v2` and
`taskgate-final-v5-compiled-classifier-v2`. v1 of each stays historical
development evidence and is now rejected **by name** rather than reinterpreted.

`requiredManifestClasses()` is gone. Which classes a manifest may declare is
derived from the independently derived `GatewayControlPlanV3`, in both
directions: a class the plan expects must be declared exactly, and a class it
does not expect must not be declared at all. The second half is what v1 had
backwards — an entry for an impossible class makes that statement *classifiable*,
so a control statement appearing where none should would be counted as a known
class instead of landing in the unexpected sink.

An idempotent replay therefore has a real manifest document — version, path kind,
signed digest, compiled operation binding — whose entry list is empty. With no
entry to match, every Business statement in its window is `V3Unexpected` and the
all-zero plan accepts none. That is the strictest contract available on that
path, not the weakest.

`ClassifierManifest` now carries `PathKind`; `CompileClassifierV2` requires
`operation.PathKind == plan.PathKind == manifest.PathKind`;
`GatewayControlPlanV3` gained a domain-separated SHA-256 that is bound into the
compiled-classifier digest; `FinalizeObservationV3` branches on
`dimensionsFor(pathKind).requiresSchema` **before** Catalog and footprint
processing, and rejects rather than ignores schema, footprint or target material
supplied for a non-attesting path; and `CarriedEvidenceV3.VisibleStatement`
became a pointer so absence is a state rather than a zero value.

`TestGate22IdempotentReplayReturnsOriginalReceiptByteForByte` replaces the
blocker test and is green.

### The cutover blocker — the target derivation is not shared

`FinalizeObservationV3` needs `VisibleSQL`/`CompanionSQL`, the **physical
statements the operation executed**, on every path that reaches Business
PostgreSQL: `deriveTargets` builds the manifest's target entries from them
(gates 18/19/20 have nothing to compare against otherwise), and
`requireStatementIdentities` compares them against the signed execution binding.

Only the Gateway (the party being checked), the Adapter (which
`TestAdapterCannotConstructTrustedInputs` forbids) or a reimplementation (which
the `internal/physicalquery` package doc forbids in terms) could supply them
today. The derivation is unexported glue in `internal/gateway`
(`planExposureContext`, `buildRelationalExposureContext`,
`Service.derivePhysicalQuery`).

The resolution is the same extraction that produced `internal/physicalquery` and
then `internal/sqlidentity`: lift that glue into a shared package both the
Gateway and the finalizer call. Pinned by
`experiment.TestV3CutoverIsBlockedByTheUnsharedTargetDerivation`, which fails the
moment it becomes shareable.

Everything downstream is therefore unreached: the v1.4 ratchet is still
non-empty, no TaskGate path has a v3 production caller, and the boundary
`V3 RUNTIME INTEGRATION PASS — CONTRACTS V1.5 FREEZE PENDING` is **not**
declared.

### Previous HEAD record — the gate 22 blocker

`a3e7b7a` — worktree clean, equal to `origin/tkde-artifact-rerun`. The v3
acceptance authority and its gate contract are complete; the runtime cutover
is not done and gate 22 is blocked on an author decision.

### Previous HEAD record — N4 checkpoint

`49dab4e` — worktree clean, equal to `origin/tkde-artifact-rerun`. The N4
checkpoint closed at this HEAD: the two qualifications from published `1a539e1`
both finished, every required equality was checked from the reports rather than
from their logs, and the agreement is committed alongside the two evidence sets.

Tags `final-v5-contracts-v1` … `v1.4` re-verified unmoved at the annotated-tag
objects `00c4636`, `167581c`, `6966cd0`, `114d190`, `af15ee1` (commits `1702e65`,
`5e12765`, `6f353f3`, `38e3bd3`, `36b04ba`); no v1.5 tag; primary worktree still
`main @ 804d65d` and clean. Docker 29.1.3 client and server, Compose v2.40.3.

### Previous HEAD record

`7e12238` — worktree clean. The push that made it equal
`origin/tkde-artifact-rerun` is recorded below, and the N4 requalification and
formal rebuild both refused to run before it.

Session start was `7356be0`, verified equal to `origin/tkde-artifact-rerun` with
a clean worktree. `50c3cb8`, `e3622a5`, `865ae8c`, `d4b2b7f`, `2d52bea` and
`5f71cbb` were confirmed ancestors; tags `final-v5-contracts-v1` … `v1.4`
unmoved; primary worktree still `main @ 804d65d`; no v1.5 tag. Docker 29.1.3
client and server are available again and `scripts/db-test-env.sh verify` passes
against the digest-pinned PostgreSQL 16.14 pair (`server_version_num=160014`, 31
`taskgate_ordinal` sidecar relations).

### Earlier HEAD record

`5f71cbb` — equalled `origin/tkde-artifact-rerun`, worktree clean.

Session start was `5e60495`. Tags `final-v5-contracts-v1` … `v1.4` verified
unmoved at `00c4636`, `167581c`, `6966cd0`, `114d190`, `af15ee1`. No v1.5 tag
exists yet, correctly: no v1.5 freeze evidence exists.

A previous continuation record named `4884119` as HEAD. That was the HEAD when
it was written; the branch had since advanced to `3d1eea9`. History is
forward-only, so the record is corrected here rather than by amending, and the
verification is against the actual branch tip rather than against a hard-coded
SHA.

Forward commits this session, in order:

| commit | what |
| --- | --- |
| `aede5d8` | gofmt hygiene over two pre-existing unformatted files |
| `ab0ae10` | N2 `AttestationFootprintV1` + N3 plan consumption |
| `507ba30` | this continuation record |
| `4361f9d` | N4 probe: footprint qualified as a contract |
| `61f932d` | PostgreSQL runtime identity pinned to an immutable digest |
| `21e693a` | N4 audit: five corrections before any qualification run |
| `818c481` | N4 qualification harness; two live Gateway failures retained |
| `306eba3` | **Stage N4 complete**: two independent live qualifications agree |
| `18a5f58` | **B** `internal/physicalquery`: shared statements + row limits |
| `d3d2c1b` | **C** bound, compiled, operation-scoped classifier |
| `4b26984` | **D** `ObserverSnapshotV2`: authoritative and atomic |
| `c43b7ba` | **E** independent finalizer |
| `c541d7e`, `18bee8e` | N4 forward-fix: provenance, profile binding, no embedded credentials |
| `349d8b9` | two v1.5-candidate qualifications, agreeing |
| `55ccd3d` | SQL-executability gate passing live |
| `11c9e30` | repository hygiene: committed build artifact removed and prevented |
| `4884119` | **I1 (part 1)** observer Business census + v2 snapshot identities |
| `3d1eea9` | continuation record for hygiene and the first half of I1 |
| `50c3cb8` | **I1-A** immutable Gateway source/build/runtime identity + formal build |
| `e3622a5` | **I1-B** observer emits `ObserverSnapshotV2` authoritatively |
| `865ae8c` | **I2-A (structures)** `internal/querybinding`, Query Receipt V9, physicalquery delegation |
| `9bc330b` | this record, corrected forward to `865ae8c` |
| `d4b2b7f` | formal Gateway base images pinned by digest |
| `2d52bea` | digest-pinned PostgreSQL environment for DB-backed tests |
| `5f71cbb` | **I2-A (persistence)** migration 019 + store plumbing for the execution binding |
| `422a80a` | **I2-A0** neutral `internal/sqlidentity`; four V9 validation gaps; exact binding idempotency; canonical persistence; deterministic manifest diagnostics |
| `a6ee9c6` | **I2-A1–A4** authoritative execution pre-state, real production binding, atomic write, V9 version selection |
| `d02efd3` | **I2-A2** semantic-replay binding, `executed=false` on both targets |
| `81bab97` | live V9 round-trip proof; the five live gateway tests fixed with the real bundle |
| `9dffac8` | **I2-B (part)** Adapter requires a verified V9 instead of re-deriving statements |
| `7e12238` | V8 equalities that hid artifact evidence on a V9 receipt |
| `76aef1f` | record for the I2-A production V9 round-trip |
| `1a539e1` | record for the formal Gateway rebuild from the integrated commit |
| `49dab4e` | record for the N4 requalification and the SQL-executability rerun |
| `54ec901` | **N4 checkpoint closed**: the two `1a539e1` evidence sets, their agreement, and the record |
| `2aa5252` | the v3 accounting stack has no production caller |
| `624b295` | the v3 runtime integration gate list, 24 requirements recorded UNSPECIFIED |
| `bcd2f1c` | **I3** `FinalizeTaskGateObservationV3`, the V9-verified acceptance entry point |
| `65ac2e7` | the v1.4 retirement enforced as an AST ratchet |
| `86eb6f0` | record of the v3 integration state |
| `35f77c6` | **Phase 0** the gate contract completed; gap notice resolved |
| `e8128ef` | `internal/testfixture/queryreceiptv9` from public API only |
| `16cedfb` | gates 7, 18, 19, 21, 25, 29; gate 22's blocker pinned |
| `a3e7b7a` | the Adapter guarded out of constructing trusted inputs |
| `HEAD` | this record |

## I2-A — production V9 round-trip

**The defect I2-A0 existed to fix.** The strict normalized-AST digest lived in
`evaluation/internal/experiment`, which Go's internal rule keeps inside
`evaluation/`. Production could not import it, so the Gateway passed a nil
digester to `physicalquery.Derive` and the structural digest of every statement
it authorized was empty — the one field the observer classifies on. The
implementation now lives in `internal/sqlidentity`; `experiment` keeps aliases so
its call sites read as before. The move was checked byte-for-byte against the
pre-move implementation over eleven statements before the old file was deleted,
and golden vectors now pin the digest space so a future edit has to be a
deliberate `StrictASTSchemaVersion` bump. Parser failures carry a stable code
rather than the parser's own message, which quotes the offending SQL.

**Four V9 rules were missing rather than relaxed.** `schema_digest` and
`signed_at` had been enforced since V2 and V3, but the conditions were written as
explicit disjunctions and V9 was left out of three of them; they are version
ranges now. `remaining_rows` and the ledger identity were independently
assertable and are cross-bound to `budget_before` and the exposure evidence. The
visible row limit was checked against the signed pre-state only when a companion
was bound. And `ON CONFLICT DO NOTHING` made a contradictory second binding
silent rather than merely redundant.

**One deviation from the written specification, deliberate.** The specification
asked for `ExposureLedgerBefore.RootEpoch == receipt.Exposure.RootEpoch`. Those
are not the same quantity: the pre-state's epoch is the one the operation was
authorized against, and the charge's is the one it settled at, which a novel
observation advances by exactly one
(`internal/control/ordinal_exposure.go:902`, `ordinal_exposure_v5.go:1225`).
Equality would reject every novel paired execution — precisely what V9 exists to
describe. The binding enforced is the ordering the epoch actually satisfies: the
pre-state may not postdate the charge. Root task and profile version remain
strict equalities.

**I2-A1, the pre-state.** The Gateway read the budget at one instant, the
exposure ledger at another, and took its reservation at a third, then signed all
three as one atomic pre-state. `ReserveBudget` now reads the exposure ledger
under the same task lock that produces the budget snapshot and returns both. The
first derivation is demoted to preparation — it exists only to size the
reservation — and the statements sent to the Connector are re-derived from the
pre-state the reservation observed. A pre-state that moved in the widening
direction leaves the reservation too small for the statement about to run; there
is no safe silent repair, so it fails closed before execution.

**I2-A2, source-checked identities.** The compiler and renderer identities are
computed from what those packages do — a frozen probe is compiled and rendered
and the output digested — so a behaviour change moves them even when no version
constant is touched. `TestCompilerIdentityIsPinnedToItsSource` and
`TestRendererIdentityIsPinnedToItsSource` pin the values and say, in the failure
message, to bump the version rather than update the expectation.

**I2-A3 and I2-A4.** The binding travels on the settlement and is written inside
the transaction that commits the terminal record, the budget settlement, the
exposure settlement and the receipt. Only a COMPLETED query may create one. The
receipt builder loads it back from the row. V9 is emitted when a persisted
binding exists and the receipt already qualifies for V8; a query that has a
binding but cannot reach V8 is refused rather than downgraded.

### The five live gateway tests

Not an I2-A regression, and now fixed. `installCatalogV4SnapshotRegistry`
substituted a hand-written double for any publication whose committed compiler
input carries no rows; that double's manifest had no cold payload, no hot index
and no segments, so `PutOrdinalDictionarySet` refused it — and every exposure-V5
path, including the only configuration that can emit V9, was unreachable in
tests. The source rows are not missing: they are fifty thousand rows of generated
ProvSQL data living in the Business database, which is where they belong. The
harness scans them exactly as `cmd/snapshot-index` does, compiles the bundle,
loads it through `ordinal.ParseHotDictionary`, and verifies the result against
both the compiler input's `expected_digests` and the Catalog. The double is
deleted.

`TestCanonicalCopySurvivesAvailableTransactionFailureAndRecoversExactlyOnce` then
reached its assertions and found V9 where it expected V8 — the correct version
for a completed exposure-V5 artifact query — and, one layer deeper, exposed three
V8 *equalities* that meant "carries artifact intent": `GetQueryReceipt` compared
the persisted version against the literal `"8"` and stopped loading the artifact
registration projection; `get_audit_receipt` gated the whole inclusion block the
same way and silently dropped both the intent and availability proofs an auditor
gets; and the independent finalizer and RQ5 verifier refused V9 outright. Receipt
versions are ordered now, through an exported `VersionAtLeast`.

## Completed milestones

- **N1 audit.** The committed Stage N1 record (`5e60495`) is exploratory
  diagnosis and is NOT consumed as a qualification contract. It measured the
  right property, but: its `expected_schema_digest` is declared and empty and no
  trial carries one; the entry count is encoded in a free-text `relation_kind`
  label rather than an integer; and no PostgreSQL image is bound. The probe
  builds ExpectedSchema directly from live relations and never calls
  `catalogschema.Build`, which is why no digest was available. The evidence
  directory is retained unchanged.
- **N2/N3/N4, as superseded by the `21e693a` audit.** The V1 footprint and the
  first N4 probe were both written and then corrected before any qualification
  ran; only the corrected shape matters now.
  `AttestationFootprintV2` is qualified against one ExpectedSchema digest and
  entry count, one measurement environment and one complete
  `PostgreSQLRuntimeIdentity` (digest-pinned reference, RepoDigest, local image
  ID, running container image ID, platform), across **four** scopes —
  `constructor_or_cold_pool`, `explicit_preflight_pool`,
  `single_query_transaction`, `paired_query_transaction` — never merged, so the
  Artifact paired path cannot consume a footprint measured through
  `Connector.Query`. Constructor and explicit preflight are retained separately
  with their equality recorded, which is what a later revision would need before
  merging them.
  The measured quantity is the **multiset** of internal structural keys, carried
  end to end into `GatewayControlPlanV3.InternalExpectation` and the classifier
  manifest, summed per key as `P * preflight + S * single + Q * paired` and
  validated key by key; the class aggregate survives only as a reporting number.
  A same-total substitution of one internal key by another fails.
  `nested_viewdef_rewrite_lookup` is renamed `postgresql_internal_attestation`,
  and the classifier no longer hard-codes a `pg_rewrite` template — internal keys
  enter the manifest from the qualified footprint under a `qualified_footprint`
  source kind.
  `PortableSHA256` excludes the qualification ID and deployment-local image IDs;
  it is what the two independent runs must agree on.
- **The ExpectedSchema identity defect, measured.** The first N4 probe
  reconstructed ExpectedSchema from live `pg_attribute` (name and type only).
  `catalogschema.Digest` also covers collation, collation version and collation
  determinism, and the Result-heavy Catalog carries `en_US.utf8` / `2.36` on
  three of its sixteen fields. On this tree the Catalog-derived digest is
  `e2a3796f…` and the reconstruction digests to `d2fd017b…`. The qualification
  path now uses `catalog.Load` + `catalogschema.Build`; live relations are read
  only to verify the Catalog, never to define the identity.
- **The measurement sequence.** `AttestationsPerTrial = 2` and dividing a
  combined delta by it are gone. `pg_stat_statements` is reset once and every
  measurement is an adjacent-cumulative-snapshot delta bracketing exactly one
  Attestation. Each interval binds `stats_reset`, `dealloc`, the environment,
  `pg_postmaster_start_time()` and `total delta == sum of structural row deltas`,
  and rejects backwards counts and disappearing keys. Nothing is called "cold"
  because the view was reset; the cold/warm stability claim is withdrawn.
- **PostgreSQL runtime identity pinned** (`61f932d`). `business-postgres` and
  `control-postgres` ran on the mutable tag `postgres:16-bookworm`, while
  `compose.real-pilot.yaml` already pinned `final-v5-direct-postgres` to
  `postgres@sha256:92620dad…`. Business PostgreSQL is the server the whole
  observer accounting measures, so the floating tag made any qualified footprint
  unreproducible in principle. Verified that digest reports PostgreSQL 16.14
  (Debian 16.14-1.pgdg12+1) — `server_version_num` 160014, what
  `RequiredMeasurementEnvironment` demands — so this fixes reproducibility rather
  than moving the frozen environment, and is not an author decision. The pin sits
  in the observer-v3 overlay so ordinary production deployments are unaffected.
  All three PostgreSQL services in the composed topology now resolve to one
  immutable identity.
- **gofmt hygiene** (`aede5d8`) — two files were unformatted at the previous
  HEAD; repaired separately from any meaningful change.

Correction to the `ab0ae10` commit body: it says "the
`expected_schema_footprint` field is declared and left empty". The field is
`expected_schema_digest`. History is forward-only, so the correction is recorded
here rather than by amending.

## Active invariants

- Forward commits only; no amend, squash, rebase or force-push; old tags never
  move.
- Diagnosis runs carry `publication_eligible=false`,
  `capability_changing=false`, `activation_support_changing=false`,
  `formal_campaign=false`, and non-formal unique IDs. No Campaign ID exists.
- A footprint qualified for one ExpectedSchema is invalid for another and must
  be re-qualified; scaling it is forbidden in code, not only in prose.
- Observer equality is never weakened to `total >= expected`; unexpected
  structural statements stay fail-closed.
- Periodic healthcheck stays `/health/live`; `/health/ready` qualification
  happens outside the observer interval.
- No raw or normalized SQL in publication evidence; `queryid` is
  deployment-local diagnosis only.

## Completed: B, C, D, E, and the N4 forward-fix

**B — `internal/physicalquery`** (`18a5f58`). Shared derivation of the physical
statements and the runtime row limits, with the Gateway delegating to it. The
limits live here because `sqlpolicy` renders them into the executable SQL. Three
properties preserved exactly: the companion follows the *authorized* visible
limit (so the visible statement is authorized first, and the ordering is
load-bearing); non-expanded evidence clamps the visible limit to InfluenceFacts
while expanded evidence does not and asks for one extra row; and the ledger
pre-state is an input, so a partly-consumed ledger derives different bytes while
leaving the structural identity fixed.

**C — compiled, bound classifier** (`d3d2c1b`). `OperationIdentity` binds
operation, path, contract, ExpectedSchema and qualification. Free-form target
declarations are gone: `TargetContractIdentity` derives a target's contract from
the operation's, so another workload's target cannot be expressed. The manifest
compiles into an immutable keyed lookup, target cardinality is enforced per path,
and internal keys must carry the operation's footprint digest.
`BindingSHA256` lets the finalizer recompute the binding.

**D — `ObserverSnapshotV2`** (`4b26984`). One atomic reading; `Validate` requires
the role total to equal the sum of the structural rows, which is what makes
"same row set" checkable. Runtime identity is part of the snapshot, including the
SHA-256 of the running healthcheck command. `Accept` is equality class by class
*and* key by key — same-total internal substitution, same-total control
substitution, and missing/extra controls all fail.

**E — independent finalizer** (`c43b7ba`). Derives ExpectedSchema from the
Catalog, the plan from path + footprint, the operation identity from frozen
contract material, and the targets from statements reproduced through
`physicalquery` — then looks at the Adapter's evidence only to reject it. Path
kind is an independent input, never inferred from target count. `MeasurementArm`
stops direct-PostgreSQL and native-ProvSQL arms from carrying observer evidence.

**N4 forward-fix and requalification** (`c541d7e`, `18bee8e`, `349d8b9`). Two
fresh isolated qualifications from clean, published commit `18bee8e`:

| | value |
| --- | --- |
| portable footprint | `032e9c53704d…` — identical, and matches the development runs |
| ExpectedSchema | `e2a3796fb3f5…`, E=1 |
| profile / registry | `profile-a86cd4df5cad6e26` / `final-v5-contracts-v1.4` |
| catalog bytes | `533837084c0d…`, cross-checked against the artifact manifest |
| artifact directory | `814d4df9971f…`, identical in both runs |
| source dependencies | 8, each bound to its bytes at HEAD |

Qualification now refuses a dirty worktree, an unpublished commit, or any source
file differing from the commit. DSN passwords are gone from the harness; the
probe takes no DSN flag at all.

**SQL-executability gate** (`55ccd3d`). Now PASSES live with `-require-live`:
28 artifacts, 71 rendered cells, 0 failed, PostgreSQL 16.14. The committed
manifest regenerated byte-identically.
`evaluation/final-v5-wsl2/scripts/run-sql-executability-gate.sh` reproduces it.
The freeze-time prohibition on a skipped gate is satisfied.

## Repository hygiene — done

`11c9e30` removed the tracked 24 MB `final-v5-attestation-footprint` ELF from the
repository root. Cause: `go build ./evaluation/cmd/<command>` run from the root
writes the executable there, and `git add -A` swept it into `c541d7e`. That
commit stands; the file was removed forward.

Prevention, in increasing durability: `make bin` builds into ignored
`generated/bin` (with `-buildvcs=false`, needed in a linked worktree);
`.gitignore` lists `generated/bin` and the 44 current command names; and
`internal/repohygiene` rejects any *tracked* root-level file whose leading bytes
are ELF/PE/Mach-O/Go-archive, plus any tracked root-level executable that is not
a script. The test is the part that matters — a name list goes stale the moment
someone adds a command. Verified by rebuilding the artifact, force-adding it, and
confirming both checks fail.

## I1 — first half done

`4884119`. The observer can now produce everything `ObserverSnapshotV2` needs
from Business PostgreSQL:

- **One statement, one materialized row set.** A `MATERIALIZED` census CTE yields
  both the role total and the statement rows, so they cannot describe two
  instants — which is exactly what `Validate`'s total-equals-sum check tests. The
  same statement returns the environment, `stats_reset`, `dealloc`, postmaster
  start time and Business WAL position.
- **No SQL escapes.** Text is base64-encoded in SQL (so newlines and the field
  separator cannot corrupt framing), then decoded, digested and dropped. Tests
  assert a parser failure names only the queryid and leaks no fragment, and that
  a marshalled snapshot contains neither the SQL nor its base64.
- **Exact argv identities:** `--phase`, `--observer-window-id`,
  `--classifier-manifest-sha256`, optional `--operation-binding-sha256`. Not
  environment variables.
- **Window identity checks:** `Delta` requires one before and one after, and
  requires window ID, classifier manifest, operation binding and observer source
  identity to match across the pair.
- `observerRequiredSources` extended to every file whose bytes change what a
  snapshot means.

## I1-A — complete (`50c3cb8`)

The observer's `gateway_source_sha256` was filled by hashing the checkout the
observer happened to run from. That asserts something the observer cannot know:
that the running container was built from those bytes. Nothing distinguished
"the image came from this commit" from "someone ran the observer in a directory
that happens to be at this commit", and the ordinary Dockerfile's `COPY . .`
made the gap real rather than theoretical.

- **`GatewayRuntimeIdentityV1`** carries submission commit, clean-tree status at
  build, build-context and source-manifest digests, build target, OCI revision
  label, local and container image IDs, binary digest, platform, healthcheck
  digest and both base images. A canonical aggregate is carried too, but only as
  a convenience: every load-bearing member stays independently inspectable,
  because an aggregate that is the only carried value is an unexplained opaque
  hash.
- **`internal/formalbuild` + `Dockerfile.formal` + `final-v5-gateway-build`.**
  The context is materialized with `git archive` and streamed to the builder, so
  it contains exactly the blobs reachable from the named commit and untracked or
  ignored host files have no path in. The context digest binds relative path,
  file mode, file bytes and symlink target, and is computed twice — over the tar
  fed to the builder and over the commit tree from the object database — with
  agreement required. That agreement is what makes the digest checkable by a
  reviewer holding only the commit.
- **Verification reads the running container through Docker Engine** and compares
  each label against an independently computed value, never against the image's
  own claims. The image also carries its provenance as files beside the binary,
  which must agree with the labels: a label is metadata a retag can rewrite, the
  files are content in the layer the binary lives in.
- **Base images are pinned by digest** in `formal-build/base-images.json`, not by
  tag. The document is committed unpinned and the formal build refuses to run
  until `record-base-images` fills it — a build that quietly accepts whatever a
  tag points at today is the provenance gap the file exists to close.

Defect found by the tests: `git archive` prepends a `pax_global_header` carrying
the commit id. Digesting it would have made two commits with byte-identical
trees digest differently, so the context digest would no longer have meant "the
same source bytes". It is excluded.

## I1-B — complete (`e3622a5`)

`run()` parsed only `--phase` and `collect()` returned the v1 document, so every
identity `ObserverSnapshotV2` was designed to carry either came from the Adapter
or did not exist.

- The v1.5 path parses the full invocation and emits v2 **or fails**. There is no
  fallback: a path that degrades under exactly the conditions the evidence exists
  to detect is worse than no evidence.
- The observer resolves the Gateway identity through Docker Engine against a
  context materialized from the published commit, and the PostgreSQL identity
  from the running container's image. Neither is accepted from the Adapter or the
  environment — a runtime identity supplied by the party being measured is a
  claim about the deployment, not evidence about it.
- `readBusinessCensus` replaces `readBusinessCounters`; total and structural rows
  come from one `MATERIALIZED` row set.
- **Project topology is retained as its own signed member.** The Gateway and
  PostgreSQL identities say what those two services are and nothing about the
  other ten; a sidecar replaced between the before and after snapshots would
  leave both untouched while changing what the interval contained.

Defect found: the census kept only the **first** queryid for a merged structural
key while summing the calls of all of them, implying that one row identified the
aggregate. Every contributing queryid is now retained, sorted and deduplicated,
as a process-local diagnostic. `queryid` stays absent from the emitted document
entirely rather than merely unused.

`EntriesFromCommit` reads its blobs through one `git cat-file --batch` instead of
one subprocess per file; the observer recomputes this digest on every snapshot,
and a process launch per tracked file made the cost proportional to the
repository rather than to the measurement.

## I2-A — structures and delegation complete (`865ae8c`); persistence NOT done

Query Receipt V8 binds the authorization, the budget and the exposure
accounting, but nothing in it says which physical statements ran. The evaluation
filled that gap by re-deriving them afterwards and treating the result as though
the Gateway had signed it — a second opinion about the execution, not evidence
about it, and the two can differ in exactly the case the evidence exists to
detect.

**Done:**

- **`internal/querybinding`**, neutral because the Gateway, the receipt and the
  evaluation all need it and production must not depend on evaluation code.
  `ExposureLedgerBeforeV1` carries the pre-state the row limits derive from and
  no FactID, bitmap member, task payload or SQL. Its limits/used/remaining
  vectors must agree: `Remaining` is what a caller would have to forge to widen
  a row limit, so it must not be independently assertable.
  `QueryExecutionBindingV1` keeps `Authorized` and `Executed` separate, because a
  semantic replay authorizes its targets to derive the semantic key and runs
  neither; collapsing them would make that path indistinguishable from a novel
  execution. Path semantics are enforced by the structure, not by each consumer.
- **Query Receipt V9** signs both structures whole, not merely their digests —
  signing only a digest leaves every other member unprotected against a holder
  who recomputes it to match an edit. The binding must name the pre-state and the
  `budget_before` the receipt itself carries, and its row limits must reproduce
  from that signed pre-state. V8 semantics are untouched; earlier versions may
  not carry V9 material, or a holder could staple a binding onto a V8 receipt and
  present it as signed.
- **Production physicalquery delegation.** The Gateway calls
  `physicalquery.Derive` once and executes the decisions it returns, instead of
  authorizing the pair itself and delegating only the row-limit arithmetic.
  Previously the two agreed only as long as nobody changed one of them, and a
  divergence would have surfaced as a measurement result rather than as a build
  failure.

**Not done — V9 database persistence, recovery and replay.** The Control
PostgreSQL migration and store plumbing that would persist
`ExposureLedgerBeforeV1` and `QueryExecutionBindingV1` atomically with the
terminal query evidence, reload them without loss, sign the same canonical
binding on recovery, and return the original binding byte-for-byte on idempotent
replay, are **not implemented**. The V9 structures are exercised only by
hand-constructed receipts, which is explicitly not sufficient.

The blocker was environmental, not technical: at the previous session Docker was
absent from this WSL2 distro (`docker` not on PATH, Desktop WSL integration off)
and no PostgreSQL server was listening, so the DB-backed gateway tests skipped
and neither the persistence round-trip nor the live canary could be verified.
Landing an unverifiable migration was refused rather than attempted. Docker is
available again as of this record.

## Formal Gateway build — rebuilt from the integrated commit

The `d4b2b7f` image below is now intentionally historical: it predates migration
019, the store plumbing and the whole production V9 path, so it must not be used
for the v3 canary. Rebuilt from the integrated, published commit:

| | |
| --- | --- |
| source commit | `76aef1fbefb1fbf3e19a6b6889120206b19828f4` (clean, equals `origin/tkde-artifact-rerun`) |
| build context | `4548fb68c4dee5abbfd42a2ef07f3fb77a9a577f54af7da1fe1db53703f633fc` over 1289 tracked files |
| source manifest | `05ecf02d80420e78a54c1a19b9a260cb1240792c7250c2881c08be650abcf9a6` |
| build target | `gateway` |
| image ID | `sha256:7b4494945a02b100114974060dd9822e14f3c0b783c46141c3f115c351b5ed76` |
| binary | `be879ece120d4c80aec680b9f1d3794a33ff90bbb2a572fb3b7dce2385aab7fb` |
| platform | `linux/amd64` |
| builder base | `golang@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58` |
| runtime base | `debian@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818` |

The image labels carry the same commit, context and manifest digests the builder
computed, and `sha256sum /usr/local/bin/app` recomputed inside the image equals
the recorded binary digest. The base pins are unchanged from `d4b2b7f`, so the
context digest moving from `3e813b17…` to `4548fb68…` is entirely the source
change: 1274 tracked files became 1289.

This image is the I2-A integration build. Any commit after `76aef1f` — including
this record — moves the context digest, so the image the v3 canary runs must be
rebuilt from whatever commit is finally integrated, and its provenance recorded
in place of the table above. The formal builder refuses a dirty or unpublished
tree, which is what makes that check mechanical rather than remembered.

### Previous formal build (historical — do NOT use for the v3 canary)

Docker returned, the base images were pinned (`d4b2b7f`) and the formal build ran
from a clean, published commit:

| | |
| --- | --- |
| source commit | `d4b2b7fb0ff37c992946a808ac0623ac9624cba3` |
| build context | `3e813b1701bbddba80b6c70d88a8e1189ab7dafa08d7c1c4ea8f7e85035a98a7` over 1274 tracked files |
| source manifest | `19e17a792be18e805e8d2347f5e6934ebb4fba75efd2998fc852279c3cf12dd5` |
| build target | `gateway` |
| image ID | `sha256:308247c28cab01365a2fbc0c2434afd6fc9a86816a7c262a651306786e4242d0` |
| binary | `22097ba0e2bb39ea319d933f7a7d0a027f731855c89eb8e1a8a74bb64d639880` |
| platform | `linux/amd64` |
| builder base | `golang@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58` |
| runtime base | `debian@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818` |

Verified independently of the build run: both base digests resolve on
linux/amd64 with the recorded repoDigest; the image labels equal the provenance
files inside the image; and `sha256sum /usr/local/bin/app` recomputed inside the
image equals the recorded binary digest.

## N4 requalification after the strict-AST move — two runs agree

Moving the strict normalized-AST digest to `internal/sqlidentity` changed an N4
source dependency, so the existing qualifications became still-valid development
evidence that had to be reproduced from the new source. Two fresh isolated
qualifications from clean, published `1a539e1`:

| | qualification-i2a-01 | qualification-i2a-02 |
| --- | --- | --- |
| portable footprint | `032e9c53704d` | `032e9c53704d` |
| footprint | `8bac78bfdfd3` | `d6dea7d99721` |
| ExpectedSchema | `e2a3796fb3f5`, E=1 | `e2a3796fb3f5`, E=1 |
| profile / registry | `profile-a86cd4df5cad6e26` / `final-v5-contracts-v1.4` | same |
| catalog bytes | `533837084c0d` | same |
| artifact directory | `814d4df9971f` | `814d4df9971f` |
| artifact manifest | `e2b96d53af3a` | `8939d1173eb4` |
| PostgreSQL | `postgres@sha256:92620dadd…`, linux/amd64 | same |

The **portable** footprint is what must agree, and it does — and it equals the
`032e9c53704d` recorded for the `18bee8e` runs. The non-portable footprint
differs between the two runs because it binds deployment-specific identity, and
the artifact manifest digest differs for the same reason; the artifact directory
digest, which is content, is identical.

The internal footprint is unchanged in all four scopes — one internal call per
attestation over one key for `constructor_or_cold_pool`,
`explicit_preflight_pool`, `single_query_transaction` and
`paired_query_transaction`, with `constructor == explicit preflight` true, over
11 snapshots and 10 intervals.

This is the live answer to the question I2-A0 raised. The specification predicted
the portable footprint should be unchanged if the move was truly byte-equivalent.
Three independent checks now agree that it was: the direct comparison against the
pre-move implementation over eleven statements before the old file was deleted,
the golden vectors pinning the digest space, and this measurement through two
fresh topologies. The N4 evidence produced before the move stays valid rather
than merely tolerated.

Two process failures preceded the passing runs, both recorded rather than
retried silently. The first was a port collision: the standing `db-test-env`
deployment holds `127.0.0.1:25534`, which the qualification's
`final-v5-direct-postgres` also binds, so phase 1 failed at container
networking. The second was a dirty worktree — the probe refuses to qualify
against an unpublished tree, which is what makes a qualification checkable by a
reviewer holding only the commit. Both retained directories remain under
`raw/`.

### The agreement, and what was actually compared

`raw/attestation-footprint-i2a-agreement.json`. The table above was written from
the two `qualification.log` files; the agreement is computed from the two
`attestation-footprint-v2.json` reports instead, so the checkpoint does not rest
on a log line agreeing with itself. Every equality below was recomputed and had
to hold before the file was written:

- **Portable footprint** — `032e9c53704d`, identical.
- **Every scope/key multiset** — compared as multisets of
  `(strict_ast_sha256, calls_per_attestation)`, not as counts. The scope name
  sets match and all four scopes carry the single key
  `e5738df165027…` at one call per attestation, with `scope_stable` true
  throughout, `constructor == explicit preflight` true and
  `internal_key_count` 1 in both runs.
- **ExpectedSchema** — digest `e2a3796fb3f5`, E=1, and the relation list itself
  (`reporting.final_v5_result_heavy`), not merely the digest over it.
- **PostgreSQL portable identity** — image reference, repo digest and platform
  identical, plus the whole `measurement_environment`
  (`server_version_num=160014`, `track=all`, `track_utility=on`,
  `track_planning=off`). The deployment-local `local_image_id` and
  `container_image_id` are compared but not required to agree; both runs share a
  host, so they happen to match too.
- **Source dependencies** — `source_manifest_sha256` `037cf38fdcab…` plus each
  of the 8 bound paths compared entry by entry. This manifest differs from the
  `18bee8e` runs' `3495d7f0fbee…` exactly because the strict-AST move changed
  the bound set; that is the change the requalification exists to cover.
- **Profile binding** — every field identical except
  `profile_artifact_manifest_sha256`, which binds deployment identity and is
  designed to differ; the artifact *directory* digest `814d4df9971f`, which is
  content, is identical.

Both runs also re-asserted `publication_eligible`, `capability_changing`,
`activation_support_changing` and `formal_campaign` all false, and
`worktree_clean` / `head_equals_origin` / `live_schema_verified` all true. The
agreement supersedes `raw/attestation-footprint-v15-candidate-agreement.json`,
which is retained.

No source fix was required, so no requalification from a new commit was needed.

**What is committed, and what is only retained.** Each run directory is 5.2 GB
on disk. What is committed is the seven-file report set per run — the same set
the `18bee8e` qualifications committed, plus `compose-up.log`, which is 20 KB of
real container-level run evidence and costs nothing to keep. Both were checked
for credentials before being added, as `raw/.gitignore` is `*` and evidence has
to be force-added.

The two bulk trees are deliberately not committed and stay in the worktree:
`profile-artifacts/` (1.9 GB), every file of which is bound by `sha256` and
`bytes` in the committed `profile-artifact-manifest.json` under directory digest
`814d4df9971f`; and `snapshot-index-artifacts-full/` (3.4 GB), bound by
`source_artifact_root_sha256` `6330d00e55f9`. A reviewer holding the commit can
therefore check every artifact identity the qualification asserts; committing the
bytes would add 10.4 GB and no checkable fact.

## SQL-executability gate — PASS, live

Rerun at `1a539e1` after the I2-A changes: 28 artifacts, 71 rendered cells, 0
failed, against a disposable digest-pinned PostgreSQL 16.14, with the committed
manifest regenerating byte-identically (the worktree stayed clean).

### Original run

`evaluation/final-v5-wsl2/scripts/run-sql-executability-gate.sh`: 28 artifacts,
71 rendered cells, 0 failed, against a disposable digest-pinned PostgreSQL 16.14.
That script exists because the gate needs a database initialized the way the
deployment initializes it but otherwise EMPTY — the generator creates
`final_v5_benchmark` itself, so pointing the check at the standing test
environment fails with "schema already exists". `validate.sh` still reports
SKIPPED on its own, which remains not-evidence; this run is the evidence.

## Next executable step

I2-A is complete and proved live at `76aef1f` — the Gateway constructs
`QueryExecutionBindingV1` from the `physicalquery.Derive` decisions it executes,
writes it in the terminal settlement transaction, and `QueryReceiptEvidence`
loads it so the receipt is signed as V9. The N4 checkpoint above is closed.

### The v3 accounting stack has no production caller

Established by inspection at this HEAD, and it resizes everything below.
`FinalizeObservationV3`, `CarriedEvidenceV3`, `IndependentInputsV3`,
`GatewayControlPlanV3` and the four `…PlanV3` constructors are referenced from
their own tests and from nowhere else. The one non-test reference is
`ObservedDelta.Accept`, inside the v3 files themselves.

What the Adapter and the finalizer actually run is still v1.4.
`applyObserverDelta` (`adapter_bindings.go:298`) builds an
`experiment.ObserverAccounting` around a v1.4 `GatewayControlPlan`, and the
finalizer reaches it through `validateObserverTransition` →
`validateSampleObserverAccounting` (`finalize_scale_artifact.go:490,497`), which
reads `sample.ObserverAccounting.Plan` — the plan the Adapter supplied.

That is defect 7 of `docs/final_v5_observer_accounting_v14_audit.md` verbatim:
the finalizer checks that the plan is internally consistent and matches the
observed counts, but never derives the expected plan independently, so *a wrong
plan the Adapter also measured against passes*. `docs/final_v5_observer_
statement_accounting.md` already records that v1.4 "is invalid and was never
exercised live" and that its claim of finalizer-side re-derivation "is also
wrong". The replacement was written — `c43b7ba`, "E — independent finalizer" —
but never wired in. Its doc comment, "the finalizer derives first and looks at
the Adapter's claims only to reject them", describes code nothing calls.

So I3 is not "harden the finalizer". I3 is **cut the live path over to the v3
finalizer and retire the v1.4 accounting**, and I4 is the same migration seen
from the three call sites that must feed it. They are one change, not two: the
v3 finalizer cannot be wired in until Artifact, Scale and ProvSQL emit
`CarriedEvidenceV3` instead of `ObserverAccounting`, and emitting it is most of
the work — it needs the observer window as `ObserverWindowV2`, the classifier
manifest and binding digests, and the *signed* visible/companion
`physicalquery.StatementIdentity` pair, which is exactly what I2-A made
available and `9dffac8` began reading.

`IndependentInputsV3` also has to be assembled by the finalizer from sources the
Adapter never touches: the activated Catalog path, the qualified
`AttestationFootprintV2` from its own retained evidence, the live PostgreSQL
runtime identity, the path kind taken from the Gateway's signed receipt, the
frozen contract identity, and the visible/companion SQL reproduced through
`internal/physicalquery` from signed pre-state.

**This blocks the canary.** A "100x4 v3 canary" run against the current tree
would measure the v1.4 path, because that is the only accounting path the
running code has. It would produce a green result that is not evidence for the
v3 accounting, which is the specific failure mode this arc exists to remove. The
formal Gateway rebuild is downstream of the same migration, since the image must
come from the fully integrated commit.

### V3 runtime integration — state

Six commits land. Capability, release, tags, Campaign and paper state are
untouched, the canary has not run, and the boundary
`V3 RUNTIME INTEGRATION PASS — CONTRACTS V1.5 FREEZE PENDING` is **not**
declared, because its first prerequisite is that all thirty gates pass.

| commit | what |
| --- | --- |
| `35f77c6` | the gate contract completed with the supplied requirements; gap notice resolved |
| `e8128ef` | `internal/testfixture/queryreceiptv9`, a signed V9 fixture built only from public API |
| `16cedfb` | gates 7, 18, 19, 21, 25, 29 implemented; gate 22's blocker pinned |
| `a3e7b7a` | the Adapter is guarded out of constructing the finalizer's trusted inputs |

**29 of 30 gates PASS. Gate 22 is BLOCKED**, and not for want of work: the v3
model cannot finalize an exact request-ID replay at all, because two of its own
invariants contradict each other on that path. `CompileClassifier`
presence-couples attestation in both directions, so a non-attesting operation
must name neither an ExpectedSchema nor a footprint and cannot compile against a
manifest whose internal entries name a qualification it does not claim. But
`ClassifierManifest.Validate` requires an entry for every class in
`requiredManifestClasses()`, which includes `postgresql_internal_attestation`
unconditionally, and the footprint is the only source of internal keys. The
manifest must carry those keys and the operation must not claim where they came
from.

Each way out changes what a manifest or an operation identity *means*, so it is
an author decision and is recorded as one in the gate document. A speculative
`BuildNonAttestingClassifierManifest` was written, found to hit the same wall at
`Validate`, and reverted rather than left as dead code. The conflict is pinned
by a test that fails the moment it is resolved.

**Three defects the gates found while being written**, each recorded rather than
silently fixed:

- the prepared target binding was signed and never compared, so a receipt
  re-sealed around a different prepared target was accepted. `CarriedEvidenceV3`
  now carries both roles' prepared bindings and acceptance compares them;
- gate 7's disappearing-structural-key branch existed and had no test;
- gate 22's conflict above.

**What is still not done**: the runtime cutover. The three TaskGate call sites
still build v1.4 accounting, so nothing calls the acceptance wrapper and
`retiredV14ActiveReferences` is not yet empty. The ratchet pins the remaining
surface file by file and can only tighten.

#### Superseded by the above

Three commits land against the author's decisions. None of them changes
capability, release, tags, Campaign or paper state, and the canary has not run.

- `624b295` — `docs/final_v5_v3_runtime_integration_gates.md`, the gate list,
  with stable numbering and no renumbering of existing IDs.
- `bcd2f1c` — `FinalizeTaskGateObservationV3`, the V9-verified production
  acceptance entry point. It verifies the receipt before reading anything out of
  it, takes the path kind and the signed target records from the Gateway's
  signature, compares the Adapter's carried statement identities against those
  records field by field — including the row limit and the policy fingerprint,
  neither of which a structural digest can see — and only then calls
  `FinalizeObservationV3`. The Adapter's verdict has no parameter to arrive
  through.
- `65ac2e7` — the retirement ratchet and the production-caller report.

**The 24 unsupplied gates.** The instruction stated the authoritative 30-item
list was supplied with it. It was not: requirement text arrived for 18, 19, 21,
22 and 25 only, and gate 1 is recoverable from this record. The rest are
recorded `UNSPECIFIED` rather than invented — see the gap notice in the gate
document. This makes canary prerequisite 1, "all 30 gates pass", currently
unsatisfiable, independently of how much code lands.

**What is not done**, and is the whole of what remains:

1. The three TaskGate call sites still build v1.4 `ObserverAccounting`. Nothing
   calls the acceptance wrapper yet, so `FinalizeObservationV3` still has no
   production caller — the condition this arc exists to fix. The remaining
   active surface is listed file by file in the gate document and pinned by the
   ratchet.
2. `evaluation/internal/legacyv14` does not exist; the v1.4 schema and decoder
   have not been moved, and the import guard is unwritten.
3. Gate tests 18, 19, 21, 22 and 25 are unwritten. They need a signed, valid V9
   receipt fixture inside `evaluation/internal/experiment`, and the existing one
   is an eight-deep unexported chain (`validReceipt` → … → `validV9Receipt`) in
   package `queryreceipt` that cannot be reached from there.

**A finding that makes the migration unavoidable rather than merely desirable.**
The v1.4 active path cannot execute at all against the current observer, and has
not been able to since I1-B. `captureBoundObserver` decodes the observer's
output into the v1 `ObserverSnapshot` through `StrictJSON`, which sets
`DisallowUnknownFields`, while the observer emits `ObserverSnapshotV2` with no
fallback; the two field sets are nearly disjoint, so the decode fails on the
first member. The adapter also invokes the observer with `--phase` alone, while
`parseObserverInvocation` requires `--observer-window-id` and
`--classifier-manifest-sha256` and rejects unknown flags. Either fault alone is
fatal. So removing the v1.4 path breaks nothing that currently works, and any
green Artifact/Scale/ProvSQL run predating I1-B measured a path that no longer
exists.

### Remaining order

1. **Supply the 24 missing gate requirements.** Nothing downstream can be
   declared publication-grade without them, and no amount of code closes them.
2. **Finish the migration**: move Artifact, Scale and ProvSQL onto
   `CarriedEvidenceV3` and `FinalizeTaskGateObservationV3`, assemble
   `IndependentInputsV3` finalizer-side, drive the observer with the window and
   classifier identities it actually requires, and empty the ratchet.
3. Move the v1.4 schema and decoder into `evaluation/internal/legacyv14` with
   the import guard, incapable of producing or accepting a v1.5 sample.
4. Write gate tests 18, 19, 21, 22 and 25 against the acceptance wrapper.
5. Then the formal Gateway build from the integrated commit, and only then the
   live Result-heavy 100x4 v3 canary.

Integration-gate items already covered by tests: 1 (observer emits strict v2
JSON — done in I1-B), 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 20,
23, 24, 26, 27, 28, 29, 30. Not yet covered: 18, 19, 21, 22, 25.

**The numbered gate list itself is not in the repository.** It is referenced here
and defined nowhere under `docs/`, so items 18, 19, 21, 22 and 25 cannot be
worked from this record alone. Whoever continues either restores the list into
`docs/` — where it belongs, being an acceptance criterion for publication
evidence — or states the five items so they can be closed against something
checkable. This is a recording gap, not a technical blocker.

## Retained true blockers

None. No author decision is outstanding.

Watch items, all ordinary technical work:

- `validate.sh` reports `contract SQL executability: SKIPPED
  (TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN is not set)`. A skip must not count as a
  pass at freeze time, so v1.5 freeze requires this gate run with the DSN set.
- The three retained failure directories under
  `evaluation/final-v5-wsl2/raw/` record the two Gateway startup findings above.
  `raw/.gitignore` is `*`, so evidence is force-added when it is worth keeping;
  these were checked for credentials before being committed.
- `result-object-store` and `result-object-store-init` still use MinIO
  `RELEASE.*` tags rather than digests. Conventionally immutable, and they touch
  no PostgreSQL statement accounting, so they are out of scope for the
  Attestation footprint — but they are not yet digest-pinned.
- Docker availability has moved twice. It was present, then absent for a whole
  session (Docker Desktop WSL integration off, `docker` not on PATH), and is
  present again: client 29.1.3, server Docker Desktop 4.55.0, Compose v2.40.3.
  Go 1.25.12 on the host. Anything Docker-dependent should re-check
  `docker version` rather than assume.
- There is no host PostgreSQL server and none should be installed: the
  repository's digest-pinned PostgreSQL 16.14 containers are the frozen
  environment the whole accounting is qualified against, and a host install
  would be a different server. `scripts/db-test-env.sh` brings up that
  environment and exports the DSNs.
- **Five `internal/gateway` tests fail against a real control store, and did so
  before this work.** Verified identical at `3d1eea9`, the accepted pre-session
  boundary, so nothing here caused them; they were simply never run, because
  they skipped for want of `CONTROL_TEST_POSTGRES_DSN`.

  `TestDelegatedTaskSharesRootExposureAndStopsWithParent`,
  `TestSQLAndExecutePlanShareV4SemanticReplayAfterConsumedRowBudget`,
  `TestOrdinalExposureBudgetBPlusOneCommitsCompleteFailureOnly`,
  `TestCanonicalCopySurvivesAvailableTransactionFailureAndRecoversExactlyOnce`,
  `TestExecutePlanSemanticViewCarriesRegistryExpectationToPairedQueries`.

  All five fail persisting the `provsql-orders-v1` publication. The harness
  stands that publication in with `liveCompilerTestSnapshotIndex`, because its
  rows are deliberately not checked in -- production activates only the
  independently verified live HOT bundle. The double's manifest omits the
  cold-payload and hot-index digests, and `DictionaryManifest.Validate` requires
  all five, so the control store rejects it.

  The chain bottoms out and cannot be closed from inside the test: filling the
  two digests from the input's `expected_digests` gets past that check and hits
  `dictionary has no segments`; real segments need real fact counts; those need
  the rows that are deliberately absent. Skipping the store write instead moves
  the failure into production, where `PutOrdinalDictionarySet` then reports
  `V4 dictionary set 无法按 Catalog 证据发布` — the dictionary row it needs is the
  one that was skipped. Both attempts were made and reverted rather than left
  in place, and no fixture was fabricated: a manifest digest that did not
  reproduce the Catalog's would assert a publication that never existed.

  Closing this needs a compiled `provsql-orders-v1` fixture, or a harness that
  installs publications the way `snapshot-sidecar-install` does. It is
  independent of I1/I2 and is not on the critical path to the canary.

  Incidental finding while diagnosing: `DictionaryManifest.Validate`
  (`internal/ordinal/dictionary.go:68`) reports the first invalid digest while
  iterating a **map**, so the same failure names "cold payload digest" or "hot
  index digest" at random between runs.

## Capability and release state

Unchanged by this work, and deliberately so — nothing here is publication
evidence.

- Contract release **v1.4** (`contracts/index-v1.json`); no v1.5 material
  exists yet.
- Artifact capability **false**; overall **6/9**.
- `artifactRealSystemValidated` **false**.
- All 11 registry profiles `activation_supported=false` /
  `targeted_run_eligible=false`; `validate.sh` confirms the activation support
  manifest is absent.
- Result-heavy coverage unchanged; `targeted_validation_passed` false.
- `.NOT_READY` intact; no formal Campaign ID; paper unchanged.

## Gate results at this HEAD

`gofmt` clean · `git diff --check` clean · `go build ./...` ok ·
`go vet ./...` ok · `go test -count=1 ./...` ok ·
`validate.sh` exit 0 (with the SQL-executability skip noted above).

Re-run at the N4-checkpoint commit. That commit changes no Go source — it adds
the two evidence sets, their agreement and this record — so the gates confirm
the tree is unchanged rather than proving anything new about the runtime. The
DB-backed tests skipped again here, for want of their DSNs; the live evidence
for this checkpoint is the two qualifications themselves, not this suite.

Previously run at `50c3cb8`, `e3622a5` and `865ae8c` in turn. The DB-backed tests
(`internal/control`, `internal/dataconnector`, `internal/gateway`,
`evaluation/security`) **skipped** at all three, so the physicalquery delegation
in `865ae8c` is verified by the unit and policy tests only. A green
`go test ./...` at those commits is not evidence about database behaviour.
