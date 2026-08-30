# Final-V5 contract amendment v1.12

Previous release:
final-v5-contracts-v1.11

New release:
final-v5-contracts-v1.12

## Why this release exists

The author-approved new-evidence program of 2026-08-30 (ledger row
P8-AUTHOR-DECISION-2026-08-30, item (c2)) adds a direct measurement of the
refused-query timing channel: a ladder of aggregates of increasing exact
Dependency footprint over the deterministic 100,000-row result relation,
executed against an unlimited arm (every rung accepted) and a bounded arm
whose Dependency ceiling equals the smallest rung's a-priori derived
footprint, so every larger rung refuses on that single dimension.

## What changes

- The contract-indexed Catalog candidate `catalog/benchmark-contract-v1.yaml`
  gains two Products over the existing `final-v5-result-heavy-v1`
  publication: `final_v5_footprint_unlimited_result_heavy` and
  `final_v5_footprint_bounded_result_heavy` (six ladder columns; `sum`
  approved; `<=`/`in` operators; `category` scope). No existing Product,
  publication, workload cell, or acceptance rule changes.
- The frozen ladder itself (rung matrix, exact expected scalars, exact
  Dependency set commitments, the bounded ceiling 400, and the refusal
  pattern) is the embedded corpus `evaluation/finalv5footprint/corpus-v1.json`,
  derived entirely from the closed-form dataset model and the declared
  Dependency rule before any execution.

## What does not change

Every measurement recorded under final-v5-contracts-v1.11 or earlier remains
attributed to its own release; this amendment adds capacity and pins no new
digests for existing artifacts other than the re-pinned Catalog candidate.
