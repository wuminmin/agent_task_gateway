# TaskGate V4 acceptance evidence

This directory contains the immutable, source-controlled evidence selected from
the successful fresh-stack campaign `taskgate-v4-full-20260730t070232z`.
`manifest.json` binds every retained artifact by SHA-256. The paper evidence
validator independently recomputes those digests, the historical campaign
source digest, all acceptance gates, sample coverage, latency and memory
thresholds, offline build limits, activation receipts, and the small-query
regression comparison.

This is historical successful-path evidence. The 560 measured operations ran
the exact acceptance source scope at Git commit
`e8e751c666b85b436e7fa2960be23b18f3d2e515`, whose path-framed source digest is
`20ae76efb71df276774becc066e084061bd181b408e75109668e4256f29c613c`.
Later production changes that complete failure-terminal audit/receipt handling
are intentionally not attributed to this campaign. Conversely, those changes
do not retroactively invalidate the latency, resource, exact-cardinality, and
successful-settlement observations made by the archived implementation.

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
  be independently reconstructed;
- `historical-source.json`: exact commit/tree, deterministic archive, source
  selection, file-count, path-set, and path-framed source-digest provenance;
- `historical-source-e8e751c.tar.gz`: deterministic, bounded source snapshot
  containing exactly the runner-declared acceptance scope (187 files).

The validator reads the archive without extracting it, rejects unsafe paths,
links, duplicate or non-regular members, enforces compressed/decompressed and
member-count bounds, parses `sourceDigestRoots` and `sourceDigestFiles` from the
archived runner, and recomputes the exact member/path/content digests. It also
reports whether the current tree matches or has diverged from the historical
scope; divergence is disclosure metadata, not a reason to rewrite old results.

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
and high-cardinality concurrency one. Dense/clustered/random-sparse,
same-root concurrent-CAS, million-fact per-Fact independent-oracle, and current
failure-terminal behavior require their separate source-bound evidence; they
are not silently folded into this historical bundle.
