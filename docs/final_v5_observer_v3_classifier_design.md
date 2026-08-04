# observer-accounting-v3: structural classifier design constraints

Findings that constrain Stage C3, established by probing `pg_query_go/v6`
against the exact Connector SQL constants and the statements Stage B observed
live. They are recorded before the classifier is built because two of them
change what the classifier can be asked to guarantee.

## 1. `pg_query.Fingerprint` is too loose to be the classification key

`pg_query.Fingerprint` is designed to group *similar* queries: it deliberately
collapses differences in list length, so that `IN (1,2)` and `IN (1,2,3)` share
a fingerprint. That property is disqualifying here.

Measured:

| statement | `pg_query.Fingerprint` |
| --- | --- |
| safety pin (two `set_config` calls) | `61f69c00d1e1fe3e` |
| statement-timeout pin (one `set_config` call) | `61f69c00d1e1fe3e` |

Two different control classes, one fingerprint. A classifier keyed on
`Fingerprint` would map one key to two classes, which the Stage C3 rule
"a fingerprint must map to exactly one class" forbids outright.

**Use instead** a strict normalized-AST digest: `pg_query.Normalize` to replace
constants, `pg_query.ParseToJSON` to obtain the full parse tree, canonicalize
the `location` fields that carry byte offsets, then a domain-separated SHA-256
over the result. This preserves target-list length, CTE structure and quoted
identifier semantics.

Measured with that construction:

| statement | strict digest (16-hex prefix) |
| --- | --- |
| safety pin | `7ce6f7cda809b5fe` |
| representation pin | `542f3da1910fdfdd` |
| statement-timeout pin | `302215f6756216a9` |
| nested `pg_rewrite` lookup | `0890db45c9b59a0c` |

All four separate, including the two pins that contracts v1.5 structurally
domain-separated.

## 2. Constants are erased before observation, so decoys are a counting problem

The classifier does not see the SQL the Connector wrote. It sees what
`pg_stat_statements` retained, and that has already replaced every constant --
including GUC *names* -- with placeholders. Stage B observed the safety pin as:

```text
SELECT pg_catalog.set_config($1, $2, $3), pg_catalog.set_config($4, $5, $6)
```

Consequently an arbitrary two-GUC `set_config` is structurally identical to the
safety pin and shares its digest:

| statement | strict digest |
| --- | --- |
| safety pin | `7ce6f7cda809b5fe` |
| `set_config('a','b',true), set_config('c','d',true)` | `7ce6f7cda809b5fe` |

No parser-based classifier can separate these, because the distinguishing
information is destroyed by the observation source before the classifier runs.

This does **not** mean a decoy passes. It means the decoy is caught by the
*accounting* rather than the *classification*: a decoy two-GUC `set_config`
lands in `safety_session_pin` and pushes that class above its derived
multiplicity `S + Q`, so the run fails on the per-class count. Requirement
"arbitrary two-GUC set_config is unexpected" is therefore satisfied at the
accounting level, and the test for it must assert a failed accounting, not an
`unexpected` classification.

The same reasoning applies to a one-GUC decoy against `statement_timeout_pin`.

**This is exactly why Stage C1 mattered.** Before contracts v1.5 the safety and
representation pins *also* shared a digest, so one could be substituted for the
other with no count change at all — invisible to both the classifier and the
accounting. Structural domain separation removed the only substitution the
counting could not catch.

## 3. What the classifier must therefore guarantee

- Control digests are generated from the exact exported Connector constants
  (`dataconnector.SafetySessionPinSQL`, `dataconnector.RepresentationPinSQL`,
  and the timeout-pin and attestation statements), never from re-typed copies.
- Targeted visible and companion identities come from the exact frozen rendered
  query contracts, not from relation-name matching.
- `toplevel` is part of the classification key: the nested `pg_rewrite` lookup
  is only valid with `toplevel=false`, and a top-level statement carrying that
  digest is not the nested lookup.
- Zero matches is `unexpected`; more than one match is ambiguous and fails.
- `queryid` is recorded as a deployment-local diagnostic only, never as the
  portable identity — Stage B already noted it is version and installation
  specific.
- The classifier manifest carries its own domain-separated digest, bound into
  the evidence.

## Status

Constraints established; the classifier itself is not yet implemented. Stages
C1 and C2 are complete and committed. See
`docs/final_v5_observer_accounting_v14_audit.md` for why v2 is superseded.
