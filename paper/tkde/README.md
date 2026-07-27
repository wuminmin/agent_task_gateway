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
- the three-trial, 31,296-observation RQ4 performance report and environment
  digest;
- the model, configuration, and raw-log hashes in
  `formal/results/exposure_ledger.json`.

It then emits `generated/evidence.tex` and compiles `main.tex` with a pinned
Docker TeX environment. A local TeX syntax build is available through
`paper/tkde/build.sh`, but it still uses Docker for the exposure evaluation.

The draft reports a controlled single-host result rather than claiming
TPC-scale, second-engine, multi-node, or live-LLM generality. The old TDSC
manuscript remains available through `make paper-tdsc`; its results are not
relabeled as exposure evidence. `evaluation/exposure-performance/results.json`
and `evaluation/exposure/results.json` are the source-controlled RQ4 and semantic
interchange files consumed by the paper build.
