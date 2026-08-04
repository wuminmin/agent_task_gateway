# Final-V5 Artifact completion — continuation record

Working worktree `/home/wmm/worktrees/taskgate-artifact-rerun`, branch
`tkde-artifact-rerun`. The primary worktree `/home/wmm/agent-scope/task_gateway`
stays on `main @ 804d65d` and is never touched.

## Current HEAD

`61f932d` — equals `origin/tkde-artifact-rerun`, worktree clean.

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

## Completed milestones

- **N1 audit.** The committed Stage N1 record (`5e60495`) is exploratory
  diagnosis and is NOT consumed as a qualification contract. It measured the
  right property, but: its `expected_schema_digest` is declared and empty and no
  trial carries one; the entry count is encoded in a free-text `relation_kind`
  label rather than an integer; and no PostgreSQL image is bound. The probe
  builds ExpectedSchema directly from live relations and never calls
  `catalogschema.Build`, which is why no digest was available. The evidence
  directory is retained unchanged.
- **N2.** `AttestationFootprintV1` in
  `evaluation/internal/experiment/attestation_footprint.go`. Qualified against
  exactly one ExpectedSchema digest + entry count, one measurement environment
  and one immutable PostgreSQL image ID; the two Attestation scopes are carried
  separately, never merged. `Require` binds all four and fails closed. No
  interpolation across ExpectedSchemas by design.
- **N3.** `GatewayControlPlanV3` consumes the footprint.
  `nested_viewdef_rewrite_lookup` is no longer `(P + S + Q) * E`; it is the
  measured count plus the digest of the qualification that produced it.
  `view_column_attestation` and `view_definition_attestation` keep their
  source-derived per-entry rule. `RequireFootprint` re-derives the nested count
  scope-wise so the finalizer recomputes rather than believes the Adapter.
  Constructors now return an error and refuse a footprint qualified elsewhere.
- **N4 probe** (`4361f9d`). `catalogschema.Digest` is exported and `Build` calls
  it, so the probe — which assembles ExpectedSchema from live relations rather
  than from a Catalog — produces an identity in the production digest space.
  Every trial records the ExpectedSchema digest, the entry count as an integer,
  and relation kinds as a per-entry list. The trial matrix is filled out: all
  three configurations (plain, materialized, two-entry) run the full cold/warm
  repetitions in both scopes, where N1 ran its two-entry case once, warm, with no
  relation-kind cross. The immutable PostgreSQL image ID and `DIAGNOSIS_ID` are
  both required. The probe emits `AttestationFootprintV1` per distinct
  ExpectedSchema and raises `ATTESTATION INTERNAL FOOTPRINT NOT STABLE` rather
  than averaging or unioning disagreeing trials. **Not yet run live.**
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

**Run the Stage N4 qualification live.** The probe is written, unit-tested and
committed; nothing has yet populated an `AttestationFootprintV1` from
measurement, so no plan can currently be built for a real deployment.

The topology composes and resolves cleanly — verified, 12 services exactly:

```
docker compose --project-name <non-formal-id> \
  -f compose.yaml -f compose.debug.yaml \
  -f evaluation/final-v5-wsl2/compose.real-pilot.yaml \
  -f evaluation/final-v5-wsl2/compose.provsql.yaml \
  -f evaluation/final-v5-wsl2/compose.observer-v3.yaml up -d
```

The observer-v3 override resolves last and yields the `/health/live`
healthcheck at a 3s interval, confirmed through `compose config`.

Then, with `DIAGNOSIS_ID` set to a fresh non-formal ID and
`-postgresql-image-id` read from `docker inspect` of the running
business-postgres container, run `final-v5-attestation-footprint` into a **new**
directory under `evaluation/final-v5-wsl2/raw/`. Never overwrite
`diagnosis-attestation-footprint-20260804T103538Z-4c6db5f116e4`. Carry the
`DIAGNOSIS-NOT-FOR-PUBLICATION` marker as the N1 directory does.

Note the probe's `-plain-view` and `-materialized-view` must name real relations
in the deployed `travel_demo` database; N1's invocation is not recorded anywhere,
so these have to be read off the deployed schema.

After that the critical path continues at A: formal Compose and per-sample
readiness wiring, then immutable Gateway source/build/image identity.

## Retained true blockers

None. No author decision is outstanding.

Watch items, all ordinary technical work:

- `validate.sh` reports `contract SQL executability: SKIPPED
  (TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN is not set)`. A skip must not count as a
  pass at freeze time, so v1.5 freeze requires this gate run with the DSN set.
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
