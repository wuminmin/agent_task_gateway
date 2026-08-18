# P54 Scale decision-chain hash-domain audit

This is a zero-deployment audit of the Scale decision chain at
`9c629e9c0bbc6b3345d46e573a2778870a912d57`. It extends the P51 dependency
domain inventory in `docs/p51_scale_oracle_audit.md` and the P52 result-preimage
inventory in `docs/p52_digest_preimage_audit.md`. “Direct” below means byte
equality is valid because both operands have the same canonical preimage;
“link” means equality is established member by member while the two aggregate
identities remain separate.

## Domain legend

| Code | Domain and canonical source |
| --- | --- |
| R-T | typed logical result: `finalv5contracts.NormalizeBDG`, fixed schema plus canonical typed rows |
| R-L | legacy JSON-shaped result: `experiment.CanonicalResultHash`; retained only for the ProvSQL three-arm contract, where expected and observed use the same reducer |
| S-S | role-bound semantic ordinary set: `finalv5oracle.SummarizeSemanticSet` over sorted canonical Fact hashes |
| S-O | production ordinal/hybrid set: Control `v4_bitmap_sets` plus immutable bitmap containers and activated HOT/COLD dictionaries |
| S-R | production radix Outcome set signed by the receipt; never compared with S-S |
| O-F | five-member ordinary Outcome candidate generated with the complete frozen binding Catalog identity |
| O-P | the same fixed five-member model generated with the activated profile Catalog identity |
| ROOT | canonical root-ledger tuple digest over production release/dependency/outcome identities |

## Complete Scale comparison inventory

| Stage | Object / dimension | Left operand | Right operand | Classification after P54 | Evidence location |
| --- | --- | --- | --- | --- | --- |
| adapter | history result / rows, columns | bound fixed values | drained artifact values | direct scalar | `evaluation/cmd/final-v5-adapter/scale.go:525-539` |
| adapter | history result / digest | binding R-T | released Parquet normalized through R-T | direct, P52 normalizer | `evaluation/cmd/final-v5-adapter/scale.go:549-575` |
| adapter | history dependency / cardinality | binding semantic cardinality | production cardinality | direct scalar | `evaluation/cmd/final-v5-adapter/scale.go:525-539` |
| adapter | history dependency / set and members | binding S-S | production S-O | link, P51 | `evaluation/cmd/final-v5-adapter/scale.go:534-539,583-590` |
| adapter | candidate result / rows, columns | bound fixed values | drained artifact values | direct scalar | `evaluation/cmd/final-v5-adapter/scale.go:292-344,572-578` |
| adapter | candidate result / digest | binding R-T | released Parquet normalized through R-T | direct, P52 normalizer | `evaluation/cmd/final-v5-adapter/scale.go:292-344,549-570` |
| adapter | candidate dependency / cardinality | binding semantic cardinality | production cardinality | direct scalar | `evaluation/cmd/final-v5-adapter/scale.go:341-344,572-578` |
| adapter | candidate dependency / set and members | binding S-S | production S-O | link, P51 | `evaluation/cmd/final-v5-adapter/scale.go:340-344,583-590` |
| adapter | history/candidate production roots | production S-O before/after | Sample/root snapshot S-O | direct same-domain transport consistency | `evaluation/cmd/final-v5-adapter/scale.go:349-365,525-539` |
| finalizer | outcome candidate / cardinality | frozen cardinality 5 | signed/reproduced cardinality 5 | direct scalar | `evaluation/internal/experiment/outcome_candidate_v1.go:253-305` |
| finalizer | outcome candidate / set and members | frozen O-F | reproduced O-P | link, P54 fixed-oracle Catalog normalizer | `evaluation/internal/experiment/outcome_candidate_v1.go:212-235,253-335` |
| finalizer | outcome production radix identity | receipt S-R | ordinary O-F/O-P | deliberately not compared; Decision 21 forbids it | `evaluation/internal/experiment/outcome_candidate_v1.go:248-252` |
| validator | history/candidate results | retained R-T | bound/sample R-T | direct | `evaluation/internal/experiment/finalize_scale_artifact.go:325-405` |
| validator | history/candidate/root dependency cardinalities | fixed Scale integers | production counters | direct scalar | `evaluation/internal/experiment/finalize_scale_artifact.go:340-429` |
| validator | candidate/history/union dependency set and members | retained S-S expectations | retained S-O production sets | P51 link records, including missing/extra member counts | `evaluation/internal/experiment/finalize_scale_artifact.go:430-474` |
| validator | root before/after dependency sets | production snapshot S-O | Sample production S-O | direct same-domain receipt/snapshot consistency | `evaluation/internal/experiment/finalize_scale_artifact.go:365-405` |
| validator | outcome candidate set and members | retained O-F plus linked O-P | finalizer-observed O-P | P54 link; signed composite must be an observed member | `evaluation/internal/experiment/finalize_scale_artifact.go:99-163` |
| validator | ROOT identity | recomputed production tuple | Sample root digest | direct ROOT domain | `evaluation/internal/experiment/finalize_scale_artifact.go:401-402` |
| launcher gate | nested acceptance/evidence | strict Sample-v3 wire | versioned validators above | delegates semantic comparison; no new digest domain | `evaluation/internal/experiment/profile_campaign_gate.go:36-49,205-241`; `evaluation/internal/experiment/types.go:1740-1810` |
| merger | per-sample Scale evidence | retained Sample | same validators above | delegates; no new digest domain | `evaluation/internal/experiment/finalize.go:375-381,723-839` |
| merger | paired result | novel R-T | replay R-T | direct same normalized domain | `evaluation/internal/experiment/finalize.go:263-268,417-425` |
| merger | paired release/dependency/outcome sets | production S-O/S-R | paired production S-O/S-R | direct same production domain | `evaluation/internal/experiment/finalize.go:285-302,383-414` |

No missing comparison was found inside the claimed Scale contract. The former
Outcome comparison was the sole remaining Scale cross-domain direct comparison:
P53 compared O-F directly with O-P. P54 retains both identities, independently
regenerates O-P from the fixed pre-run oracle model and activated Catalog, then
requires exact O-P member equality. A changed atom or composite therefore still
rejects; the link does not skip or weaken the five-member comparison.

## Artifact and ProvSQL side audit

| Chain | Comparison | Classification / action | Evidence location |
| --- | --- | --- | --- |
| Artifact result | binding R-T vs released Parquet R-T | direct, correct; shared normalizer already used | `evaluation/cmd/final-v5-adapter/artifact.go:263-279,427-460` |
| Artifact exposure/root sets | receipt production identities vs snapshots/manifest production identities | direct same-domain transport consistency | `evaluation/cmd/final-v5-adapter/artifact.go:598-612`; `evaluation/internal/experiment/finalize.go:1492-1524` |
| Artifact dependency/outcome oracle | no independent oracle comparison | not a P54 gap: Decision 22 expressly does not claim Artifact dependency or Outcome identity | `docs/final_v5_author_decisions.md` decision 22 |
| ProvSQL external result | fixed rows through R-L vs drained visible rows through R-L | direct, correct within the frozen three-arm presentation contract | `evaluation/cmd/final-v5-adapter/provsql.go:850-866`; `evaluation/internal/experiment/finalize.go:913-933` |
| ProvSQL TaskGate dependency / cardinality | binding scalar vs production scalar | direct scalar, correct | `evaluation/internal/experiment/finalize.go:932-1003` |
| ProvSQL TaskGate dependency / set and members | binding S-S vs production S-O | **cross-domain before P54**; now linked through all three activated ProvSQL HOT/COLD publications | `evaluation/cmd/final-v5-adapter/provsql.go:514-523`; `evaluation/internal/experiment/deployment_scale_dependency_link_v1.go`; `evaluation/internal/experiment/finalize.go:986-1003` |
| ProvSQL three-arm expected result/dependency identities | binding R-L/S-S copied across direct, ProvSQL and TaskGate arms | direct same binding domain; TaskGate production S-O is separately linked and is not substituted here | `evaluation/internal/experiment/finalize.go:597-618` |
| ProvSQL release/outcome oracle | no independent oracle comparison | not a P54 gap: Decision 22 does not claim ProvSQL Outcome identity; release expectation is not used as a production acceptance claim | `docs/final_v5_author_decisions.md` decision 22 |

P54 introduces explicit new-output versions only: Scale evidence-v5 with
Outcome verification-v2/domain-link-v1, and TaskGate ProvSQL verification-v2
with dependency-link-v1. Historical Scale v1-v4 and ProvSQL v1 decoding and
validation remain unchanged.
