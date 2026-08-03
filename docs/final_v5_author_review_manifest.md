# Final V5 Author Review Manifest

This manifest separates approval of the experiment research design from
author review of exact submission inputs. It is source-controlled so later
review decisions can name immutable bytes, but it contains no private
configuration, dataset binding, credentials, task identifiers, or oracle
values.

Current state:

- Reference repository commit: `59a9ee4462f6f23c5469ddb1feb643bad41e0630`
- Research-design decision: `AUTHOR_APPROVED_FOR_IMPLEMENTATION`
- Exact generated-bytes gate: `NOT_APPROVED`
- Submission freeze: `NOT_CONFIRMED`

## Current immutable audit references

The following hashes identify the bytes inspected when this manifest was
created. A listed hash is an audit reference, not evidence that an author has
approved those bytes.

| Artifact | SHA-256 | Exact author review |
|---|---|---|
| `paper/tkde/main.tex` | `a20a26ece45b6f9f09b351f8b916bf399e26237539526e62c19f7fe83e3c0be2` | `NOT_CONFIRMED` |
| `paper/tkde/supplement.tex` | `e973c665db1b933950e6e968976b7ee1ea018c0bcce86ef68a2a0bb24d764a80` | `NOT_CONFIRMED` |
| `docs/m3_clearance_accounting_claim_matrix.md` | `ae6da63cd537639708d7eb211dff317e2b37440fa0b32e726642032f31a76c48` | `NOT_CONFIRMED` |
| `evaluation/final-v5-wsl2/protocol/protocol-v1.yaml` | `b652e81ee669f0a54a0b5f954d2803e3119707ce0ea415472d6993871c284f3f` | `NOT_CONFIRMED` |
| `evaluation/final-v5-wsl2/protocol/workloads-v1.yaml` | `c5a921581dd8ab3e43d940504c5c0e537b913cc6107f78116ca91650fa1aaee7` | `NOT_CONFIRMED` |
| `config/catalog.yaml` | `190f9b1096eb4ea182fdca107395a725de655656c117d771853d5578ddf54566` | `NOT_CONFIRMED` |

## Research design approved by the current instruction

The current instruction is the author decision fixing these research designs
for implementation:

1. Complete the formal Baseline path for the existing S1--S6 publication
   profile using real immutable datasets, real public Task/OA execution, and
   reviewed result and effect evidence.
2. Complete the Scale path for the existing `dependency-e2e` and
   `outcome-merkle` profiles without shrinking or relabeling their cells.
3. Complete the Artifact path for the existing six-cell `result-heavy`
   profile without replacing a failed requested scale with a smaller one.
4. Define real shared Data Products, immutable publication bindings, approval
   routes, scopes, fields, and budgets sufficient for those formal paths.
5. Define independent Oracles whose expected Task/OA/query/result and
   dependency evidence is derived separately from the production Adapter and
   bound to the reviewed dataset and Catalog.

The S1--S6 meanings and Claim boundaries, Dependency/Outcome and Artifact
matrix meanings, three-family reuse architecture, and independent Oracle
method are therefore no longer awaiting a design choice. This decision is not
approval of subsequently generated manifest/digest bytes, a live Catalog or
dataset binding, private config, executable, or evidence bytes. Synthetic
placeholders and copied test fixtures cannot satisfy the approved design.

## Explicit non-approvals

The current instruction does **not** approve or confirm any of the following:

- the bytes or wording of the core paper Claim, main paper, supplement, or M3
  Claim matrix;
- the nine final private experiment configurations;
- the three final deployment dataset-binding JSON files;
- exact generated dataset, Catalog, Task/OA, query, result, or Oracle manifest
  values and digests as submission-freeze bytes;
- `9/9` source-controlled formal capabilities;
- a Campaign ID or `TASKGATE_CAMPAIGN_ID` value;
- a submission commit or `TASKGATE_SUBMISSION_COMMIT` value;
- submission freeze, publication execution, evidence sealing, or paper-number
  updates.

No implementation result, test pass, Real-system Pilot, preflight result, or
documentation change may be used to infer one of these approvals.

## Reserved exact-review record

The four research-design rows record the current authorization. Every other
field below requires a later, explicit author statement tied to exact bytes or
exact recorded evidence and remains `NOT_CONFIRMED`.

| Review or freeze field | Current value | Required future evidence |
|---|---|---|
| Core Claim bytes reviewed | `NOT_CONFIRMED` | Explicit author statement naming the exact main, supplement, and M3 hashes |
| Baseline research design | `AUTHOR_APPROVED_FOR_IMPLEMENTATION` | Current author instruction; exact generated manifests still require freeze review |
| Scale research design | `AUTHOR_APPROVED_FOR_IMPLEMENTATION` | Current author instruction; `x1` runtime conflict remains open |
| Artifact research design | `AUTHOR_APPROVED_FOR_IMPLEMENTATION` | Current author instruction; exact live bindings remain unapproved |
| Shared Product architecture and Oracle method | `AUTHOR_APPROVED_FOR_IMPLEMENTATION` | Current author instruction; exact generated Catalog/dataset/oracle bytes still require freeze review |
| Nine private configs reviewed | `NOT_CONFIRMED` | SHA-256 manifest over the exact nine mode-`0600` files |
| Three dataset bindings reviewed | `NOT_CONFIRMED` | SHA-256 and byte-identity proof for the exact three mode-`0600` files |
| Dataset and Catalog digests fixed | `NOT_CONFIRMED` | Reviewed digest record matching live preflight observations |
| All formal capabilities verified `9/9` | `NOT_CONFIRMED` | Frozen source-built Adapter output plus exact-profile tests |
| Real-system Pilot on the final commit | `NOT_CONFIRMED` | Non-publication Pilot evidence bound to the prospective submission commit |
| Final clean-tree publication preflight | `NOT_CONFIRMED` | Passing preflight bound to the prospective submission commit and measured tree |
| Campaign ID | `NOT_CONFIRMED` | Unique ID generated only after the final audit authorizes it |
| Submission commit | `NOT_CONFIRMED` | Final clean full commit selected only after all prerequisites pass |
| Submission freeze authorized | `NOT_CONFIRMED` | Explicit author authorization naming the Campaign ID and submission commit |
| Publication campaign authorized | `NOT_CONFIRMED` | Freeze authorization plus all technical campaign launch gates |
| Paper numeric update authorized | `NOT_CONFIRMED` | Three passing sealed deployments, reviewed evidence pack, and explicit author approval |

## Update rule

A future update must preserve the distinction between research-design
approval and exact-byte review. It must quote or reference the explicit author
decision, record the reviewed SHA-256 values, and change only the fields that
decision actually covers. Private config and dataset-binding contents remain
outside Git even after review.
