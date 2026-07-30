# TaskGate V4 acceptance evidence

This directory contains the immutable, source-controlled evidence selected from
the successful fresh-stack campaign `taskgate-v4-full-20260730t070232z`.
`manifest.json` binds every retained artifact by SHA-256. The paper evidence
generator independently recomputes those digests, the campaign source digest,
all acceptance gates, sample coverage, latency and memory thresholds, offline
build limits, activation receipts, and the small-query regression comparison.

The retained artifacts are:

- `results.json`: complete 140-root/560-operation campaign report;
- `full-config.json`: exact seven-case configuration bound by the report;
- `environment.json`: fixed host, image, database, Catalog, and dataset record;
- `activation-verification-receipt.json`: strict receipt used by measured HOT
  activation;
- `preflight-artifact-verification-receipt.json`: independent pre-provision
  HOT/COLD/sidecar verification receipt;
- `small-query-candidate.json`: current-tree V4 small-query report;
- `small-query-results.json`, `small-query-samples.jsonl`, and
  `small-query-docker-stats.jsonl`: its merged report, 832 operation samples,
  and container-memory observations, retained so the five benchmark cells can
  be independently reconstructed.

The legacy small-query baseline remains
`evaluation/exposure-performance/results.json` and is also digest-bound by the
manifest. Run `python3 paper/tkde/generate_evidence.py` from the repository root
to validate the complete paper evidence chain and regenerate LaTeX macros.

The 1.589 GB compiled publication is deliberately not duplicated in Git: the
two retained verification receipts bind every HOT/COLD/sidecar artifact by
path, size, digest, and manifest. The checked-in configuration contains only
environment-variable names and synthetic task identifiers, not token or DSN
values. The environment and receipts intentionally retain host/container and
filesystem metadata needed to audit the fixed experimental deployment.

Scope is intentionally narrow: one fixed host and fresh deployment, frozen
snapshot publications, the closed V4 online algebra, warm verified indexes,
and high-cardinality concurrency one. This bundle does not claim the separate
dense/clustered/random-sparse, same-root concurrent-CAS, or million-fact
per-Fact independent-oracle campaigns have been completed.
