# TaskGate TKDE working draft

This is a separate database-paper project derived from the implementation and
the requirements in [../../tkde.md](../../tkde.md). It is not a template
conversion of the TDSC security-gateway draft.

Build from the repository root:

```sh
make paper
```

Validate all source-controlled evidence and deterministically regenerate only
the LaTeX evidence macros with:

```sh
make paper-evidence
```

The build first runs `make eval-exposure`, which refreshes the deterministic
exposure report. `generate_evidence.py` then verifies:

- the report's `corpus.json` SHA-256 and fixed rewrite seed;
- RQ2's independent-oracle fixture digest, real PostgreSQL 16 version,
  generated/unique rewrite counts, check counts, and zero mismatches;
- complete RQ1/RQ2/RQ3 oracle and integration results;
- all raw files for the three-trial RQ4 campaign, including 7,896 full-path and
  23,400 ablation samples, reconstructed summaries, environment digest, and
  the digest of the archived, historically tested gateway/benchmark source;
- the three-scale PostgreSQL 16 Join--Group report, its 27 direct/novel/replay
  points, exact million-fact accounting, raw-artifact digest, implementation
  digest, and service memory peaks;
- the source-controlled V4 acceptance manifest, current tested-source digest,
  fixed environment and verification receipts, all 30 implemented gates and
  all 560 measured operations, including exact overlap, replay identity,
  latency, cgroup-memory, network, WAL, offline-build, artifact, activation,
  storage, and small-query-regression checks;
- the V4 small-query candidate's 832 raw operation samples and Docker-memory
  observations, from which its five benchmark cells are reconstructed before
  comparison with the digest-bound legacy baseline;
- the production Control PostgreSQL storage curve and budget-boundary trials;
- compact source snapshots for the legacy performance, storage, and scale
  campaigns, each uniquely bound to commit `38a35d7...` and re-hashed from
  exact archived bytes instead of being compared with the newer V4 tree;
- the model, configuration, and raw-log hashes in
  `formal/results/exposure_ledger.json` and
  `formal/results/exposure_bitmap_refinement.json`.

It then emits `generated/evidence.tex` and compiles both `main.tex` and the
separate `supplement.tex` with a pinned Docker TeX environment. The broad 1,024
pair PostgreSQL result-equivalence stress test is confined to the supplement;
the main paper's RQ2 evidence is closed-language FactSet/charge invariance.
Complete typing/big-step rules, output-key cases, and the detailed operator
coverage matrix are also in the supplement; the main paper retains the effect
summary and safety properties. A local TeX syntax build is available through
`paper/tkde/build.sh`, but it still uses Docker for the exposure evaluation.

The draft reports controlled single-host evidence, including a TPC-H-derived
(not official TPC-H) workload up to 225,000 joined members and 1,035,000
dependency facts. The V4 bundle covers one fresh deployment with warm verified
indexes; it does not claim the outstanding dense/clustered/random-sparse,
same-root concurrent-CAS, or million-fact per-Fact independent-oracle
campaigns, nor second-engine, multi-node, arbitrary-SQL, mutable-source,
production-SLO, or live-LLM generality. The old TDSC manuscript remains available through
`make paper-tdsc`; its results are not relabeled as exposure evidence. The
source-controlled semantic, performance, storage, and scale reports are all
consumed by the paper build.

## Submission-length check

Checked 2026-07-27: the IEEE Computer Society's public author guidance defines
12 formatted pages for a regular Transactions paper, including references and
author biographies, and the [2026 IEEE charge
list](https://magazines.ieeeauthorcenter.ieee.org/wp-content/uploads/sites/10/IEEE-Article-Processing-Charges-List.pdf)
lists TKDE at 12 pages before mandatory overlength charges of USD 220 per page.
The [Computer Society author
guidance](https://www.computer.org/publications/author-resources) warns that a
journal's submission-page limit can differ from its production overlength
threshold. Therefore the corresponding author must still confirm the selected
TKDE manuscript category, initial-submission limit, treatment of supplementary
material, and current charge acknowledgement in ScholarOne/Author Portal
immediately before submission. Do not infer acceptability from a successful
local PDF build alone. The 2026-07-30 pinned-template syntax build is 14
main-paper pages plus a separate 4-page supplement, so the draft requires
editorial shortening or an explicitly confirmed overlength submission path.
