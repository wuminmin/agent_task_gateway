# Final-V5 contract amendment v1.5

Previous release:
final-v5-contracts-v1.4

New release:
final-v5-contracts-v1.5

## Why this release exists

Two contract *statements* were inaccurate. Neither gate code nor executed bytes
change in this release: no query, no normalizer, no oracle, no plan. This
amendment corrects what the contract text claims, and pays the price a release
bump always costs.

## Correction 1 — the S5 relation fragment is declared at its measured extent

v1.4 declared the Baseline S5 allowed claim as:

```text
registered two-branch UNION DISTINCT behavior
```

That wording predates the qualified-column refactor. At the time it was
written, a two-branch UNION DISTINCT carrying *branch-local predicates* could
not be prepared at all: `queryplan.PredicateBindings` keyed the predicate
footprint by product name, so `left_branch.expense_type` resolved to nothing
and preparation failed closed with `POLICY_DENIED`. The declared fragment and
the supported fragment therefore differed, and the contract named the wider
one.

Author decision 16 chose the qualified-column refactor over restating S5 or
excluding the arm. The refactor landed: the predicate footprint is keyed by the
exact `(branch role, product)` composite, `CompileRelational(...).Sources`
supplies it through a single helper shared by ordinary relational lowering, the
semantic View and the compiler identity probe, and the left and right UNION
branches of the same product keep independent role atoms. The closing tests
require exactly one V5 predicate atom on each of `left_branch` and
`right_branch`, in production-vs-`Prepare` parity and in the reproducibility
loops.

The claim is therefore regenerated as:

```text
registered two-branch UNION DISTINCT behavior with branch-local predicates
```

applied to all eight S5 cells (four SF1, four SF10).

**The wording still does not exceed measured coverage.** The prohibitions are
unchanged and still binding: `arbitrary set-operation support` and
`BDG SQL-union entrypoint` remain prohibited. This release declares one
additional, actually-exercised property of an already-registered shape; it does
not widen the shape.

## Correction 2 — decision 10's rationale replaces a grammar rationale

The S2 workload spec in `contracts/baseline-v1.json` justified the canonical
ordering by a property of the lowering:

```text
... because production multi-source lowering forbids ORDER BY
```

That is a statement about what the production grammar will not emit. It is the
wrong reason. Author decision 10's authoritative prose rests on a property of
the frozen queries themselves: they define no total result order, and therefore
do not rely on returned row order. A rule that depended on the grammar would
have to be revisited every time the lowering gained an ability; a rule that
depends on the queries does not. Regenerated as:

```text
... because this frozen query defines no total result order and therefore does
not rely on returned row order
```

### Scope of correction 2, and what it deliberately does not touch

The same historical rationale also appears in the `total_order_rule` of
`contracts/result-normalization-v1.json`. **v1.5 does not regenerate those
bytes**, and the correction is therefore confined to the frozen baseline
metadata.

The reason is not cosmetic. Every one of the 135 oracle manifests pins
`normalization_spec_sha256` to the indexed digest of the normalization
contract, and the bridge rejects any manifest whose pin is not the indexed
digest. Regenerating that one prose string therefore forces all 135 manifests
to be regenerated against a live PostgreSQL, and their new digests are oracle
inputs to the publication binding. That is a live oracle regeneration, not a
text edit, and it is not what a text correction should smuggle in.

`TestBenchmarkS2S4S5QueriesAndCanonicalNormalizerRemainFrozen` continues to pin
the v1.4 `result-normalization-v1.json` bytes, and
`TestResultNormalizationContractIsCompatible` continues to pin the v1.4
`total_order_rule` literal. A future release may regenerate them, together with
the manifest set they govern.

## What did not change

**The ordering modes themselves.** `query_order_v1` and
`canonical_typed_row_lexicographic_v1` keep their identifiers, their
assignments to workloads, and their definitions. Which normalizer runs for
which workload is byte-for-byte what v1.4 specified.

**The S2, S4 and S5 query bytes.** `sql/contracts/*.sql` is untouched. A
rationale correction that also moved a query would be indistinguishable from
changing the measurement, so the two are kept separable and the queries are
left alone. `TestBenchmarkS2S4S5QueriesAndCanonicalNormalizerRemainFrozen`
still pins all six query/plan artifacts and both normalizer implementation
files to their original historical constants across this release; only the
`contracts/baseline-v1.json` pin was regenerated.

Also unchanged: Dataset logical content; probe output schema and meaning;
Products; Publications; Profile closures and IDs; Query Contracts; Oracles;
workload cells; warmups; measured samples; statistics; the 160 MiB ceiling;
FactID; Receipt; Exposure settlement; PENDING/AVAILABLE; the observer runtime
schema, source-build manifest and digest binding; and the closed-world observer
accounting introduced by v1.4.

One of the 28 indexed artifacts changes bytes and was recomputed:
`contracts/baseline-v1.json`. The other 27 digests are unchanged. The
hash-locked protocol, workload manifest, acceptance rules and statistics
references are byte-identical to v1.2.

## Activation support does not carry across this release

`ActivationSupport.ContractRelease` pins the contract a smoke ran under, and
activation support deliberately does not survive a release change. The recorded
smokes ran under v1.4, so `config/profiles/activation-support-v1.json` has been
removed rather than retagged: retagging would claim the probes executed under a
contract they never saw. The route-matrix and semantic-cache-isolation evidence
records are left pinned to v1.4 for the same reason.

The registry was regenerated with the manifest absent, which is the designed
path rather than an escape hatch — an absent manifest yields an empty support
set. All eleven profiles are therefore `activation_supported=false` and
`targeted_run_eligible=false` under v1.5.

This is self-enforcing: `resolveArtifactProfileBinding` refuses to produce a
binding for a profile that is not targeted-run eligible, so no Artifact run can
execute under v1.5 until an operator records a live activation smoke against
this release. `artifactRealSystemValidated` stays false and the artifact
capability stays 6/9 until then.

**This is the designed cost of the bump.** The P3.3 Artifact canary runs 06, 08
and 09 passed under v1.4, and that evidence keeps naming v1.4. It is not
retagged and it does not transfer.

## SQL executability record

`contracts/sql-executability-v1.json` embeds `contract_index_sha256`, which the
index bump invalidates. It is re-derived by re-running the gate against a real
PostgreSQL 16, never by editing the recorded digest.

## Publication evidence affected

None. The publication binding approved by author decision 22 is a pre-run
review candidate: it binds independent-oracle expectations and direct Business
PostgreSQL result observations, and contains no activation-dependent sample and
no publication-eligible sample. No publication-eligible sample exists under any
release.

## Execution status

v1, v1.1, v1.2, v1.3 and v1.4 are preserved for audit but superseded for
Final-V5 execution by v1.5.
