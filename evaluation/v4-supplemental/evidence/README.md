# TaskGate V4 supplemental evidence

This immutable bundle closes the three V4 axes that the base 560-operation
campaign did not measure: physical bitmap distribution, same-root contention,
and a derivation-independent maximum-point oracle. `manifest.json` binds every
local artifact, the retained base V4 `results.json`, and the exact generalized
supplemental source scope. The paper build rejects missing, extra, symlinked,
oversized, malformed, non-canonical, hash-stale, source-stale, or semantically
incomplete evidence.

The retained artifacts are:

- `distribution.json`: 3 layouts x 4 exact overlap points, 1,035,000 ordinals
  per cell and 50 runs per cell. It is a bitmap-kernel measurement, not a
  Gateway/PostgreSQL end-to-end SLO.
- `concurrency-config.json` and `concurrency.json`: the credential-free final
  fresh-root configuration and 36-gate report. Two identical Gateway replicas
  contend on one Control-PostgreSQL root at widths 1/4/8/16. Each case proves
  B-1, one charged B winner, N-1 zero-novelty settlements, and fail-closed B+1.
  The observed transitive root-lock wait chains establish exercised contention
  but are not interpreted as stale reads, CAS conflicts, or retry counts.
- `million-oracle.json`: the offline external-merge comparison of all
  1,035,013 Release/Influence/Outcome facts, canonical payloads, and witness
  multiplicities. It is derivation-independent while sharing the versioned
  canonical FactID specification and encoder.
- `environment.json`: fixed host/software/image and frozen dataset identities.

Only the final concurrency run is retained. Earlier diagnostic runs are
excluded: they found a production failure-finalization defect that withheld
ledger/result changes but left the query reservation pending. The current
Gateway sanitizes failure settlement; the retained run requires terminal
`FAILED`, exposure reservation `RELEASED`, one failure audit and receipt, and
zero partial result/chunk/materialization/observation state for every B+1.

The base V4 success-path result remains bound to its historical source archive
under `evaluation/v4-acceptance/evidence`; the final concurrency report binds
the fixed current production source. Validate this bundle and regenerate all
paper macros from the repository root with:

```sh
python3 -m unittest paper.tkde.test_v4_supplemental_evidence
python3 paper/tkde/generate_evidence.py
```

Scope remains narrow: frozen publications, the closed structured-plan algebra,
one host, a kernel-only layout sweep, a ten-row concurrency fixture, and an
offline oracle. This bundle does not claim arbitrary SQL, mutable sources,
multi-host behavior, or high-cardinality concurrent latency/throughput.
