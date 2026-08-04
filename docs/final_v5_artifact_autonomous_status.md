# Final-V5 Artifact completion — continuation record

Working worktree `/home/wmm/worktrees/taskgate-artifact-rerun`, branch
`tkde-artifact-rerun`. The primary worktree `/home/wmm/agent-scope/task_gateway`
stays on `main @ 804d65d` and is never touched.

## Current HEAD

`21e693a` plus this checkpoint — equals `origin/tkde-artifact-rerun`, worktree
clean.

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

## Next executable step

**Materialize the per-profile snapshot artifact directory, then run the two
qualifications.** The probe, the contract and the harness are all in place;
what is missing is deployment wiring on critical path A.

`evaluation/final-v5-wsl2/scripts/qualify-attestation-footprint.sh` brings up a
fresh isolated project with its own volumes, proves `/health/ready` explicitly
outside the measurement window, verifies the running Gateway carries the
`/health/live` periodic probe, captures the complete PostgreSQL runtime identity
from the running container, and runs the probe. Two live attempts have been made
and both failed in the Gateway, for two distinct real reasons:

1. **Full catalog exceeds the activation boundary.** `compose.yaml` defaults
   `TASKGATE_PROFILE_CATALOG` to `config/catalog.yaml`; the Gateway rejected it
   with `Catalog hot artifacts exceed the 160 MiB activation boundary`. Fixed in
   the harness by exporting `TASKGATE_PROFILE_CATALOG` to the Profile Catalog
   being qualified — which is required anyway, since the full catalog is a
   different ExpectedSchema from the one the footprint qualifies.
2. **Undeclared publication in the shared artifact volume.** With the
   Result-heavy catalog the Gateway then rejected startup with `snapshot artifact
   directory contains undeclared publication "expense-detail-v1"`. The three
   `snapshot-index-*` services populate one shared `snapshot-index-artifacts`
   volume with every publication, while the Result-heavy closure declares only
   `final-v5-result-heavy-v1`. The Gateway fails closed, correctly.

The designed mechanism for (2) is already present and unused here:
`compose.yaml:355` mounts `${TASKGATE_PROFILE_ARTIFACT_DIR:-snapshot-index-artifacts}`
into the Gateway, and `evaluation/cmd/final-v5-profile-artifacts` materializes
per-profile directories from a verified full artifact directory
(`--source`, `--destination`, `--profile-id`, `--manifest-out`). So the sequence
is: bring the topology up far enough for the snapshot-index services to populate
the full volume, export it, run `final-v5-profile-artifacts` for the Result-heavy
profile, point `TASKGATE_PROFILE_ARTIFACT_DIR` at that directory, and start the
Gateway.

Then run, sequentially — `business-postgres` publishes a fixed host port, so
concurrent runs would not be isolated:

```
QUALIFICATION_ID=qualification-01 bash evaluation/final-v5-wsl2/scripts/qualify-attestation-footprint.sh
QUALIFICATION_ID=qualification-02 bash evaluation/final-v5-wsl2/scripts/qualify-attestation-footprint.sh
```

and require the two `portable_footprint_sha256` values and the exact
per-scope/per-key multisets to be identical. Retain both runs either way.

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
