# Observer statement accounting: the derived control plan

Design note for the author-approved option (b), closed-world statement
accounting. This records the *derivation* of the required-control multiplicities
from the frozen execution path, so the expected counts are computed from
structure rather than copied from an observed number. It is the input to
`observer-accounting-v2` and AMENDMENT-v1.4; it is not itself an acceptance
rule.

## What was observed

On the Result-heavy artifact cell, one governed `query_sql` produced 16
`gateway_reader` statements against Business PostgreSQL: 2 targeted Product
statements and 14 others. `applyObserverDelta` and `validateObserverTransition`
required total == targeted, which can never hold.

## Where the 14 come from

`internal/dataconnector.Connector.Query` runs each governed statement inside one
read-only repeatable-read transaction and, before touching data, re-establishes
the controls that make the read attributable:

| # | Control class | Source |
| --- | --- | --- |
| 1 | transaction begin | `pool.BeginTx(RepeatableRead, ReadOnly)` |
| 2 | statement-timeout pin | `set_config('statement_timeout', $1, true)` |
| 3 | session pins | `set_config('search_path', …), set_config('standard_conforming_strings', …)` — one statement, two settings |
| 4 | datasource identity attestation | `attestDatasource` → `liveIdentity`: `datasource_attestation` + `current_database/current_user/server_version_num` |
| 5 | reporting-view column attestation | `attestSchemaDigest`: one `pg_attribute` query **per declared reporting view** |
| 6 | view-definition attestation | `viewDefinition`: one `pg_get_viewdef` query **per declared reporting view** |
| 7 | transaction commit | `tx.Commit` |

So per governed transaction:

```
controls_per_transaction = 5 fixed            (begin, timeout pin, session pin,
                                               datasource attestation, commit)
                         + 2 * N_reporting_views   (column + view-definition
                                                    attestation, per view)
```

A governed artifact query settles a **visible** statement and a **provenance
companion** statement, each in its own governed transaction:

```
required_gateway_controls = T_governed_transactions * (5 + 2 * N_reporting_views)
```

## Why Result-heavy is 14, and why that is not a constant

The Result-heavy profile Catalog declares exactly **one** reporting view, and the
artifact cell settles **two** governed transactions:

```
(5 + 2 * 1) * 2 = 7 * 2 = 14
gateway_reader_total = 2 targeted + 14 controls + 0 unexpected = 16   ✓
```

The observed 16 is therefore fully accounted for, with nothing left over.

The count is a function of the *activated profile*, not a global constant. A
profile declaring two reporting views yields `(5 + 4) * 2 = 18` controls and a
total of 20; the expense-family profiles, the analytics profiles and the
ProvSQL profile each derive their own value from their own reporting-view set
and View Registry closure. Hard-coding 14 would be wrong for every profile
except this one, which is precisely why the amendment forbids it.

## What the accounting must still prove

The derivation above gives per-class expected multiplicities, not just a total.
The v2 accounting therefore has to check each class separately, so that a
substitution which preserves the total — one fewer attestation, one more
unknown — still fails. That property is what makes closed-world accounting
strictly stronger than the equality it replaces, rather than a relaxation of it.

## What the View Registry adds, and why it is not in the formula

`Connector.Query` also calls `verifyViewRegistry`, which issues a server-version
pin and one discovery traversal per relation in the view closure. It is a no-op
when the request carries no View Registry expectation, and the Gateway only
attaches one when the signed Grant carries a View binding digest. The
Result-heavy profile carries none, which is why the formula above closes at
exactly 16 without a View Registry term.

This is not an assumption the accounting relies on. A profile that does reach
Business PostgreSQL through the View Registry produces statements the classifier
does not model, they land in the `unexpected` class, and the run fails. Extending
the closed world to cover those profiles is a contract change, not an
implementation detail.

## Implementation status

Implemented in `b51ad59` and frozen as `final-v5-contracts-v1.4`:
`experiment.ObserverAccounting`, `ClassifyGatewayStatement`, the three call-site
updates, nineteen mutation tests including both same-total substitutions, and
the finalizer-side re-derivation. N is read from the Catalog the Gateway signed
rather than declared.

Outstanding, and operator-gated: a live activation smoke under v1.4. Activation
support does not carry across a contract release, so every profile is currently
`targeted_run_eligible=false`, no Artifact run can execute,
`artifactRealSystemValidated` stays false and the capability stays 6/9.
