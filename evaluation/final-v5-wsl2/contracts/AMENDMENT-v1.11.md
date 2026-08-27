# Final-V5 contract amendment v1.11

Previous release:
final-v5-contracts-v1.10

New release:
final-v5-contracts-v1.11

## Why this release exists

The publication campaign is scoped to the claims the paper actually makes. Three
frozen cells measured a load shape or scale that no reported hypothesis, acceptance
rule, or statistic depends on, so they are removed to keep the campaign within its
claim boundary and its wall-clock budget:

- `concurrency/shared-root/500` (forced_queue_safety, natural_contention)
- `concurrency/shared-root/100` (forced_queue_safety, natural_contention)
- `scale-extreme/taskgate_scale_extreme` at `10m` and `100m` (kernel_storage_only)

The two dropped concurrency widths exercise the bounded service queue above the
retained `ServiceActiveWindow` of ten, not additional database contention; H6 rests
on shared-root settlement with one contender per cell, and `500`/`100` appear zero
times in `hypotheses-v1.yaml`, `acceptance-rules-v1.yaml`, and `statistics-v1.yaml`.
The `scale-extreme` million-row cells measured a touched-branch I/O shape that the
paper's Limitations explicitly disclaims as million-scale; no figure, table, or
`generate_evidence.py` macro consumes them, and the finalizer does not require the
`scale-extreme` experiment.

The frozen matrix therefore drops from 178 to 172 cells (profile 129→125, non-profile
49→47). `protocol-v1.yaml` and `workloads-v1.yaml` change, so the current tree
requires a named v1.11 qualification; v1.10 activation or qualification evidence
cannot be inherited or relabelled.

## What changed

- `protocol/protocol-v1.yaml`: `scale-extreme` removed from the standard replicate
  profiles. SHA-256 `b652e81e…` → `a7cd2cd4…`.
- `protocol/workloads-v1.yaml`: `shared-root` scales `["10","50"]`; the
  `scale-extreme` block removed. SHA-256 `c5a92158…` → `b698b036…`.
- Contract index and the four contract descriptors carry the two new digests.
- Ten publication config examples carry the two new digests; `scale-extreme.example.json`
  removed. `concurrencyfixture` retires the four width-100/500 cells and lowers the
  offered-width ceiling to 50.

## What did not change

Capability, the Gateway cgroup ceiling `12g`, `GATEWAY_CONNECTOR_STATEMENT_TIMEOUT`,
the accounting mechanism, and every wire, Sample, and ledger byte outside the removed
cells remain unchanged. `acceptance-rules-v1.yaml` and `statistics-v1.yaml` are byte-identical.

## What is still NOT claimed

Capability remains 6/9: `baseline`, `scale`, and `artifact` remain false. Million-scale
latency, sustained contention, distributed behavior, and live-agent performance remain
outside the claim boundary.
