# Final-V5 Artifact completion — continuation record

Working worktree `/home/wmm/worktrees/taskgate-artifact-rerun`, branch
`tkde-artifact-rerun`. The primary worktree `/home/wmm/agent-scope/task_gateway`
stays on `main @ 804d65d` and is never touched.

## Current HEAD

`ab0ae10` — equals `origin/tkde-artifact-rerun`, worktree clean.

Session start was `5e60495`. Tags `final-v5-contracts-v1` … `v1.4` verified
unmoved at `1702e65`, `5e12765`, `6f353f3`, `38e3bd3`, `36b04ba`.

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

**Stage N4 — the requalification run that produces a real
`AttestationFootprintV1`.** The type exists and is consumed; nothing yet
populates it from measurement. The probe must:

1. derive ExpectedSchema through `catalogschema.Build` and record
   `Result.Digest` and `Result.Count` per trial;
2. record the entry count as an integer, with `relation_kind` as an independent
   dimension (the N1 record has no cold E=2 trial and no relation-kind cross at
   E=2, so the per-entry claim currently rests on a single E=2 point);
3. record the immutable PostgreSQL image ID;
4. measure both scopes separately, as N1 correctly did;
5. emit an `AttestationFootprintV1` into a fresh diagnosis directory — never
   overwrite `diagnosis-attestation-footprint-20260804T103538Z-4c6db5f116e4`.

Then the recommended critical path continues at A (formal Compose and
per-sample readiness wiring, immutable Gateway and PostgreSQL identities).

## Retained true blockers

None. No author decision is outstanding.

Watch items, all ordinary technical work:

- `validate.sh` reports `contract SQL executability: SKIPPED
  (TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN is not set)`. A skip must not count as a
  pass at freeze time, so v1.5 freeze requires this gate run with the DSN set.
- Docker 29.1.3 and Go 1.25.12 are available; no containers currently running.

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
