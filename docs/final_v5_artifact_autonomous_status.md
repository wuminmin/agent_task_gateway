# Final-V5 Artifact completion — continuation record

Working worktree `/home/wmm/worktrees/taskgate-artifact-rerun`, branch
`tkde-artifact-rerun`. The primary worktree `/home/wmm/agent-scope/task_gateway`
stays on `main @ 804d65d` and is never touched.

## Current HEAD

`55ccd3d` — equals `origin/tkde-artifact-rerun`, worktree clean.

Session start was `5e60495`. Tags `final-v5-contracts-v1` … `v1.4` verified
unmoved at `1702e65`, `5e12765`, `6f353f3`, `38e3bd3`, `36b04ba`. No v1.5 tag
exists yet, correctly: no v1.5 freeze evidence exists.

Forward commits this session, in order:

| commit | what |
| --- | --- |
| `aede5d8` | gofmt hygiene over two pre-existing unformatted files |
| `ab0ae10` | N2 `AttestationFootprintV1` + N3 plan consumption |
| `507ba30` | this continuation record |
| `4361f9d` | N4 probe: footprint qualified as a contract |
| `61f932d` | PostgreSQL runtime identity pinned to an immutable digest |
| `21e693a` | N4 audit: five corrections before any qualification run |
| `818c481` | N4 qualification harness; two live Gateway failures retained |
| `306eba3` | **Stage N4 complete**: two independent live qualifications agree |
| `18a5f58` | **B** `internal/physicalquery`: shared statements + row limits |
| `d3d2c1b` | **C** bound, compiled, operation-scoped classifier |
| `4b26984` | **D** `ObserverSnapshotV2`: authoritative and atomic |
| `c43b7ba` | **E** independent finalizer |
| `c541d7e`, `18bee8e` | N4 forward-fix: provenance, profile binding, no embedded credentials |
| `349d8b9` | two v1.5-candidate qualifications, agreeing |
| `55ccd3d` | SQL-executability gate passing live |

## Completed milestones

- **N1 audit.** The committed Stage N1 record (`5e60495`) is exploratory
  diagnosis and is NOT consumed as a qualification contract. It measured the
  right property, but: its `expected_schema_digest` is declared and empty and no
  trial carries one; the entry count is encoded in a free-text `relation_kind`
  label rather than an integer; and no PostgreSQL image is bound. The probe
  builds ExpectedSchema directly from live relations and never calls
  `catalogschema.Build`, which is why no digest was available. The evidence
  directory is retained unchanged.
- **N2/N3/N4, as superseded by the `21e693a` audit.** The V1 footprint and the
  first N4 probe were both written and then corrected before any qualification
  ran; only the corrected shape matters now.
  `AttestationFootprintV2` is qualified against one ExpectedSchema digest and
  entry count, one measurement environment and one complete
  `PostgreSQLRuntimeIdentity` (digest-pinned reference, RepoDigest, local image
  ID, running container image ID, platform), across **four** scopes —
  `constructor_or_cold_pool`, `explicit_preflight_pool`,
  `single_query_transaction`, `paired_query_transaction` — never merged, so the
  Artifact paired path cannot consume a footprint measured through
  `Connector.Query`. Constructor and explicit preflight are retained separately
  with their equality recorded, which is what a later revision would need before
  merging them.
  The measured quantity is the **multiset** of internal structural keys, carried
  end to end into `GatewayControlPlanV3.InternalExpectation` and the classifier
  manifest, summed per key as `P * preflight + S * single + Q * paired` and
  validated key by key; the class aggregate survives only as a reporting number.
  A same-total substitution of one internal key by another fails.
  `nested_viewdef_rewrite_lookup` is renamed `postgresql_internal_attestation`,
  and the classifier no longer hard-codes a `pg_rewrite` template — internal keys
  enter the manifest from the qualified footprint under a `qualified_footprint`
  source kind.
  `PortableSHA256` excludes the qualification ID and deployment-local image IDs;
  it is what the two independent runs must agree on.
- **The ExpectedSchema identity defect, measured.** The first N4 probe
  reconstructed ExpectedSchema from live `pg_attribute` (name and type only).
  `catalogschema.Digest` also covers collation, collation version and collation
  determinism, and the Result-heavy Catalog carries `en_US.utf8` / `2.36` on
  three of its sixteen fields. On this tree the Catalog-derived digest is
  `e2a3796f…` and the reconstruction digests to `d2fd017b…`. The qualification
  path now uses `catalog.Load` + `catalogschema.Build`; live relations are read
  only to verify the Catalog, never to define the identity.
- **The measurement sequence.** `AttestationsPerTrial = 2` and dividing a
  combined delta by it are gone. `pg_stat_statements` is reset once and every
  measurement is an adjacent-cumulative-snapshot delta bracketing exactly one
  Attestation. Each interval binds `stats_reset`, `dealloc`, the environment,
  `pg_postmaster_start_time()` and `total delta == sum of structural row deltas`,
  and rejects backwards counts and disappearing keys. Nothing is called "cold"
  because the view was reset; the cold/warm stability claim is withdrawn.
- **PostgreSQL runtime identity pinned** (`61f932d`). `business-postgres` and
  `control-postgres` ran on the mutable tag `postgres:16-bookworm`, while
  `compose.real-pilot.yaml` already pinned `final-v5-direct-postgres` to
  `postgres@sha256:92620dad…`. Business PostgreSQL is the server the whole
  observer accounting measures, so the floating tag made any qualified footprint
  unreproducible in principle. Verified that digest reports PostgreSQL 16.14
  (Debian 16.14-1.pgdg12+1) — `server_version_num` 160014, what
  `RequiredMeasurementEnvironment` demands — so this fixes reproducibility rather
  than moving the frozen environment, and is not an author decision. The pin sits
  in the observer-v3 overlay so ordinary production deployments are unaffected.
  All three PostgreSQL services in the composed topology now resolve to one
  immutable identity.
- **gofmt hygiene** (`aede5d8`) — two files were unformatted at the previous
  HEAD; repaired separately from any meaningful change.

Correction to the `ab0ae10` commit body: it says "the
`expected_schema_footprint` field is declared and left empty". The field is
`expected_schema_digest`. History is forward-only, so the correction is recorded
here rather than by amending.

## Active invariants

- Forward commits only; no amend, squash, rebase or force-push; old tags never
  move.
- Diagnosis runs carry `publication_eligible=false`,
  `capability_changing=false`, `activation_support_changing=false`,
  `formal_campaign=false`, and non-formal unique IDs. No Campaign ID exists.
- A footprint qualified for one ExpectedSchema is invalid for another and must
  be re-qualified; scaling it is forbidden in code, not only in prose.
- Observer equality is never weakened to `total >= expected`; unexpected
  structural statements stay fail-closed.
- Periodic healthcheck stays `/health/live`; `/health/ready` qualification
  happens outside the observer interval.
- No raw or normalized SQL in publication evidence; `queryid` is
  deployment-local diagnosis only.

## Completed: B, C, D, E, and the N4 forward-fix

**B — `internal/physicalquery`** (`18a5f58`). Shared derivation of the physical
statements and the runtime row limits, with the Gateway delegating to it. The
limits live here because `sqlpolicy` renders them into the executable SQL. Three
properties preserved exactly: the companion follows the *authorized* visible
limit (so the visible statement is authorized first, and the ordering is
load-bearing); non-expanded evidence clamps the visible limit to InfluenceFacts
while expanded evidence does not and asks for one extra row; and the ledger
pre-state is an input, so a partly-consumed ledger derives different bytes while
leaving the structural identity fixed.

**C — compiled, bound classifier** (`d3d2c1b`). `OperationIdentity` binds
operation, path, contract, ExpectedSchema and qualification. Free-form target
declarations are gone: `TargetContractIdentity` derives a target's contract from
the operation's, so another workload's target cannot be expressed. The manifest
compiles into an immutable keyed lookup, target cardinality is enforced per path,
and internal keys must carry the operation's footprint digest.
`BindingSHA256` lets the finalizer recompute the binding.

**D — `ObserverSnapshotV2`** (`4b26984`). One atomic reading; `Validate` requires
the role total to equal the sum of the structural rows, which is what makes
"same row set" checkable. Runtime identity is part of the snapshot, including the
SHA-256 of the running healthcheck command. `Accept` is equality class by class
*and* key by key — same-total internal substitution, same-total control
substitution, and missing/extra controls all fail.

**E — independent finalizer** (`c43b7ba`). Derives ExpectedSchema from the
Catalog, the plan from path + footprint, the operation identity from frozen
contract material, and the targets from statements reproduced through
`physicalquery` — then looks at the Adapter's evidence only to reject it. Path
kind is an independent input, never inferred from target count. `MeasurementArm`
stops direct-PostgreSQL and native-ProvSQL arms from carrying observer evidence.

**N4 forward-fix and requalification** (`c541d7e`, `18bee8e`, `349d8b9`). Two
fresh isolated qualifications from clean, published commit `18bee8e`:

| | value |
| --- | --- |
| portable footprint | `032e9c53704d…` — identical, and matches the development runs |
| ExpectedSchema | `e2a3796fb3f5…`, E=1 |
| profile / registry | `profile-a86cd4df5cad6e26` / `final-v5-contracts-v1.4` |
| catalog bytes | `533837084c0d…`, cross-checked against the artifact manifest |
| artifact directory | `814d4df9971f…`, identical in both runs |
| source dependencies | 8, each bound to its bytes at HEAD |

Qualification now refuses a dirty worktree, an unpublished commit, or any source
file differing from the commit. DSN passwords are gone from the harness; the
probe takes no DSN flag at all.

**SQL-executability gate** (`55ccd3d`). Now PASSES live with `-require-live`:
28 artifacts, 71 rendered cells, 0 failed, PostgreSQL 16.14. The committed
manifest regenerated byte-identically.
`evaluation/final-v5-wsl2/scripts/run-sql-executability-gate.sh` reproduces it.
The freeze-time prohibition on a skipped gate is satisfied.

## Next executable step

**Integration.** B–E are libraries with tests, not yet wired into the running
harness. `grep` over `evaluation/cmd/` shows only the footprint probe uses
`ObserverSnapshotV2`, `CompiledClassifier`, `FinalizeObservationV3` or
`AttestationFootprintV2`; `final-v5-adapter` and `final-v5-observer` still take
the pre-v3 path. Nothing downstream can pass until they are converted:

1. `final-v5-observer` emits `ObserverSnapshotV2` (it must collect the runtime
   and image identities and the healthcheck digest itself);
2. `final-v5-adapter` builds the operation identity, compiles the classifier and
   carries `CarriedEvidenceV3`;
3. the finalizer path calls `FinalizeObservationV3` and rejects v2 evidence.

Then **F** (v1.5 freeze: AMENDMENT-v1.5, the observer-accounting machine
contract, index regeneration, tag) and **G** (activation, 100x4 / 100k-x4 /
100k-x16 canaries, six cells, cross-cell leakage, capability).

## Retained true blockers

None. No author decision is outstanding.

Watch items, all ordinary technical work:

- `validate.sh` reports `contract SQL executability: SKIPPED
  (TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN is not set)`. A skip must not count as a
  pass at freeze time, so v1.5 freeze requires this gate run with the DSN set.
- The three retained failure directories under
  `evaluation/final-v5-wsl2/raw/` record the two Gateway startup findings above.
  `raw/.gitignore` is `*`, so evidence is force-added when it is worth keeping;
  these were checked for credentials before being committed.
- `result-object-store` and `result-object-store-init` still use MinIO
  `RELEASE.*` tags rather than digests. Conventionally immutable, and they touch
  no PostgreSQL statement accounting, so they are out of scope for the
  Attestation footprint — but they are not yet digest-pinned.
- Docker 29.1.3 and Go 1.25.12 available; no containers running at last check.
  Prior deployment images are cached locally, so bring-up should not rebuild
  from scratch.

## Capability and release state

Unchanged by this work, and deliberately so — nothing here is publication
evidence.

- Contract release **v1.4** (`contracts/index-v1.json`); no v1.5 material
  exists yet.
- Artifact capability **false**; overall **6/9**.
- `artifactRealSystemValidated` **false**.
- All 11 registry profiles `activation_supported=false` /
  `targeted_run_eligible=false`; `validate.sh` confirms the activation support
  manifest is absent.
- Result-heavy coverage unchanged; `targeted_validation_passed` false.
- `.NOT_READY` intact; no formal Campaign ID; paper unchanged.

## Gate results at this HEAD

`gofmt` clean · `git diff --check` clean · `go build ./...` ok ·
`go vet ./...` ok · `go test -count=1 ./...` ok ·
`validate.sh` exit 0 (with the SQL-executability skip noted above).
