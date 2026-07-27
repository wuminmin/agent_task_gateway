# TaskGate TKDE working draft

This is a separate database-paper project derived from the implementation and
the requirements in `tkde.md`. It is not a template conversion of the TDSC
security-gateway draft.

Build from the repository root:

```sh
make paper
```

The build first runs `make eval-exposure`, which refreshes the deterministic
exposure report. `generate_evidence.py` then verifies:

- the report's `corpus.json` SHA-256 and fixed rewrite seed;
- RQ2's independent-oracle fixture digest, real PostgreSQL 16 version,
  generated/unique rewrite counts, check counts, and zero mismatches;
- complete RQ1/RQ2/RQ3 oracle and integration results;
- all raw files for the three-trial RQ4 campaign, including 7,896 full-path and
  23,400 ablation samples, reconstructed summaries, environment digest, and
  the digest of the current gateway/benchmark source;
- the three operational RQ5 policy scenarios, nine declared workflow goals,
  fact-class calibration, and exact dual-budget/utility curves;
- the three-scale PostgreSQL 16 Join--Group report, its 27 direct/novel/replay
  points, exact million-fact accounting, raw-artifact digest, implementation
  digest, and service memory peaks;
- the production Control PostgreSQL storage curve and budget-boundary trials;
- the model, configuration, and raw-log hashes in
  `formal/results/exposure_ledger.json`.

It then emits `generated/evidence.tex` and compiles both `main.tex` and the
separate `supplement.tex` with a pinned Docker TeX environment. The broad 1,024
pair PostgreSQL result-equivalence stress test is confined to the supplement;
the main paper's RQ2 evidence is closed-language FactSet/charge invariance. A local TeX syntax build is available through
`paper/tkde/build.sh`, but it still uses Docker for the exposure evaluation.

The draft reports controlled single-host evidence, including a TPC-H-derived
(not official TPC-H) workload up to 225,000 joined members and 1,035,000
dependency facts. It does not claim second-engine, multi-node, production-SLO,
or live-LLM generality. The old TDSC manuscript remains available through
`make paper-tdsc`; its results are not relabeled as exposure evidence. The
source-controlled semantic, performance, storage, and scale reports are all
consumed by the paper build.
