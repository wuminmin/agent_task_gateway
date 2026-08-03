# TKDE Revision Status

This page is the current submission-status snapshot and supersedes earlier
revision plans and experiment wish lists. It reflects the V5 exposure profile,
encrypted Parquet result path, V8 settlement receipts, and exact Merkle radix
set now present in the repository.

- **Core architecture:** current draft candidate; not submission-frozen
- **Submission format:** rendered abstract 182 words; containerized IEEE build
  12 main-paper pages plus a 12-page supplement
- **Enterprise data-estate vision:** integrated into the manuscript as a
  brownfield-compatible serving-tier architecture; no production integration
  cost or enterprise deployment claim
- **Functional evidence:** complete for the scoped rows marked `SUPPORTED`;
  the combined-formal-model limitation below remains explicit
- **Measurement instrumentation:** the Real-system Pilot passed only for repair
  candidate commit `59a9ee4462f6f23c5469ddb1feb643bad41e0630`; its evidence
  remains `publication_eligible=false`.  A later commit does not inherit that
  PASS and must be independently revalidated and rerun before it is treated as
  current evidence
- **Experiment orchestration/evidence skeleton:** complete
- **Source-controlled formal capabilities:** `6/9` are currently true: RLS,
  adaptive attacks, ProvSQL, compiler, concurrency, and RQ5.  Baseline, Scale,
  and Artifact correctly remain false.  Baseline has only the non-publication
  `S1/tiny` Real-Pilot path; Scale and Artifact have handler code but no
  launcher-selected, live-bound complete formal matrix with reviewed datasets and oracles
- **Author contract decision:** the current instruction fixes the research
  design of the Baseline, Scale, and Artifact contracts, the three-family
  shared Benchmark Product architecture, and the independent Oracle method as
  `AUTHOR_APPROVED_FOR_IMPLEMENTATION`. Exact generated manifest/digest bytes,
  core-Claim bytes, nine private configs, three dataset bindings, `9/9`, a
  Campaign ID, submission freeze, publication execution, and paper numbers
  remain unapproved; see
  `docs/final_v5_author_review_manifest.md`
- **Stage B adapter verification:** incomplete.  Offline validate-only, unit,
  failure-retention, and schema checks do not promote a handler into a formal
  capability; any real-backend preflight is an engineering diagnostic, not
  publication evidence
- **Stage D freeze gate:** blocked; all nine capabilities, the repaired exact
  commit's Real Pilot, reviewed configs and core Claim, fixed dataset/Catalog
  digests, and a clean measured tree must all pass before a Campaign ID or
  submission commit is recorded
- **Formal publication campaign:** not executed
- **Paper numeric update:** pending
- **Final V5 evidence reseal:** deferred until manuscript/protocol finalization;
  source/raw evidence must be regenerated before the final Compose recording
- **Draft paper/evidence build:** validates retained V5 evidence against its
  recorded historical Git blobs and receipts without running experiments or
  requiring the current measured source tree to remain frozen; final-tree
  identity is deferred to the explicit strict final check
- **M3 exactness boundary:** manuscript and formal/executable evidence mapping
  completed; I3 is closed by the evaluation-only final-campaign composite
  verifier without a Receipt/schema/protocol change. The audit matrix retains
  one explicit combined-formal-model `GAP`; the verifier is not described as a
  public audit SDK or a mechanized end-to-end proof

Until the formal publication campaign is complete, archived V4 measurements
must remain labeled as V4. Current V5 evidence supports functional behavior and
scaling shape only; it must not be described as a completed million-fact V5
full-path campaign.
