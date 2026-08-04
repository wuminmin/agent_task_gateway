# Final-V5 contract amendment v1.4

Previous release:
final-v5-contracts-v1.3

New release:
final-v5-contracts-v1.4

## Defect

The out-of-process observer gate required the observer's total `gateway_reader`
statement delta to equal the targeted visible/companion counters:

```go
if delta.BusinessSQLDelta != expectedBusinessSQL {
    return fmt.Errorf("observer total Business SQL delta %d differs from "+
        "targeted visible/companion counters %d", ...)
}
```

That equality can never hold on a governed deployment. `Connector.Query` runs
each governed statement inside a read-only repeatable-read transaction and, before
touching data, re-establishes the controls that make the read attributable. The
observer counts every `gateway_reader` statement; the targeted counters count
only the Product relations. The total therefore exceeds the targeted count by
exactly the controls, always.

On the Result-heavy artifact cell one governed `query_sql` produced 16
`gateway_reader` statements: 2 targeted and 14 controls. The gate rejected the
run. The rule was not merely too strict — it was unsatisfiable by any correct
execution of the system it was gating.

## Correction

Replace the equality with closed-world statement accounting
(`taskgate-final-v5-observer-accounting-v2`). Every `gateway_reader` statement is
assigned to exactly one class, and each class is compared with a multiplicity
derived from the activated profile.

The derivation is recorded in `docs/final_v5_observer_statement_accounting.md`
and reproduced here. Per governed transaction:

| Control class | Multiplicity |
| --- | --- |
| transaction begin | 1 |
| statement-timeout pin | 1 |
| session pins (`search_path`, `standard_conforming_strings`) | 1 |
| datasource identity attestation | 1 |
| reporting-view column attestation | 1 × N |
| view-definition attestation (`pg_get_viewdef`) | 1 × N |
| transaction commit | 1 |

giving

```text
required_gateway_controls = T * (5 + 2 * N)
```

for T governed transactions and N declared reporting views.

**The count is a function of the activated profile, not a constant.** The
Result-heavy profile declares one reporting view and settles two governed
transactions — the visible statement and its provenance companion, each in its
own transaction — so it derives `(5 + 2) * 2 = 14`, and
`2 targeted + 14 controls + 0 unexpected = 16`, closing the books exactly. A
two-view profile derives `(5 + 4) * 2 = 18` and a total of 20. **Hard-coding 14
is forbidden**: it would be wrong for every profile except this one.

N is read from the Catalog the Gateway signed into the Receipt, by locating the
registry profile that pins that digest and verifying the Catalog bytes hash to
it. It is an observation of what served the query, never a declared constant.

## This is not a relaxation

Replacing an equality with an accounting could be a way to make a failing gate
pass. It is not, and the difference is checkable.

The v1 rule compared one number with one number. The v2 rule compares **each
class separately** and then requires the classified total to equal the total an
independent, source-built, digest-bound observer process measured over the same
window. A substitution that preserves the total — one fewer attestation and one
more unmodelled statement — passes a totals-only rule and fails this one.
`TestSameTotalSubstitutionIsReportedByClass` proves exactly that, and requires
the rejection to name the offending class.

Nineteen single-point mutations of a valid record are rejected, including both
same-total substitutions (an attestation replaced by an unmodelled statement,
and a `begin` replaced by a `commit`). The v1 rule accepted neither more nor
fewer real executions than v2 — it accepted *none*.

## Fail-closed classification

`ClassifyGatewayStatement` matches normalized `pg_stat_statements` templates.
The control set is closed and is tested before the targeted relations, so a
control statement can never be mistaken for a Product read. `pg_stat_statements`
normalizes the two `set_config` pins' setting names into placeholders, so they
are distinguished by arity — the timeout pin sets one GUC, the session pin sets
two in one statement. The view-definition attestation embeds its own
`set_config` and is therefore matched first.

Anything the classifier does not recognise lands in `unexpected`, which **no
plan ever expects**. A profile that reaches Business PostgreSQL by a path this
derivation does not model — a View Registry closure, for instance — fails the
accounting rather than being absorbed into a control class. Extending the
closed world is a contract change, not an implementation detail.

## Disclosure posture unchanged

Normalized templates are classified in process and never enter the evidence;
only per-class counts are retained. `pg_stat_statements` has already replaced
every constant with a placeholder, so no workload literal — and therefore no
business value — is present in the text the classifier sees.

## Independent re-derivation

The finalizer re-derives the accounting rather than trusting the Adapter's
verdict. A sample whose accounting was never settled, or was settled against a
different observer transition, or whose plan describes a different execution than
the sample records, cannot reach a published cell.

## Discovery point

The defect was discovered during the Artifact targeted validation on the
Result-heavy profile. Every requested cell was retained as a failure. No formal
Publication Campaign had run and no publication-eligible sample existed.

## Unchanged

- Dataset logical content
- Probe output schema and meaning
- Products
- Publications
- Profile closures and IDs
- Query Contracts
- Oracles
- workload cells
- warmups
- measured samples
- statistics
- 160 MiB ceiling
- FactID
- Receipt
- Exposure settlement
- PENDING/AVAILABLE
- observer runtime schema, source-build manifest and digest binding

Every artifact the Contract Index names is byte-identical to v1.3; all 28
indexed digests were recomputed and are unchanged. The hash-locked protocol,
workload manifest, acceptance rules and statistics references are byte-identical
to v1.2. This amendment changes gate code only.

## Activation support does not carry across this release

`ActivationSupport.ContractRelease` pins the contract a smoke ran under, and
activation support deliberately does not survive a release change. The recorded
smokes ran under v1.3, so `config/profiles/activation-support-v1.json` has been
removed rather than retagged: retagging would claim the probes executed under a
contract they never saw. The route-matrix and semantic-cache-isolation evidence
records are left pinned to v1.3 for the same reason.

The registry was regenerated with the manifest absent, which is the designed
path rather than an escape hatch — an absent manifest yields an empty support
set. All eleven profiles are therefore `activation_supported=false` and
`targeted_run_eligible=false` under v1.4, including Result-heavy.

This is self-enforcing: `resolveArtifactProfileBinding` refuses to produce a
binding for a profile that is not targeted-run eligible, so no Artifact run can
execute under v1.4 until an operator records a live activation smoke against
this release. `artifactRealSystemValidated` stays false and the artifact
capability stays 6/9 until then.

## Publication evidence affected

None. No publication-eligible sample was produced under v1.3.

## Execution status

v1, v1.1, v1.2 and v1.3 are preserved for audit but superseded for Final-V5
execution by v1.4.
