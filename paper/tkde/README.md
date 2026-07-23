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
- complete RQ1/RQ2/RQ3/RQ5 deterministic results;
- the explicit `not_measured` status for RQ4 runtime overhead; and
- the model, configuration, and raw-log hashes in
  `formal/results/exposure_ledger.json`.

It then emits `generated/evidence.tex` and compiles `main.tex` with a pinned
Docker TeX environment. A local TeX syntax build is available through
`paper/tkde/build.sh`, but it still uses Docker for the exposure evaluation.

The draft intentionally says that publication-scale overhead, second-engine,
full TPC-H/TPC-DS, and agent task-success campaigns are not measured. The old
TDSC manuscript remains available through `make paper-tdsc` for provenance and
comparison; its results are not relabeled as exposure evidence.
