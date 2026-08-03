# Final-V5 contract amendment v1.2

Previous version: `final-v5-contracts-v1.1`
(commit `5e127651c374957324e4b2d78e90b6c78226f6a2`, tag `final-v5-contracts-v1.1`,
preserved and never moved, overwritten, or deleted. `final-v5-contracts-v1`
remains preserved as well.)

New version: `final-v5-contracts-v1.2`

## Change

The runtime Profile unit is the canonical minimal transitive Product closure
required by one or more workload cells. Experiment names are not Profile
identities. Catalog-bound, version-routed instances activate one such closure at
a time.

## Reason

A monolithic experiment-level Catalog combined mutually exclusive workload
products and exceeded the per-instance 160 MiB HOT limit, although no individual
workload closure required that combined working set.

## Unchanged

- workload cells
- Product logical data
- Query Contracts
- Oracle semantics
- Dataset logical content
- warmups
- measured samples
- deployments
- statistics
- 160 MiB production ceiling
- FactID
- Receipt
- PENDING/AVAILABLE protocol

## Publication evidence affected

None. No formal publication campaign was executed under v1.1. No Campaign ID
exists, `TASKGATE_SUBMISSION_COMMIT` is unset, Stage D remains closed with its
`.NOT_READY` markers retained, and no paper number was produced.

## Readiness is five states, not one boolean

v1.1 recorded a single `routable` flag. That conflated three different
situations and hid the real state of 28 analytics cells, so v1.2 replaces it
with five independent states, each carrying structured unresolved reasons:

| State | Question |
| --- | --- |
| `closure_complete` | does the master Catalog candidate name every Product, terminal, Publication, sidecar, dictionary, manifest, source and scope? |
| `catalog_materializable` | does the live Catalog publish the whole closure with generated, non-sentinel digests and observed HOT artifacts? |
| `live_route_available` | does the live Catalog resolve an approval route and budget profile for this exact Product closure? |
| `activation_supported` | can a Catalog-bound runtime be restarted on this profile and verified? |
| `targeted_validation_passed` | has a targeted non-publication run executed this profile's cells end to end? |

A profile is `routable` only when all five hold. Under this amendment no
profile is routable: `activation_supported` is false everywhere because the
activation orchestrator specified by `contracts/profile-activation-v1.json` is
not implemented yet, and `targeted_validation_passed` is false everywhere
because no targeted non-publication run has been executed.

## Registry corrections this amendment records

- Every preregistered cell now appears in exactly one profile. A cell is never
  dropped because its Product is not live yet; it is carried by a profile whose
  status says exactly what is missing. The v1.1 registry's separate
  `unresolved_cells` list is therefore removed.
- `exposure-scale` and `depth4-semantic-view` are first-class profile
  candidates, derived from the reviewed master Catalog candidate rather than
  from the live Catalog, so Baseline S3, Baseline S4 and Scale dependency-e2e
  can no longer vanish from the registry.
- Closure identity no longer includes routes, budgets, digests or byte counts.
  Those are deployment properties; including them let a Catalog edit change a
  profile ID. The closure digest now covers Products, Publications, Sources and
  Scopes only, and `RegistryVersion` moves to
  `taskgate-final-v5-workload-closure-profile-v2` because every digest changes.
- The profile count is 11, not 9. The two additional profiles are the
  first-class candidates above; no distinct closures were merged to preserve a
  previous count.

### The 28 analytics cells

Baseline S1/S5 (`provsql_orders`, 18 cells) and Baseline S2
(`provsql_orders` + `provsql_lineitem`, 10 cells) are **not** missing Products.
Both closures are `closure_complete` and `catalog_materializable`: the live
Catalog publishes those Products with generated digests and observed HOT
artifacts. They are blocked only on `live_route_available`, because the live
Catalog's ProvSQL approval route is scoped to the exact triple
`[provsql_lineitem, provsql_nonce, provsql_orders]` and neither subset matches
it. That is a Catalog route gap, recorded as
`no_approval_route_for_closure`, and it is deliberately not worked around here.

## New and updated machine contracts

- `contracts/profile-activation-v1.json` (new, indexed): profile unit, the five
  readiness states, the per Catalog-bound Gateway instance HOT limit, the
  activation and isolation requirements, and the matched-pair rule.
- `contracts/index-v1.json`: release identity moves to
  `final-v5-contracts-v1.2`, supersedes `final-v5-contracts-v1.1`, points at
  this record, and indexes the new activation policy.
- `profiles/profile-coverage-v1.2.json` (generated, source-controlled): the
  cell completeness audit. Every count is derived from the registry.
- `config/profiles/registry.json` and `config/profiles/*.catalog.yaml`
  (generated, source-controlled): regenerated under the v2 closure algorithm.

## Digests re-derived under this amendment

Every digest below was recomputed rather than assumed, and the generated
artifacts were produced twice and compared byte for byte.

| Artifact | Result |
| --- | --- |
| `contracts/profile-activation-v1.json` | new, indexed |
| `contracts/index-v1.json` | changed (release identity + new indexed artifact) |
| every profile closure digest | changed (v2 algorithm; structural members only) |
| every profile ID | changed (derived from the closure digest) |
| every generated profile Catalog + its SHA-256 | regenerated |
| `config/profiles/registry.json` | regenerated |
| `profiles/profile-coverage-v1.2.json` | regenerated |
| `protocol/protocol-v1.yaml`, `protocol/workloads-v1.yaml` | unchanged, still hash-locked by the index |
| `contracts/baseline-v1.json`, `contracts/scale-v1.json`, `contracts/artifact-v1.json` | unchanged |
| `contracts/benchmark-products-v1.json`, `contracts/oracle-policy-v1.json`, `contracts/result-normalization-v1.json` | unchanged |
| `catalog/benchmark-contract-v1.yaml` | unchanged |
| `sql/datasets/benchmark-v1-generate.sql`, `sql/datasets/benchmark-v1-probe.sql` | unchanged |
| all 18 query templates under `sql/contracts/` | unchanged |
| all six Artifact Oracle Manifests | unchanged; no Oracle input moved |
| `evidence/amendment-v1.1-determinism.json` | unchanged |

## Stage status after this amendment

| Item | State |
| --- | --- |
| `final-v5-contracts-v1` | preserved |
| `final-v5-contracts-v1.1` | preserved; superseded by this amendment |
| `final-v5-contracts-v1.2` | corrected contract candidate |
| Artifact capability | `false` |
| Overall capability | `6/9` |
| `.NOT_READY` | retained |
| Stage D | closed |
| Publication Campaign | not started |
