# Final V5 pre-contract audit — author checklist

Internal authoring record only. This document records the contract that is
visible at repair candidate
`59a9ee4462f6f23c5469ddb1feb643bad41e0630`, before the author-approved
contracts in this change were written. It is intentionally preserved as the
requested gap inventory; its “missing” cells and unchecked design items are a
pre-contract snapshot, not the current contract-layer status. Current design
resolutions are in `final_v5_experiment_contract.md`, machine cells are under
`evaluation/final-v5-wsl2/contracts/`, and remaining implementation conflicts
are in `final_v5_contract_conflicts.md`.

The post-contract state is:

| Layer | Current state |
| --- | --- |
| Research definitions | `AUTHOR_APPROVED_FOR_IMPLEMENTATION`; 58/60/6 explicit cells |
| Shared Product definitions | Source-controlled generator, probe, complete Catalog candidate, and typed streaming dataset oracle implemented |
| Independent Oracle foundation | Result/Parquet, Dependency, Outcome/schedule, Artifact, manifest, and import-boundary tests implemented |
| Generated live bindings/manifests | `NOT_GENERATED` / `NOT_APPROVED` |
| Adapters and capabilities | Baseline/Scale/Artifact still false; overall `6/9` |
| Stage D | `.NOT_READY`; no Campaign ID or Submission Commit |

This document is not an author-review sign-off, a publication authorization,
or permission to generate a Campaign ID or set `TASKGATE_SUBMISSION_COMMIT`.

## Current verdict

- The checked-in protocol defines 58 baseline cells, 62 cells counted by the
  Scale capability gate, and six Artifact cells.
- The four publication manifest digests in all ten publication example configs
  match the current manifest bytes.
- Those digests freeze workload names/scales/modes, replicate counts,
  acceptance text, and statistical text. They do not, by themselves, freeze a
  runnable Catalog/Dataset/Query/Oracle contract for the three profiles audited
  here.
- The live source-built capability report remains **6/9**. `baseline`, `scale`,
  and `artifact` are `false`; `rls`, `attack`, `provsql`, `compiler`,
  `concurrency`, and `rq5` are `true`.
- The repository therefore has no Final V5 freeze authorization. Publication
  execution and Campaign-ID generation remain closed.

## Hash-lock inventory

| Bound bytes | Current SHA-256 | What the binding currently proves |
| --- | --- | --- |
| `protocol/protocol-v1.yaml` | `b652e81ee669f0a54a0b5f954d2803e3119707ce0ea415472d6993871c284f3f` | Claim boundary, deployment count, profile-specific replicate matrix, general protocol rules, and the current status string |
| `protocol/workloads-v1.yaml` | `c5a921581dd8ab3e43d940504c5c0e537b913cc6107f78116ca91650fa1aaee7` | Ordered workload/scale/mode matrices |
| `protocol/acceptance-rules-v1.yaml` | `16b5e3d5712d2498325b8bedc79d827c6547e1517bda89156cc2e7f1191cfcd1` | Publication acceptance text, invalid rules, and Pilot exclusion |
| `protocol/statistics-v1.yaml` | `4c296e35d693c78d40048ea2fbb8500754b35d17a3ea85a11cb25cc60ba64a59` | Type-7 p50/p95/raw-n reporting, paired estimands, deterministic bootstrap, and failure retention |

`ValidateProtocol` recomputes all four hashes, binds an experiment to its one
profile, checks the profile-specific process/warmup/sample counts, and requires
the config workload value to equal the decoded workload manifest exactly.

This is not yet a submission freeze:

- all ten publication examples still contain the zero submission commit and a
  placeholder Campaign ID;
- no reviewed private config directory or byte-identical three-file dataset
  binding set is present in the repository;
- the current Catalog digest and dataset digest have not been recorded in a
  reviewed campaign binding;
- `hypotheses-v1.yaml` is not one of the four config/campaign hashes. Its
  current SHA-256 is
  `32a5ce6f9ffda05357e60b79edfdfa9663d6cd5e769673356c4c05a3ff4e9be8`;
  it would only be indirectly fixed by a real final Git commit at present;
- author review of the core Claim and final configs cannot be inferred from
  checked-in example files.

### Contract-layer summary

| Profile component | Four-manifest hash lock | Catalog | Dataset | Query | Independent oracle | Capability |
| --- | --- | --- | --- | --- | --- | --- |
| Baseline S1–S6 | Cell names/scales/modes and replicate rules only | Missing for the 58-cell contract | Missing | Unbound drafts only | Missing scale/result/FactSet/plan mapping | `false` |
| Scale dependency-e2e | 12 labels × two modes and replicate rules only | Complete pair is not routable | Reviewed binding absent | Defined only by a future private binding | Cardinality parser exists; query/result/FactSet oracle binding is absent | `false` as part of Scale |
| Scale Outcome-Merkle | 36 labels and replicate rules; parser bytes await final Git SHA | Not applicable to this declared Control microbenchmark | Not applicable | Not applicable | Source-controlled deterministic oracle exists, but `x1` semantics conflict with the author schedule | `false` as part of Scale |
| Scale extreme | Two labels and replicate rules; kernel bytes await final Git SHA | Not applicable to the kernel-only boundary | Not applicable | Not applicable | Source-controlled kernel oracle exists; Campaign inclusion is unresolved | `false` as part of Scale |
| Artifact result-heavy | Six NxC names and replicate rules only | Complete matrix is not routable | Reviewed binding absent | Defined only by a future private binding; not tied to S6 drafts | Expected result/Dependency oracle binding absent | `false` |

“Not applicable” above is a boundary decision already stated by the current
microbenchmark profile; it is not permission to reuse those samples as SQL E2E
evidence. Every other “missing” entry is a freeze blocker.

## Baseline S1–S6: 58 cells

The current machine-readable profile expands each listed scale by each listed
mode.

| Workload | Frozen scales | Frozen modes | Cells | Existing SQL draft | Executable-contract status |
| --- | --- | --- | ---: | --- | --- |
| S1 | `SF1`, `SF10` | `direct`, `novel`, `semantic_replay`, `idempotent_replay`, `normalized_rewrite_replay` | 10 | orders filter by `$1`, returning `orderkey,status` | Names only; no SF-to-parameter/dataset/task/result/FactSet/plan oracle |
| S2 | `SF1`, `SF10` | same five modes as S1 | 10 | orders–lineitem join filtered by `$1` | Names only; draft conflicts with the author Join-Group contract |
| S3 | `1k-5k`, `10k-50k`, `45k-225k` | same five modes as S1 | 15 | status-grouped aggregate over orders–lineitem | Names only; draft does not encode the author controlled-ratio contract |
| S4 | `depth-4` | `direct`, `novel`, `semantic_replay` | 3 | order/status grouped join aggregate | Names only; no depth-4 view identity, plan, fixture, or expected-result binding |
| S5 | `SF1`, `SF10` | `direct`, `novel`, `semantic_replay`, `idempotent_replay` | 8 | `LIMIT/OFFSET` pagination | Names only; draft conflicts with the author `UNION DISTINCT` contract |
| S6 | `100x4`, `10k-x4`, `100k-x4`, `100x16`, `10k-x16`, `100k-x16` | `direct`, `novel` | 12 | four fields or four fields repeated under aliases to appear as 16 | Names/NxC labels only; draft conflicts with the author requirement for 16 real fields |
| **Total** |  |  | **58** |  |  |

The seven files under `evaluation/final-v5-wsl2/sql/direct/` are Git-tracked
drafts, but no runner or adapter loads those paths and none of their individual
digests appears in the four manifest binding. A future non-placeholder Git
commit would lock their bytes indirectly; it would not make the missing
scale-parameter, task, Catalog, dataset, expected-result, or FactSet mapping
appear.

The source-built baseline adapter currently accepts only the separate,
non-publication `S1/tiny` Pilot query over `expense_detail`. It rejects every
formal S1–S6 cell as `unsupported_source_controlled_baseline_cell`, and
`baselineImplementedPublicationCells` is empty. Pilot success is not formal
S1 coverage.

### Baseline author checklist

- [ ] Replace the conflicting S2/S3/S5/S6 drafts through an explicit versioned
  contract decision; do not silently reinterpret the existing labels.
- [ ] Specify every scale-to-fixture and scale-to-parameter mapping, including
  all `$1`, `$2`, and `$3` values.
- [ ] Specify the Direct and TaskGate logical/physical SQL relationship and the
  normalized-rewrite input for each workload that claims it.
- [ ] Fix the exact Catalog products, fields, scopes, approval route, and budget
  for every cell.
- [ ] Fix dataset bytes/digest, expected rows/columns, canonical result digest,
  dependency/outcome FactSets, and required plan digest where the Claim uses
  plan identity.
- [ ] Implement all 58 real paths and independent finalizer checks before
  changing `baseline` capability to `true`.

## Scale: 62 cells counted by the capability gate

### Dependency E2E: 24

The workload is the Cartesian product of:

- candidate facts: `10,000`, `100,000`, `1,035,000`;
- target overlap: `0%`, `50%`, `90%`, `100%`;
- modes: `novel`, `semantic_replay`.

The current parser fixes exact overlap cardinalities:

| Candidate facts | 0% | 50% | 90% | 100% |
| ---: | ---: | ---: | ---: | ---: |
| 10,000 | 0 | 5,000 | 9,000 | 10,000 |
| 100,000 | 0 | 50,000 | 90,000 | 100,000 |
| 1,035,000 | 0 | 517,500 | 931,500 | 1,035,000 |

The private binding type can retain an approved task, candidate/history SQL,
expected rows/columns/result SHA-256, dependency cardinality, and dependency
set SHA-256 for exactly 12 scale labels. Those values are not in the four
manifests and no reviewed binding currently exists.

The checked-in Catalog cannot route the complete matrix. Its only route with a
large enough influence ceiling grants one query, while every cell needs at
least `novel` plus `semantic_replay` on one task; a nonzero-overlap cell also
needs history prefill. This is a real Catalog-contract gap, not permission to
reduce the requested scale.

### Outcome-Merkle: 36

The workload is the Cartesian product of:

- root cardinality: `10,000`, `100,000`, `1,000,000`;
- candidate cardinality: `1`, `100`, `10,000`;
- target overlap label: `0%`, `50%`, `90%`, `100%`;
- mode: `merkle_control`.

The implementation fixes deterministic member/oracle domains and the
`warm_immutable_content_after_fixture_prefill` cache policy. The current parser
uses `nearest_integer_half_up` for overlap cardinality. Consequently, for an
`x1` candidate both `o50` and `o90` currently realize one overlapping member in
every sample. That behavior conflicts with the author 30-measured-sample
schedule and must be resolved explicitly before a new protocol hash is
approved; see `final_v5_contract_conflicts.md`.

### Extreme kernel/storage: 2

`10m` and `100m`, each in `kernel_storage_only` mode, form two more cells.
They are a separate `scale-extreme` profile with `kernel_only=true`.

The arithmetic `24 + 36 + 2 = 62` is used by the Scale capability test. The
default campaign, however, runs only the 60 non-extreme cells; extreme is an
environment-variable opt-in and is absent from the nine required campaign
experiments. This is an unresolved completeness conflict, not a frozen 62-cell
campaign contract.

`scaleImplementedPublicationCells` is currently empty and Scale capability
must remain `false`.

### Scale author checklist

- [ ] Decide whether the publication Claim requires 60 default Scale cells or
  all 62 cells, then align workload profiles, capability coverage, launcher,
  README, and campaign finalizer.
- [ ] Replace `x1` per-sample rounding with the author-approved 30-sample
  schedule, or explicitly retain and relabel the current realized-overlap
  semantics. Either decision changes the contract and requires review.
- [ ] Fix the measured-iteration schedule, warmup treatment, seed/domain, and
  recorded realized overlap for every `x1` cell.
- [ ] Supply exact, reviewed dependency task/query/result/FactSet oracles and a
  Catalog route that can execute the complete pair without raising product
  limits or silently shrinking scale.
- [ ] Bind the dataset and Catalog digests and independently validate every
  scale sample before changing Scale capability to `true`.

## Artifact: 6 cells

The frozen name matrix is:

| Rows | Columns | Mode | Cells |
| ---: | ---: | --- | ---: |
| 100, 10,000, 100,000 | 4, 16 | `novel` | 6 |

The source parser locks the NxC label. The private binding contract can lock an
approved task, one read-only query, expected rows/columns/result SHA-256, and
dependency cardinality/set SHA-256 for each of the six labels. It does not
currently force that private query to equal either checked-in S6 SQL draft, and
no reviewed binding exists.

The current Catalog's largest legal row grant is 1,000 and its largest legal
approved field set has 15 fields. It therefore cannot authorize the 10k/100k
cells or any x16 cell. A possibly routable `100x4` corner cannot enable a
partial publication capability. `artifactImplementedPublicationCells` is
empty and Artifact capability must remain `false`.

### Artifact author checklist

- [ ] Define four real fields and sixteen real, distinct fields; repeated
  aliases are not an x16 workload.
- [ ] Fix the exact query, task, Catalog products/fields/scopes/budget, dataset,
  row/column oracle, canonical result digest, and dependency FactSet for all six
  cells.
- [ ] Decide and encode the intended relationship between baseline S6 and
  `result-heavy`; matching labels alone are not a machine contract.
- [ ] Review a Catalog that can authorize the complete matrix without silent
  scale reduction, then bind its exact digest.
- [ ] Implement and independently validate all six real paths before changing
  Artifact capability to `true`.

## Acceptance and statistics already fixed by the four manifests

For these standard profiles the current protocol requires three fresh
deployments, one process, five warmups per cell, and 30 measured samples per
cell. Acceptance requires every cell/sample, zero swap activity, no OOM or
unexpected restart, no result/FactSet/receipt/artifact/audit mismatch,
`business_sql_delta == 0` for semantic replay, and retention of requested-scale
failures and invalid samples.

Reported statistics are Hyndman–Fan Type 7 p50/p95/raw-n, deployment p50/p95,
cross-deployment median/range, within-pair ratios/differences, and a deterministic
95% bootstrap with seed `20260801`. p99, outlier deletion, and ratios of
independently aggregated arm percentiles are forbidden.

These rules are necessary but do not supply missing Catalog, Dataset, Query, or
Oracle definitions.

## Freeze decision checklist

All boxes must remain unchecked until there is retained evidence of the stated
review.

- [ ] Authors approved the replacement workload contract and its Claim impact.
- [ ] S1–S6 58, the resolved Scale matrix, and Artifact 6 have complete real
  implementations and independent oracles.
- [ ] `--capabilities` reports exactly 9/9 `true` for those real implementations.
- [ ] All private configs were regenerated after the final manifest hashes and
  reviewed as exact bytes.
- [ ] Three dataset-binding files are safe, byte-identical, and contain the
  reviewed dataset/Catalog/query/oracle identities.
- [ ] The core Claim/hypotheses and all final configs have explicit author
  review; this document is not that confirmation.
- [ ] The measured worktree is clean and the non-placeholder final commit is
  recorded only after all preceding checks pass.
