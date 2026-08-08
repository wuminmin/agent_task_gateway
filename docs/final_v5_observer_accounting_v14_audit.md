# Audit: contracts v1.4 observer accounting is invalid

**Status: V1.4 OBSERVER ACCOUNTING INVALID — FORWARD CORRECTION REQUIRED**

This is an audit record, not an amendment. It states what
`final-v5-contracts-v1.4` is and why it must not be used to produce
accounting-bearing targeted-validation or publication evidence. The release and
its tag are preserved unchanged as an immutable audit record; the correction is
made by forward commits only. Activation smoke and outside-Product refusal
evidence that stops before task issuance does not exercise or validate the
observer accounting audited here.

## Identity of the audited release

| | |
| --- | --- |
| Tag | `final-v5-contracts-v1.4` |
| Tag object | `af15ee1c6f94` |
| Dereferenced commit | `36b04ba0227cf2404d493da1466e937fea27e962` |
| Implementation commit | `b51ad59` |
| Contract freeze commit | `b5bc1e7` |
| SQL-executability commit | `36b04ba` |

Nothing above is moved, deleted, recreated or force-updated.

## What v1.4 has, and has not

At the audited tag, v1.4 had **no live activation evidence**, **no
targeted-validation evidence**, and **no publication-eligible samples**. Its
observer-accounting correctness was never exercised against a running measured
deployment. The deletion of the v1.3 activation-support manifest at `b5bc1e7`
was correct: it prevented older activation evidence from crossing a release
boundary and prevented a known-invalid accounting contract from producing a
measured sample.

A 2026-08-08 forward commit later produced fresh v1.4 activation and
outside-Product refusal evidence. All 54 requests were refused before task
issuance; the records assert no targeted validation, measured sample, or
publication eligibility. That evidence therefore does not rehabilitate or
test the invalid observer accounting described below.

The absence of accounting-bearing evidence at the audited tag is the reason
this defect is an audit note rather than a retraction.

## Defects

### 1. The transaction model is wrong

`b51ad59` models the Result-heavy path as two `Connector.Query` transactions:

```text
T = visible + companion = 2
controls = T * (5 + 2*N)
```

That is not the production path. For Exposure V4/V5 the Gateway performs a
preflight `datasourceEvidence` → `Connector.Attestation`, and then, for a novel
execution, a single `Connector.QueryPairStream`.

`internal/dataconnector/postgres.go` confirms this directly. `QueryPairStream`
(line 633) opens **one** read-only repeatable-read transaction (line 645), runs
the visible query and the provenance companion inside it, and issues **one**
commit (line 688). `Connector.Attestation` (line 352) attests against `c.pool`,
outside any transaction, so it contributes no begin or commit at all.

v1.4 therefore requires two BEGINs and two COMMITs where the system issues one
of each, and it has no representation of the preflight attestation pass.

### 2. The representation pin has no class

`QueryPairStream` executes **two distinct** double-`set_config` statements:

```sql
SELECT pg_catalog.set_config('search_path', 'pg_catalog', true),
       pg_catalog.set_config('standard_conforming_strings', 'on', true)   -- line 655
SELECT pg_catalog.set_config('TimeZone', 'UTC', true),
       pg_catalog.set_config('extra_float_digits', '3', true)             -- line 658
```

The v2 classifier distinguishes `set_config` templates by **arity** alone, so it
collapses both into a single `session_pins` class. The safety pin and the
representation pin are different controls with different meanings, and the
accounting cannot tell them apart. Classifying by arity was the wrong idea, not
merely an incomplete table.

### 3. Statement-timeout pins are misplaced

v1.4 charges one timeout pin per transaction. The pin is actually issued
immediately before each target statement, inside `queryInTx` (line 715) and
`streamQueryInTx` (line 760) — so the count follows the number of target
statements, not the number of transactions.

### 4. Semantic replay is modelled as all-zero

v1.4 gives a served-from-cache replay a plan in which every class expects zero.
The semantic cache lookup happens *after* `datasourceEvidence`, so a semantic
replay still performs a preflight `Attestation` — a non-zero number of Business
PostgreSQL statements — while performing zero target statements and opening no
pair transaction. The v1.4 replay plan would reject a correct replay.

### 5. The classifier matches on over-broad tokens

Classification is by substring containment: `pg_get_viewdef`, `pg_attribute`,
`datasource_attestation`, and bare mention of the target relation. Any unrelated
statement carrying one of those tokens is silently filed into a control or
targeted class instead of failing as unexpected. The classifier also strips all
double quotes globally, discarding quoted-identifier semantics.

### 6. Census and observer windows differ

The per-class census is taken by the Adapter over `pg_stat_statements`, while the
total is taken by the separate out-of-process observer binary. v1.4 takes
`censusBefore` and `observerBefore` at different moments, and likewise after,
then requires the two to reconcile as though they covered one interval. They do
not cover the same interval, so the reconciliation identity is not sound.

### 7. The finalizer validates an Adapter-supplied plan

`validateSampleObserverAccounting` re-runs `ValidateObserverAccounting` on the
plan the Adapter embedded in the sample. It checks that the plan is internally
consistent and matches the observed counts, but it never derives the expected
plan independently. A wrong plan that the Adapter also measured against passes.

## Arithmetic status

Source inspection of the current one-view novel Result-heavy path gives, for one
preflight pass `P=1`, one paired transaction `Q=1`, one ExpectedSchema entry
`E=1`, one visible `V=1` and one companion `C=1`:

| Class | Count |
| --- | --- |
| transaction_begin | 1 |
| transaction_commit | 1 |
| safety_session_pins | 1 |
| representation_pins | 1 |
| statement_timeout_pin | 2 |
| datasource_identity | 2 |
| view_column_attestation | 2 |
| view_definition_attestation | 2 |
| **controls** | **12** |
| targeted_visible | 1 |
| targeted_companion | 1 |
| **total** | **14** |

The historically observed window was **16**. This provisional model does not
explain it, and **the remaining statements must be located rather than absorbed
by adjusting a multiplier**. Until a live per-`queryid` diagnosis reconciles the
delta with `unexpected=0`, no replacement accounting may be frozen.

Both v1.4's 14-control answer and this 12-control answer reach a total close to
the observation by different wrong routes. That coincidence is precisely why the
derivation must be closed against live per-statement evidence and not against
arithmetic.

## Retained state

| | |
| --- | --- |
| `artifactRealSystemValidated` | false |
| artifact capability | false |
| overall capability | 6/9 |
| all profiles `activation_supported` | false |
| all profiles `targeted_run_eligible` | false |
| `.NOT_READY` markers | both retained |
| Publication Campaign ID | none |
| paper | unchanged |

## Correction path

1. Preserve v1.4 and its tag as an immutable audit record.
2. Declare it superseded before live activation.
3. Diagnose the actual 16-statement window per `queryid` against a live
   deployment, under a non-publication diagnosis identifier.
4. Implement `taskgate-final-v5-observer-accounting-v3` as a new version rather
   than silently repairing the semantics of the tagged v2.
5. Freeze `final-v5-contracts-v1.5` by forward commits only, with the observer
   contract expressed as an indexed machine-readable artifact rather than living
   only in Go code and prose.

See `docs/final_v5_observer_statement_accounting.md` for the superseded
derivation, and `evaluation/final-v5-wsl2/contracts/AMENDMENT-v1.4.md` for the
release record this audit invalidates.
