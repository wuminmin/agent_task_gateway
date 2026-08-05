# Final-V5 Artifact completion — continuation record

Working worktree `/home/wmm/worktrees/taskgate-artifact-rerun`, branch
`tkde-artifact-rerun`. The primary worktree `/home/wmm/agent-scope/task_gateway`
stays on `main @ 804d65d` and is never touched.

## Current HEAD

`5f71cbb` — equals `origin/tkde-artifact-rerun`, worktree clean.

Session start was `5e60495`. Tags `final-v5-contracts-v1` … `v1.4` verified
unmoved at `00c4636`, `167581c`, `6966cd0`, `114d190`, `af15ee1`. No v1.5 tag
exists yet, correctly: no v1.5 freeze evidence exists.

A previous continuation record named `4884119` as HEAD. That was the HEAD when
it was written; the branch had since advanced to `3d1eea9`. History is
forward-only, so the record is corrected here rather than by amending, and the
verification is against the actual branch tip rather than against a hard-coded
SHA.

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
| `11c9e30` | repository hygiene: committed build artifact removed and prevented |
| `4884119` | **I1 (part 1)** observer Business census + v2 snapshot identities |
| `3d1eea9` | continuation record for hygiene and the first half of I1 |
| `50c3cb8` | **I1-A** immutable Gateway source/build/runtime identity + formal build |
| `e3622a5` | **I1-B** observer emits `ObserverSnapshotV2` authoritatively |
| `865ae8c` | **I2-A (structures)** `internal/querybinding`, Query Receipt V9, physicalquery delegation |
| `9bc330b` | this record, corrected forward to `865ae8c` |
| `d4b2b7f` | formal Gateway base images pinned by digest |
| `2d52bea` | digest-pinned PostgreSQL environment for DB-backed tests |
| `5f71cbb` | **I2-A (persistence)** migration 019 + store plumbing for the execution binding |

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

## Repository hygiene — done

`11c9e30` removed the tracked 24 MB `final-v5-attestation-footprint` ELF from the
repository root. Cause: `go build ./evaluation/cmd/<command>` run from the root
writes the executable there, and `git add -A` swept it into `c541d7e`. That
commit stands; the file was removed forward.

Prevention, in increasing durability: `make bin` builds into ignored
`generated/bin` (with `-buildvcs=false`, needed in a linked worktree);
`.gitignore` lists `generated/bin` and the 44 current command names; and
`internal/repohygiene` rejects any *tracked* root-level file whose leading bytes
are ELF/PE/Mach-O/Go-archive, plus any tracked root-level executable that is not
a script. The test is the part that matters — a name list goes stale the moment
someone adds a command. Verified by rebuilding the artifact, force-adding it, and
confirming both checks fail.

## I1 — first half done

`4884119`. The observer can now produce everything `ObserverSnapshotV2` needs
from Business PostgreSQL:

- **One statement, one materialized row set.** A `MATERIALIZED` census CTE yields
  both the role total and the statement rows, so they cannot describe two
  instants — which is exactly what `Validate`'s total-equals-sum check tests. The
  same statement returns the environment, `stats_reset`, `dealloc`, postmaster
  start time and Business WAL position.
- **No SQL escapes.** Text is base64-encoded in SQL (so newlines and the field
  separator cannot corrupt framing), then decoded, digested and dropped. Tests
  assert a parser failure names only the queryid and leaks no fragment, and that
  a marshalled snapshot contains neither the SQL nor its base64.
- **Exact argv identities:** `--phase`, `--observer-window-id`,
  `--classifier-manifest-sha256`, optional `--operation-binding-sha256`. Not
  environment variables.
- **Window identity checks:** `Delta` requires one before and one after, and
  requires window ID, classifier manifest, operation binding and observer source
  identity to match across the pair.
- `observerRequiredSources` extended to every file whose bytes change what a
  snapshot means.

## I1-A — complete (`50c3cb8`)

The observer's `gateway_source_sha256` was filled by hashing the checkout the
observer happened to run from. That asserts something the observer cannot know:
that the running container was built from those bytes. Nothing distinguished
"the image came from this commit" from "someone ran the observer in a directory
that happens to be at this commit", and the ordinary Dockerfile's `COPY . .`
made the gap real rather than theoretical.

- **`GatewayRuntimeIdentityV1`** carries submission commit, clean-tree status at
  build, build-context and source-manifest digests, build target, OCI revision
  label, local and container image IDs, binary digest, platform, healthcheck
  digest and both base images. A canonical aggregate is carried too, but only as
  a convenience: every load-bearing member stays independently inspectable,
  because an aggregate that is the only carried value is an unexplained opaque
  hash.
- **`internal/formalbuild` + `Dockerfile.formal` + `final-v5-gateway-build`.**
  The context is materialized with `git archive` and streamed to the builder, so
  it contains exactly the blobs reachable from the named commit and untracked or
  ignored host files have no path in. The context digest binds relative path,
  file mode, file bytes and symlink target, and is computed twice — over the tar
  fed to the builder and over the commit tree from the object database — with
  agreement required. That agreement is what makes the digest checkable by a
  reviewer holding only the commit.
- **Verification reads the running container through Docker Engine** and compares
  each label against an independently computed value, never against the image's
  own claims. The image also carries its provenance as files beside the binary,
  which must agree with the labels: a label is metadata a retag can rewrite, the
  files are content in the layer the binary lives in.
- **Base images are pinned by digest** in `formal-build/base-images.json`, not by
  tag. The document is committed unpinned and the formal build refuses to run
  until `record-base-images` fills it — a build that quietly accepts whatever a
  tag points at today is the provenance gap the file exists to close.

Defect found by the tests: `git archive` prepends a `pax_global_header` carrying
the commit id. Digesting it would have made two commits with byte-identical
trees digest differently, so the context digest would no longer have meant "the
same source bytes". It is excluded.

## I1-B — complete (`e3622a5`)

`run()` parsed only `--phase` and `collect()` returned the v1 document, so every
identity `ObserverSnapshotV2` was designed to carry either came from the Adapter
or did not exist.

- The v1.5 path parses the full invocation and emits v2 **or fails**. There is no
  fallback: a path that degrades under exactly the conditions the evidence exists
  to detect is worse than no evidence.
- The observer resolves the Gateway identity through Docker Engine against a
  context materialized from the published commit, and the PostgreSQL identity
  from the running container's image. Neither is accepted from the Adapter or the
  environment — a runtime identity supplied by the party being measured is a
  claim about the deployment, not evidence about it.
- `readBusinessCensus` replaces `readBusinessCounters`; total and structural rows
  come from one `MATERIALIZED` row set.
- **Project topology is retained as its own signed member.** The Gateway and
  PostgreSQL identities say what those two services are and nothing about the
  other ten; a sidecar replaced between the before and after snapshots would
  leave both untouched while changing what the interval contained.

Defect found: the census kept only the **first** queryid for a merged structural
key while summing the calls of all of them, implying that one row identified the
aggregate. Every contributing queryid is now retained, sorted and deduplicated,
as a process-local diagnostic. `queryid` stays absent from the emitted document
entirely rather than merely unused.

`EntriesFromCommit` reads its blobs through one `git cat-file --batch` instead of
one subprocess per file; the observer recomputes this digest on every snapshot,
and a process launch per tracked file made the cost proportional to the
repository rather than to the measurement.

## I2-A — structures and delegation complete (`865ae8c`); persistence NOT done

Query Receipt V8 binds the authorization, the budget and the exposure
accounting, but nothing in it says which physical statements ran. The evaluation
filled that gap by re-deriving them afterwards and treating the result as though
the Gateway had signed it — a second opinion about the execution, not evidence
about it, and the two can differ in exactly the case the evidence exists to
detect.

**Done:**

- **`internal/querybinding`**, neutral because the Gateway, the receipt and the
  evaluation all need it and production must not depend on evaluation code.
  `ExposureLedgerBeforeV1` carries the pre-state the row limits derive from and
  no FactID, bitmap member, task payload or SQL. Its limits/used/remaining
  vectors must agree: `Remaining` is what a caller would have to forge to widen
  a row limit, so it must not be independently assertable.
  `QueryExecutionBindingV1` keeps `Authorized` and `Executed` separate, because a
  semantic replay authorizes its targets to derive the semantic key and runs
  neither; collapsing them would make that path indistinguishable from a novel
  execution. Path semantics are enforced by the structure, not by each consumer.
- **Query Receipt V9** signs both structures whole, not merely their digests —
  signing only a digest leaves every other member unprotected against a holder
  who recomputes it to match an edit. The binding must name the pre-state and the
  `budget_before` the receipt itself carries, and its row limits must reproduce
  from that signed pre-state. V8 semantics are untouched; earlier versions may
  not carry V9 material, or a holder could staple a binding onto a V8 receipt and
  present it as signed.
- **Production physicalquery delegation.** The Gateway calls
  `physicalquery.Derive` once and executes the decisions it returns, instead of
  authorizing the pair itself and delegating only the row-limit arithmetic.
  Previously the two agreed only as long as nobody changed one of them, and a
  divergence would have surfaced as a measurement result rather than as a build
  failure.

**Not done — V9 database persistence, recovery and replay.** The Control
PostgreSQL migration and store plumbing that would persist
`ExposureLedgerBeforeV1` and `QueryExecutionBindingV1` atomically with the
terminal query evidence, reload them without loss, sign the same canonical
binding on recovery, and return the original binding byte-for-byte on idempotent
replay, are **not implemented**. The V9 structures are exercised only by
hand-constructed receipts, which is explicitly not sufficient.

The blocker was environmental, not technical: at the previous session Docker was
absent from this WSL2 distro (`docker` not on PATH, Desktop WSL integration off)
and no PostgreSQL server was listening, so the DB-backed gateway tests skipped
and neither the persistence round-trip nor the live canary could be verified.
Landing an unverifiable migration was refused rather than attempted. Docker is
available again as of this record.

## Formal Gateway build — done, provenance retained

Docker returned, the base images were pinned (`d4b2b7f`) and the formal build ran
from a clean, published commit:

| | |
| --- | --- |
| source commit | `d4b2b7fb0ff37c992946a808ac0623ac9624cba3` |
| build context | `3e813b1701bbddba80b6c70d88a8e1189ab7dafa08d7c1c4ea8f7e85035a98a7` over 1274 tracked files |
| source manifest | `19e17a792be18e805e8d2347f5e6934ebb4fba75efd2998fc852279c3cf12dd5` |
| build target | `gateway` |
| image ID | `sha256:308247c28cab01365a2fbc0c2434afd6fc9a86816a7c262a651306786e4242d0` |
| binary | `22097ba0e2bb39ea319d933f7a7d0a027f731855c89eb8e1a8a74bb64d639880` |
| platform | `linux/amd64` |
| builder base | `golang@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58` |
| runtime base | `debian@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818` |

Verified independently of the build run: both base digests resolve on
linux/amd64 with the recorded repoDigest; the image labels equal the provenance
files inside the image; and `sha256sum /usr/local/bin/app` recomputed inside the
image equals the recorded binary digest.

## SQL-executability gate — PASS, live

`evaluation/final-v5-wsl2/scripts/run-sql-executability-gate.sh`: 28 artifacts,
71 rendered cells, 0 failed, against a disposable digest-pinned PostgreSQL 16.14.
That script exists because the gate needs a database initialized the way the
deployment initializes it but otherwise EMPTY — the generator creates
`final_v5_benchmark` itself, so pointing the check at the standing test
environment fails with "schema already exists". `validate.sh` still reports
SKIPPED on its own, which remains not-evidence; this run is the evidence.

## Next executable step

1. **Finish I2-A**: the Gateway must construct `QueryExecutionBindingV1` from the
   `physicalquery.Derive` decisions it already executes, write it through
   `putQueryExecutionBindingTx` in the terminal settlement transaction, and have
   `QueryReceiptEvidence` load it so the receipt is signed as V9. The persistence
   layer under it is done and live-tested; nothing writes the row yet.
2. Then I2-B (adapter on the v3 path), I3 (finalizer as sole authority), I4
   (Artifact, Scale, ProvSQL call sites), the remaining integration cases, and
   the live Result-heavy 100x4 diagnosis canary.

Integration-gate items already covered by tests: 1 (observer emits strict v2
JSON — done in I1-B), 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 20,
23, 24, 26, 27, 28, 29, 30. Not yet covered: 18, 19, 21, 22, 25.

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
- Docker availability has moved twice. It was present, then absent for a whole
  session (Docker Desktop WSL integration off, `docker` not on PATH), and is
  present again: client 29.1.3, server Docker Desktop 4.55.0, Compose v2.40.3.
  Go 1.25.12 on the host. Anything Docker-dependent should re-check
  `docker version` rather than assume.
- There is no host PostgreSQL server and none should be installed: the
  repository's digest-pinned PostgreSQL 16.14 containers are the frozen
  environment the whole accounting is qualified against, and a host install
  would be a different server. `scripts/db-test-env.sh` brings up that
  environment and exports the DSNs.
- **Five `internal/gateway` tests fail against a real control store, and did so
  before this work.** Verified identical at `3d1eea9`, the accepted pre-session
  boundary, so nothing here caused them; they were simply never run, because
  they skipped for want of `CONTROL_TEST_POSTGRES_DSN`.

  `TestDelegatedTaskSharesRootExposureAndStopsWithParent`,
  `TestSQLAndExecutePlanShareV4SemanticReplayAfterConsumedRowBudget`,
  `TestOrdinalExposureBudgetBPlusOneCommitsCompleteFailureOnly`,
  `TestCanonicalCopySurvivesAvailableTransactionFailureAndRecoversExactlyOnce`,
  `TestExecutePlanSemanticViewCarriesRegistryExpectationToPairedQueries`.

  All five fail persisting the `provsql-orders-v1` publication. The harness
  stands that publication in with `liveCompilerTestSnapshotIndex`, because its
  rows are deliberately not checked in -- production activates only the
  independently verified live HOT bundle. The double's manifest omits the
  cold-payload and hot-index digests, and `DictionaryManifest.Validate` requires
  all five, so the control store rejects it.

  The chain bottoms out and cannot be closed from inside the test: filling the
  two digests from the input's `expected_digests` gets past that check and hits
  `dictionary has no segments`; real segments need real fact counts; those need
  the rows that are deliberately absent. Skipping the store write instead moves
  the failure into production, where `PutOrdinalDictionarySet` then reports
  `V4 dictionary set 无法按 Catalog 证据发布` — the dictionary row it needs is the
  one that was skipped. Both attempts were made and reverted rather than left
  in place, and no fixture was fabricated: a manifest digest that did not
  reproduce the Catalog's would assert a publication that never existed.

  Closing this needs a compiled `provsql-orders-v1` fixture, or a harness that
  installs publications the way `snapshot-sidecar-install` does. It is
  independent of I1/I2 and is not on the critical path to the canary.

  Incidental finding while diagnosing: `DictionaryManifest.Validate`
  (`internal/ordinal/dictionary.go:68`) reports the first invalid digest while
  iterating a **map**, so the same failure names "cold payload digest" or "hot
  index digest" at random between runs.

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

Run at `50c3cb8`, `e3622a5` and `865ae8c` in turn. The DB-backed tests
(`internal/control`, `internal/dataconnector`, `internal/gateway`,
`evaluation/security`) **skipped** at all three, so the physicalquery delegation
in `865ae8c` is verified by the unit and policy tests only. A green
`go test ./...` at those commits is not evidence about database behaviour.
