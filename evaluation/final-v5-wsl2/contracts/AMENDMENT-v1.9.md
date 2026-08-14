# Final-V5 contract amendment v1.9

Previous release:
final-v5-contracts-v1.8

New release:
final-v5-contracts-v1.9

## Why this release exists

The `exposure-scale` profile enters the activation-eligible set for the first
time. It now has an independent profile Catalog and a product-scoped approval
route that reuses `final-v5-benchmark-low-v1`. The profile registry and
activation support therefore changed, and the current tree requires a named
v1.9 qualification rather than reusing or relabelling v1.8 evidence.

Author Decision 24 voided Decision 13's restriction against changing the live
Catalog surface and authorized per-profile activation. This release records
the resulting profile-Catalog and activation surface without changing runtime
code or opening a capability claim.

## What did not change

The default `config/catalog.yaml`, `maxGatewayHotArtifactsBytes = 160 << 20`,
and every file under `paper/` remain unchanged. The Gateway cgroup ceiling
remains `12g`, and `GATEWAY_CONNECTOR_STATEMENT_TIMEOUT` remains `10m`. The
accounting mechanism and every wire, Sample, ledger and Receipt byte remain
unchanged.

The Scale closure's separately measured HOT artifact is 107,642,350 B
(102.66 MiB), within the 160 MiB boundary, so this release does not raise the
constant.

## What is still NOT claimed

Capability remains **6/9**: `baseline`, `scale`, and `artifact` remain false.
This release flips no capability. Decision 15 still requires every real cell,
negative control, independent oracle manifest and formal binding to pass with
retained evidence before any capability can become true.

`targeted_validation_passed` remains **false** for `exposure-scale`. A live
activation smoke is not a targeted execution and must not be reported as one.

The campaign's cumulative peak is still not measured. The 60 Scale publication
cells and 58 baseline cells are still not implemented.

## Activation support does not carry across this release

The recorded v1.8 smokes did not carry into v1.9. The v1.8 activation-support
manifest was removed rather than relabelled, and the registry was regenerated
with all profiles initially ineligible. No old evidence byte received a v1.9
label. Eight live-route profiles were then activated against a fresh v1.9
deployment and regenerated to a byte-stable fixed point: 8 of 11 profiles are
eligible, while all 11 remain `routable=false`.

## SQL executability record

`contracts/sql-executability-v1.json` embeds the Contract Index digest, which
the release bump invalidated. The record is re-derived by executing the indexed
SQL against a disposable PostgreSQL 16.14 database, never by editing its
digest. The measured result was:

`contract SQL executability: pass (final-v5-contracts-v1.9, PostgreSQL 16.14 (Debian 16.14-1.pgdg12+1), 28 artifacts, 71 rendered cells, 0 failed)`

## Publication evidence affected

The author-decision document changed when Decision 24 was added. The v1.8
publication-binding provenance is therefore in a known mismatch state. Its
regeneration will be combined with the post-v1.9 provenance update and signed
once by the author; this release does not regenerate it or alter an approval.

## Execution status

v1 through v1.8 remain preserved for audit. v1.9 supersedes v1.8. Fresh
activation and SQL executability have completed. Attestation portable-identity
requalification remains a release-task gate before handoff for the
author-owned tag.
