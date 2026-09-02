# Final-V5 contract amendment v1.13

Previous release:
final-v5-contracts-v1.12

New release:
final-v5-contracts-v1.13

## Why this release exists

The author-approved new-evidence program of 2026-09-02 (ledger rows
P9E-DESIGN-V1 and the strong-evidence election of menu item 3) adds the
P9.E scale point: a deterministic 750,000-row sixteen-field publication
(final-v5-scale-e7-v1) so a single admitted SUM-ladder query (fourteen
Dependency facts per surviving row: the base-row fact, six summed
argument cells, and seven always-true predicate columns) settles a
Dependency footprint above 10^7 declared facts, answering the review's
scale concern with a measured point instead of positioning prose. The
row count respects the snapshot compiler's measured in-memory envelope
(~18KB per row against a 22g ceiling).

## What changes

- sql/datasets/benchmark-v1-generate.sql: the final_v5_benchmark.scale_e7
  table (750,000 rows), its materialized reporting view, unique index,
  cardinality check, frozen-publication trigger, and comment; the closed
  column formulas are byte-identical to result_heavy with only the row
  bound changed.
- catalog/benchmark-contract-v1.yaml and the live config/catalog.yaml: the
  final-v5-scale-e7-v1 snapshot publication (all-zero fail-closed
  sentinels; only a deployment can produce the digests) and the
  final_v5_scale_e7 product (operators = and <=, aggregate sum only).
- compose.yaml: the snapshot-index-scale-e7 compiler sequenced after
  exposure-scale, and the sidecar installer's input list.
- Launcher phase-1 service lists in run-profile-campaign.sh,
  qualify-attestation-footprint.sh, and run-artifact-targeted.sh.

## What does not change

No sealed run, oracle manifest, workload cell, or budget of any earlier
release is modified; the 172-cell publication campaign of v1.11 remains
sealed and untouched. Experiments over the new product enter as
campaign_class=pilot until the author-signed dataset binding of the
P9-SIGN-HOLD batch unlocks the publication class.
