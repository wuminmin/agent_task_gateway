# TDSC Remediation Baseline

Stage: 0 - remediation baseline.
Created: 2026-07-22, Asia/Shanghai.

This file tracks the TDSC submission remediation plan before semantic fixes,
experiments, or paper rewrites. Stage 0 intentionally did not run formal
experiments, benchmark campaigns, long fuzzing, or fault-injection campaigns.

## Repository Baseline

- Current HEAD inspected: `3091d57a841e27b0e623a1800c4bcd5a2c76ec21`.
- Initial worktree status: clean (`git status --short` returned no entries).
- Objective source: `/home/wmm/.codex/attachments/4d2bc603-b812-48cc-9b70-c8a050a9389c/pasted-text-1.txt`.
- Stage 0 commands used for evidence gathering only: `git rev-parse HEAD`,
  `git status --short`, targeted file reads with `sed`/`rg`, and `sha256sum`.

## Existing Generated Results

These artifacts already existed in the repository before Stage 0 edits. They
are recorded as archived evidence, not as results reproduced during this stage.

| Artifact | Current recorded state | Evidence timestamp or status | SHA-256 observed in Stage 0 |
|---|---|---|---|
| `formal/results/tlc.json` | TLC passed for archived finite config | `checked_at=2026-07-21T17:38:38Z`; TLC 1.7.1 | `6184f6fb024b8009716031471acd8c4e72ff8e82c42e2ec79fa606999484ab96` |
| `formal/results/tlc.log` | Complete archived TLC log for the JSON above | referenced by `formal/results/tlc.json` | `050210d6fc226755af484d7768029deca7049a282b2e239308a715c962978d6f` |
| `evaluation/security/results.json` | partial security evidence | 15/15 SQL corpus, 2/2 prompt-boundary checks, 0.00185 fuzz CPU-hours; connector crossings and budget faults unmeasured | `3cbd55c31db682c074e1aa05eee635689cbb7e4f870bae2e5ee848b51fa96981` |
| `evaluation/generated/paper-results.json` | formal loaded, performance not measured, security partial | `generated_at=2026-07-21T17:38:38Z` | `c6f4aa813f4091942937991dd9d8308e1f16e8f07efab1c1c53cdffeb4bb638a` |
| `paper/tdsc/generated/performance_status.tex` | performance `not measured` | no complete source-backed record set | `9a0f852f5c0a0f527163d09eb4ab5ef665cf4c98cce1f6fbb68c48f2b6b9d502` |
| `paper/tdsc/generated/security_status.tex` | security partial | unauthorized crossings and budget invariant violations `not measured` | `b8a2a11174bd2001ae74d39bdb7d716a9913ae3e499885aaf38482ff64cb8a5e` |
| `paper/tdsc/generated/formal_status.tex` | archived TLC pass rendered | source-backed finite TLC run only | `3c85fe6f14877ed81a4946a6b51e00bbeb8a1eec64e3e8b155555b85dac93432` |
| `paper/tdsc/generated/artifact_status.tex` | most command outcomes `not measured` | `make formal` present/passed; `make verify`, `make eval-smoke`, `make eval-full`, and `make paper` not measured | `bf7ce2ae343bb2d53e64212a6c49d80a2d25805175754e30d7d6dcfd6704213c` |

## P0 Issue Tracker

P0 means the issue blocks formal safety claims, security/performance campaigns,
or submission-ready paper claims. Each P0 has owner files and a close condition
so later phases can close it only with current evidence.

| ID | Issue | Paper claim affected | Owner files | Current status | Close condition | Can close now? |
|---|---|---|---|---|---|---|
| P0-001 | Product-aware SQL authorization semantics. | Product-indexed `E_G(p)=(R,C,F,A,O)`, scope-preserving SQL mediation, product-specific functions/operators, ambiguous-name rejection. | `internal/sqlpolicy/types.go`, `internal/sqlpolicy/policy.go`, `internal/sqlpolicy/ast.go`, `internal/sqlpolicy/render.go`, `internal/gateway/query.go`, `internal/sqlpolicy/*_test.go`, `evaluation/attacks/corpus.json`, `paper/tdsc/main.tex` | Closed in Stage 1. The SQL engine now builds per-product relation/column/function/aggregate/operator policies, validates one lexical scope per `SELECT`/CTE/subquery, tracks derived-column source products, rejects ambiguous columns, and treats boolean/null/case syntax separately from catalog-controlled operators. The gateway passes catalog allowlists per product instead of unioning them. | Evidence: `docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/sqlpolicy ./internal/gateway ./evaluation/security` passed; `docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./...` passed. External PostgreSQL differential tests remain environment-gated by `BUSINESS_TEST_POSTGRES_DSN`. | Yes |
| P0-002 | Signed Catalog must be bound to a stable live datasource identity, with proof carried from authorization through query receipt. | Catalog/data-source binding and receipt proof that authorized Catalog matched the executed source. | `internal/catalog/types.go`, `internal/catalog/validate.go`, `internal/dataconnector/postgres.go`, `cmd/gateway/main.go`, `internal/domain/authorization.go`, `internal/queryreceipt/`, control migrations | Closed in Stage 2. Catalog sources now require `datasource_id` and PostgreSQL major version; production Gateway startup also requires the selected Source to carry `schema_digest`. Gateway builds the business DSN from Catalog Source + `secretRef`. PostgreSQL Connector attests datasource ID, `current_database()`, `current_user`, PostgreSQL major version, and schema digest over live reporting-view column names/order/generic types plus normalized `pg_get_viewdef` output. Manifest/grant cores, control grants, query records, audit payloads, and V2/V3 query receipts carry datasource/schema evidence while V1 receipt verification remains compatible. Unit/gateway tests cover missing/duplicate datasource IDs, schema-digest validation, deterministic column and view-definition digesting, database/user/version/schema mismatch rejection, multi-source route denial, datasource/schema mismatch before reservation, protocol narrowing, signed receipts, and receipt propagation. A targeted live PostgreSQL 16 regression detects same-column view-definition drift when `CONTROL_TEST_POSTGRES_DSN` is supplied; the normal container suite still skips external DB fixtures. | Evidence: full `go test ./...` passed after Stage 2 residual work; targeted live PostgreSQL 16 `TestLiveSchemaDigestDetectsViewDefinitionDrift` also passed against a disposable local DSN; `gofmt -l .` and `git diff --check` returned no output in the final gate. | Yes |
| P0-003 | Terminal query receipts are generated on read, not persisted as immutable terminal evidence; historical keyring and full verifier semantics are incomplete. Audit and receipt listing still use 500-entry truncation paths. | Persistent receipt lifecycle, immutable terminal settlement evidence, historical key verification, audit inclusion/path evidence. | `internal/control/budget.go`, `internal/control/audit.go`, `internal/control/result.go`, `internal/control/entities.go`, `internal/control/store.go`, `internal/control/recovery.go`, `internal/gateway/tools.go`, `internal/gateway/query.go`, `internal/queryreceipt/`, `internal/approval/`, `internal/control/migrations/` | Stage 3 started, not closed. Control PG now has immutable `query_receipts` rows storing signed canonical receipt JSON bytes, signature, key ID, signed timestamp, receipt hash, terminal audit sequence, and terminal audit hash. Gateway reads return the persisted bytes instead of re-signing live rows. Gateway-driven terminal settlement transactions now persist the signed V3 receipt before the same transaction commits for `COMPLETED`, `RELEASED`, `FAILED`, and `INDETERMINATE` outcomes; production startup also signs receipts in the same recovery transaction for recovered `RESERVED` queries. V1/V2 verification remains compatible. Terminal `query_records` now reject deletes, evidence-field rewrites, and updates after leaving `RESERVED`. Query receipt validation now enforces charge <= reservation, before/after budget transitions, reservation release, and terminal status/result/error combinations. Query and OA approval receipt verifiers now support keyrings with active/historical keys, valid-from windows, and retirement cutoffs; production Gateway can load `OA_RECEIPT_KEYRING_JSON` while keeping the old single-key env path. `queryreceipt` now defines a `taskgate-query-receipt-keyring/v1` public verifier bundle, and production Gateway publishes active/historical query-receipt verification keys at `/.well-known/taskgate/query-receipt-keyring.json`, with optional `GATEWAY_RECEIPT_KEYRING_JSON` validity and retirement windows. Receipt listing now pages directly over `(created_at,id)` query cursors instead of preloading 500 records. Audit receipts now include exact query events plus terminal/predecessor/successor/checkpoint evidence, and the shared `auditchain`/`queryreceipt` verifiers independently reconstruct hash material and bind the terminal event to the signed receipt. `auditchain` now defines a signed `taskgate-audit-checkpoint-anchor/v1` checkpoint anchor, and production Gateway can periodically POST the current signed checkpoint to a configured external log/WORM sink via `GATEWAY_AUDIT_ANCHOR_URL`. Archived revoked/expired tasks retain old owner-readable result ciphertext until retention purge deletes `encrypted_query_results` or result key-ID erasure disables decryption; query records, receipts, and audit evidence remain. Retention purge now excludes tasks with active legal holds, emits audit evidence when ciphertext is purged, can run from a configured Gateway TTL scheduler, and can be triggered through token-protected admin endpoints. Result rows now bind a `key_id`, registered result keys can be moved from `ACTIVE` to immutable `ERASED`, erased keys make existing ciphertext reads fail closed while preserving rows and evidence, and a token-protected admin endpoint can perform key-ID erasure. Disabled principals are denied tool listing and calls even when the old bearer principal is presented. Tests cover idempotent same-byte receipt saves, conflicting receipt-byte rejection, immutable receipt/query-record triggers, byte-identical gateway retry receipts, invalid semantic receipt vectors, atomic receipt persistence and rollback in terminal settlement transactions, recovery receipt persistence, key overlap/retirement for old grants and query receipts, public query-receipt key bundle verifier reconstruction, well-known keyring publication without private material, signed audit-checkpoint anchor verification and external POST behavior, audit inclusion verification over a 505-event path, pagination past 500 terminal query records, revoked/expired archive result readability until purge or key erasure, legal hold exclusion and release for retention purge, admin retention/key-erasure endpoint auth, ciphertext-only purge evidence retention, result key-ID erasure evidence retention, and stolen-token denial after principal disable. Remaining gaps: centralized KMS/HSM-backed key custody, actual result-key material destruction, revocation transparency, and external verifier transparency logs are not implemented; the repository posts signed checkpoint anchors but does not itself provide WORM retention, trusted timestamps, or transparency-log guarantees for the external sink. | Persist canonical receipt payload, signature, key ID, signed timestamp, datasource/schema/catalog evidence, and terminal audit hash at settlement. Enforce immutable terminal records. Add semantic receipt verifier checks, precise audit predecessor/inclusion proof or checkpoint, keyring with active and historical keys, and lifecycle tests for revoke/expiry/retention/crypto-erasure. Repeated reads must return byte-identical signed receipts. | No |
| P0-004 | Empirical evidence is incomplete and must not be upgraded into paper claims. | Security acceptance, fault matrix, full fuzz, and performance conclusions. | `evaluation/security/`, `evaluation/fuzz/`, `evaluation/cmd/runner/`, `evaluation/plots/generate.py`, `paper/tdsc/generated/*.tex`, `paper/tdsc/main.tex` | Not measured or partial. Stage 5 closed the evaluation-framework portion: it renamed the misleading `native_view_rls` baseline to `native_view`, records `TPC-derived` workload lineage, records seeded cell order/cache/task-mode/environment provenance, supports warm and cold cache strategies with checksummed cold reset hooks, validates result-equivalence hashes, admits multiple sealed full campaigns, reports per-query statistics, and renders empty performance evidence as `not_measured`. Security corpus evidence remains stale, fuzz CPU-hours are far below 24, connector crossings and budget faults are not measured, and performance has no complete source-backed records. | After P0-001 through P0-003 and the rest of P0-004 fault/security gates, run smoke gates, then full security/fault/fuzz/performance campaigns only when prerequisites and resources are available. Raw artifacts must bind current HEAD, environment, datasets, commands, and checksums. | No |
| P0-005 | Paper title/contribution wording still risks overclaiming "Human-Approved" and "evaluation plan" as contribution. | Title, abstract, contributions, threat model, TCB, related work, final claims. | `paper/tdsc/main.tex`, `paper/tdsc/references.bib`, generated result tables | Paper-implementation inconsistency. The body limits human claims, but the title still says `Human-Approved`; contribution text includes a source-backed evaluation plan before evidence is complete. | Rename to the safer Task-Scoped title or supply independent human route evidence. Rewrite contributions after implementation/evidence phases. Remove unsupported claims and tie every conclusion to raw artifacts. | No |

## Paper Claim Classification

Labels use the remediation-plan categories: `已有证据`, `仅机制存在`, `尚未测量`,
and `论文-实现不一致`.

| Claim group | Current classification | Current evidence or reason |
|---|---|---|
| Task-bound authorization protocol with narrowing-only grants and signed OA approval receipts | `已有证据` for mechanism-level tests, not newly rerun in Stage 0 | `internal/approval/`, `internal/domain/authorization.go`, gateway callback tests, and paper claim table reference these mechanisms. |
| Mandatory scoped reporting-view rewrite and rejection of basic SQL attack corpus | `已有证据` for existing tests/artifacts, not a current reproduction | `internal/sqlpolicy/policy_test.go` and `evaluation/security/results.json` show 15/15 archived corpus pass, but this does not cover product-aware Stage 1 semantics. |
| Product-indexed `E_G(p)` including product-specific relations/columns/functions/aggregates/operators and lexical scopes | `已有证据` | Stage 1 implementation and tests cover per-product allowlists, multi-product intersections, derived-column source products, CTE/subquery scopes, and gateway per-product catalog wiring. |
| Unqualified columns accepted only when unique; ambiguous names rejected; CTE shadowing isolated | `已有证据` | Stage 1 tests cover unique unqualified columns, ambiguous shared columns, alias hiding, CTE shadowing, and derived-relation alias column names. |
| Budget reservation/settlement invariant and task-level one-in-flight query | `已有证据` for current mechanism, not newly rerun | `internal/control/budget.go` has row locks and SQL checks; archived TLC has finite scalar `BudgetSafety` and `SingleInFlight`. |
| Datasource identity binds Catalog, Grant, execution, audit, and receipt | `已有证据` | Stage 2 binds Catalog Source to `datasource_id`, database, role, PostgreSQL major version, and schema digest; Grant, control records, audit payloads, and V2/V3 query receipts carry datasource/schema/catalog evidence. |
| Query receipt is terminal, persistent, immutable, and historical-key verifiable | `仅机制存在` plus `论文-实现不一致` | Signed receipt bytes are now persisted in immutable `query_receipts` rows and reused on reads; terminal query rows are immutable after settlement; V3 query receipts bind `signed_at`; gateway and configured startup-recovery terminal settlement transactions persist receipts atomically; query and approval receipt verifiers support active/historical key windows; Gateway publishes a public query-receipt verifier bundle for active/historical keys; audit inclusion verification binds terminal events to receipts. Revoked/expired archived tasks keep owner-readable old results until retention purge removes ciphertext or key-ID erasure disables decrypt; active legal holds block purge; disabled principals lose tool access. Result key-ID erasure is implemented at the Control PG/Gateway admin layer, but centralized KMS/HSM-backed custody, actual key-material destruction, revocation transparency, and external verifier transparency logs remain incomplete. |
| Audit hash chain makes terminal evidence verifiable beyond fixed truncation | `仅机制存在` plus `论文-实现不一致` | Receipt listing now pages past 500 query records, and audit receipts return exact query events plus terminal/predecessor/successor/checkpoint evidence. The verifier reconstructs hash material over paths longer than 500 events. Gateway can POST signed checkpoint anchors to an external sink when configured, but this repository does not itself provide WORM retention, trusted timestamps, or transparency-log guarantees for that sink. |
| TLA+ model proves current implementation or full-system refinement | `已有证据` for finite split-model checks; `论文-实现不一致` if claimed as a mechanized implementation proof | Current TLC evidence covers six finite models: one-task scalar lifecycle, vector budget with hard-limit archival liveness, product-aware SQL authorization, multi-task audit ordering and revoke/expiry races, terminal receipt/audit consistency, and recovery liveness. `formal/REFINEMENT.md` maps current TLA+ actions to Go methods, PostgreSQL invariants, and tests, but it remains an audit map rather than a mechanized Go/PostgreSQL refinement proof. |
| Security acceptance: connector crossings 0, budget violations 0, full fault matrix, 24 CPU-hour fuzz | `尚未测量` | Generated security status is now marked stale/not measured after the Stage 1 corpus update; focused Go security tests passed but no full security campaign is claimed. |
| Performance acceptability with Direct, Native View, RLS/AST-only/Full TaskGate and p50/p95/p99 | `尚未测量` | Generated performance status is `not measured`; no complete raw campaign exists. |
| Evaluation baseline named RLS | `已有证据` for corrected framework wording; no performance claim | Stage 5 renamed `native_view_rls` to `native_view` across runner, configs, workload manifests, generators, README, and paper method text. No PostgreSQL RLS claim is made for this baseline. |
| Standard TPC-H/TPC-DS claim | `已有证据` for corrected `TPC-derived` lineage; no standard TPC compliance claim | Stage 5 requires `workload_lineage=TPC-derived` in evaluation configs and workload manifests, validates that provenance, and paper method text says the workload is TPC-derived rather than a standards-compliant TPC-H/TPC-DS result. |
| Human-review speed, fatigue, or user-study benefit | `尚未测量` | Existing approval-count table is analytical only; there is no participant evidence. |
| IEEE build readiness | `仅机制存在` | Paper has IEEEtran fallback and generated tables; `make paper` is `not measured`, and final PDF checks were not run in Stage 0. |

## Stage 1 Evidence

Modified implementation files:

- `internal/sqlpolicy/types.go`
- `internal/sqlpolicy/policy.go`
- `internal/sqlpolicy/ast.go`
- `internal/gateway/query.go`
- `internal/sqlpolicy/policy_test.go`
- `internal/gateway/query_test.go`
- `evaluation/attacks/corpus.json`
- `evaluation/attacks/sql/alias_shadow.sql`
- `evaluation/attacks/sql/ambiguous_column.sql`
- `evaluation/attacks/sql/correlated_authorized.sql`
- `evaluation/attacks/sql/cte_shadow_column.sql`
- `evaluation/attacks/sql/structural_nodes_authorized.sql`
- `paper/tdsc/main.tex`
- `paper/tdsc/generated/security_status.tex`

Core design:

- `ProductGrant` now carries product-specific `AllowedFunctions`,
  `AllowedAggregates`, and `AllowedOperators`.
- `gateway.policyGrant` populates those allowlists from the catalog per
  approved product instead of returning a task-wide union.
- `sqlpolicy` builds a product-indexed environment and validates one lexical
  scope per `SELECT`, CTE, and subquery.
- Relation aliases bind to a concrete product or derived relation. A qualified
  column must exist in that relation. An unqualified column must resolve to
  exactly one in-scope relation.
- Derived columns carry source-product and source-column provenance; functions
  and operators use the source-product allowlist intersection when an
  expression combines products.
- Constant expressions use task-global safe lists. Structural SQL nodes such as
  boolean expressions, null tests, and case expressions are not treated as
  catalog-controlled functions/operators.

Validation commands and results:

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/sqlpolicy ./internal/gateway ./evaluation/security
```

Result: passed.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./...
```

Result: passed.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm gofmt -l internal/sqlpolicy/types.go internal/sqlpolicy/policy.go internal/sqlpolicy/ast.go internal/sqlpolicy/policy_test.go internal/gateway/query.go internal/gateway/query_test.go
```

Result: no output.

```text
git diff --check
```

Result: no output.

Remaining Stage 1 risk:

- PostgreSQL differential tests are present but environment-gated; this Stage 1
  run did not set `BUSINESS_TEST_POSTGRES_DSN`, so those database-backed cases
  were not exercised.
- The full security campaign was not rerun. `paper/tdsc/generated/security_status.tex`
  is marked stale/not measured until the security pipeline is rerun against the
  expanded corpus.

## Stage 2 Evidence

Modified implementation files:

- `internal/catalog/types.go`
- `internal/catalog/validate.go`
- `cmd/gateway/main.go`
- `internal/dataconnector/postgres.go`
- `internal/gateway/datasource.go`
- `internal/domain/authorization.go`
- `internal/queryreceipt/queryreceipt.go`
- `internal/control/budget.go`
- `internal/control/entities.go`
- `internal/control/migrations/003_datasource_attestation.sql`
- `config/catalog.yaml`
- targeted tests in `internal/catalog/`, `internal/dataconnector/`,
  `internal/domain/`, `internal/gateway/`, and `internal/queryreceipt/`
- documentation and claim wording in `docs/` and `paper/tdsc/main.tex`

Core design:

- Catalog Source identity now includes `datasource_id`, database, user,
  PostgreSQL major version, and a selected-source `schema_digest` required by
  production Gateway startup.
- Gateway builds the business DSN from Catalog Source plus `secretRef`; DSN or
  cleartext password values in Catalog remain rejected.
- The PostgreSQL Connector attests `reporting.datasource_attestation`,
  `current_database()`, `current_user`, PostgreSQL major version, and a
  reporting-schema digest.
- The reporting-schema digest is domain-separated as
  `TASKGATE-REPORTING-SCHEMA-V2` and covers reporting view identity, normalized
  `pg_get_viewdef` output, column count/order, column names, and generic
  PostgreSQL types.
- Datasource ID, schema digest, catalog version, and catalog digest now flow
  through manifest/grant cores, control grants, query records, audit payloads,
  and V2/V3 query receipts. V1 receipt verification remains compatible.

Validation commands and results:

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/dataconnector
```

Result: passed.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/gateway
```

Result: passed.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./...
```

Result: passed.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=<temporary local PostgreSQL 16 DSN> -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/dataconnector -run TestLiveSchemaDigestDetectsViewDefinitionDrift -count=1 -v
```

Result: passed against a disposable PostgreSQL 16 container, then the container
was stopped. The demo Catalog digest
`02b4a211cfbab7347fdce28e2dd76406b1118c5f18e1d2146cc2e85a38ccf1cc` was
recomputed from the initialized demo reporting views and matched
`config/catalog.yaml`.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm gofmt -l .
```

Result: no output.

```text
git diff --check
```

Result: no output.

Remaining Stage 2 risk:

- Routine `go test ./...` remains independent of external PostgreSQL fixtures,
  so the live same-column view-definition drift regression runs only when
  `CONTROL_TEST_POSTGRES_DSN` is supplied or when invoked against a disposable
  local PostgreSQL instance as above.
- The mechanism binds the normalized PostgreSQL view definition and generic
  column shape, not business semantic correctness of administrator-published
  views.

## Stage 3 Evidence

Partial Stage 3 milestones: atomic persisted terminal query receipts, terminal
query row immutability, receipt semantic validation, receipt keyring
verification, verifiable paged audit/receipt evidence, and tested result
lifecycle behavior.

Modified implementation files:

- `internal/control/types.go`
- `internal/control/receipt.go`
- `internal/control/budget.go`
- `internal/control/audit.go`
- `internal/control/entities.go`
- `internal/control/result.go`
- `internal/control/store.go`
- `internal/control/recovery.go`
- `internal/auditchain/auditchain.go`
- `internal/auditchain/anchor.go`
- `internal/control/migrations/001_initial.sql`
- `internal/control/migrations/004_query_receipts.sql`
- `internal/control/migrations/005_terminal_query_immutability.sql`
- `internal/control/migrations/006_failed_query_status.sql`
- `internal/control/migrations/007_result_retention_holds.sql`
- `internal/gateway/query.go`
- `internal/gateway/tools.go`
- `cmd/gateway/main.go`
- `cmd/gateway/main_test.go`
- `scripts/generate-ed25519-env.sh`
- `compose.yaml`
- `.env.example`
- `internal/approval/protocol.go`
- `internal/queryreceipt/keybundle.go`
- `internal/queryreceipt/queryreceipt.go`
- targeted tests in `internal/auditchain/auditchain_test.go`
- targeted tests in `internal/control/control_test.go` and
  `internal/gateway/query_test.go`
- targeted access/audit receipt test in `internal/gateway/access_test.go`
- targeted tests in `internal/approval/protocol_test.go`
- targeted tests in `internal/queryreceipt/queryreceipt_test.go`
- documentation and claim wording in `docs/` and `paper/tdsc/main.tex`

Core design:

- Control PG now has a `query_receipts` table keyed by `query_id`.
- Each row stores the receipt version, gateway key ID, signature, signed
  timestamp, terminal audit sequence/hash, canonical signed receipt JSON bytes,
  a SHA-256 hash of those bytes, and creation time.
- `query_receipts` rows are protected by no-update/no-delete triggers using the
  existing immutable-row trigger function.
- `SaveQueryReceipt` verifies the query is terminal and that the supplied
  terminal audit sequence/hash names a terminal query audit event for the same
  query. Re-saving the same bytes is idempotent; different bytes conflict.
- Gateway terminal receipt reads first return stored receipt bytes. If a
  terminal record lacks a stored receipt, the gateway signs canonical V3 receipt
  bytes with `signed_at`, persists them, and returns the persisted bytes.
- Gateway-driven terminal settlement transactions persist the signed receipt
  before commit for `COMPLETED`, `RELEASED`, `FAILED`, and `INDETERMINATE`
  query records. A receipt-builder failure rolls the query back to `RESERVED`
  and leaves encrypted result bytes uncommitted.
- Startup recovery can receive a `WithRecoveryReceiptBuilder`; production
  `cmd/gateway` supplies one from `GATEWAY_RECEIPT_KEY_ID` and
  `GATEWAY_RECEIPT_PRIVATE_KEY`, so recovered `RESERVED` queries are marked
  `INDETERMINATE` and signed in the same recovery transaction.
- Terminal `query_records` rows reject deletes, identity/evidence-field
  rewrites, and updates once the row has left `RESERVED`. The normal
  `RESERVED` to terminal settlement transition remains allowed.
- `FAILED` is now a terminal query status for post-execution failures with
  bounded observed usage but no durable result hash.
- `queryreceipt` semantic validation now rejects receipts where charge exceeds
  reservation, before/after budget vectors do not match the charged budget,
  reservation evidence is not released, or terminal status/result/error fields
  are inconsistent.
- `queryreceipt` keeps V1/V2 verification compatibility and adds V3
  `signed_at` binding plus a keyring verifier with active/historical keys,
  valid-from windows, and retirement cutoffs.
- `queryreceipt` also defines the machine-readable
  `taskgate-query-receipt-keyring/v1` public key bundle and can reconstruct a
  receipt verifier directly from that bundle. Bundle keys are sorted by key ID,
  carry active/historical public keys plus valid-from/retirement windows, and
  never include private signing material.
- `approval` receipt verification now supports the same valid-from/retirement
  windows for old task grants; `cmd/gateway` can load
  `OA_RECEIPT_KEYRING_JSON` while preserving the single-key environment
  variables for local demos.
- Production Gateway derives the active query-receipt public key from
  `GATEWAY_RECEIPT_PRIVATE_KEY`, merges optional historical/verifier metadata
  from `GATEWAY_RECEIPT_KEYRING_JSON`, rejects mismatched active-key metadata,
  and publishes the resulting verifier bundle at
  `/.well-known/taskgate/query-receipt-keyring.json`.
- Receipt listing uses `ListQueriesPage` over `(created_at,id)` cursors, so a
  task with more than 500 query records can be traversed page by page.
- `get_audit_receipt` no longer scans the first 500 task events. It fetches the
  query's exact terminal audit event, all audit events for that query, the
  predecessor event when present, the successor path from the terminal event to
  the current checkpoint, and the current audit checkpoint.
- `internal/auditchain` defines the shared audit hash material, chain hash
  function, and inclusion verifier. Control PG uses the shared hash function,
  and the gateway verifies its own inclusion proof before returning it.
- `queryreceipt.VerifyAuditInclusion` binds the proof's terminal event to the
  signed receipt's query ID, terminal sequence, previous hash, current hash, and
  status-specific terminal audit event type before validating the successor path
  to the checkpoint.
- `auditchain` defines a signed `taskgate-audit-checkpoint-anchor/v1` payload
  over the current checkpoint sequence/hash, deterministic anchor ID, signing
  time, and Gateway key ID. The verifier checks the signature, checkpoint hash,
  anchor ID, and optional validity/retirement windows.
- Production Gateway can load `GATEWAY_AUDIT_ANCHOR_KEY_ID` and
  `GATEWAY_AUDIT_ANCHOR_PRIVATE_KEY`; when `GATEWAY_AUDIT_ANCHOR_URL` is set,
  it periodically signs the current Control PG audit checkpoint and POSTs the
  anchor JSON to the configured external log/WORM sink with an idempotency key.
- `DisablePrincipal` records `disabled_at` for a principal. Gateway tool
  listing and dispatch re-read the stored principal and deny mismatched or
  disabled identities, so a stale bearer token cannot continue to call tools.
- `PurgeEncryptedResultsBefore` deletes retained result ciphertext by
  `created_at` cutoff while leaving terminal query records, receipt rows, and
  audit evidence intact; it now skips tasks with an active
  `result_retention_holds` row and appends `RETENTION_PURGE_RESULTS` audit
  evidence when ciphertext is deleted.
- `encrypted_query_results` now records a result `key_id`, and
  `result_encryption_keys` records each key ID as `ACTIVE` or immutable
  `ERASED`. `EraseResultEncryptionKey` marks the key ID erased, appends
  `RESULT_ENCRYPTION_KEY_ERASED` audit evidence, and makes all ciphertext rows
  under that key fail closed on read while retaining ciphertext rows, terminal
  query records, receipts, and audit evidence. The existing
  `NewAES256GCM` constructor remains compatible; production Gateway can set
  `GATEWAY_DATA_KEY_ID`, and the token-protected admin surface exposes
  `POST /admin/v1/result-encryption-keys/{key_id}/erase`.
- `SetResultRetentionHold`, `ClearResultRetentionHold`, and
  `GetResultRetentionHold` provide an auditable legal-hold workflow. Active
  holds block result-ciphertext purge until explicitly released.
- Production Gateway can run a result-retention TTL sweep from
  `GATEWAY_RESULT_RETENTION_TTL` and `GATEWAY_RESULT_RETENTION_SWEEP_INTERVAL`.
  When `GATEWAY_ADMIN_TOKEN` is configured, token-protected HTTP admin
  endpoints can trigger purge and set or clear task legal holds.
- Archived revoked and expired tasks do not accept new queries, but their old
  results remain readable to the owner until encrypted-result retention cleanup
  purges the ciphertext or result key-ID erasure disables decrypts.

Validation commands and results:

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/control
```

Result: passed in the normal fixture-skipping container run.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/gateway
```

Result: passed.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/queryreceipt
```

Result: passed.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/auditchain ./internal/queryreceipt ./internal/control ./internal/gateway
```

Result: passed.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./cmd/gateway ./internal/approval ./internal/queryreceipt ./internal/gateway
```

Result: passed.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=<temporary local PostgreSQL 16 DSN> -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/control -run 'TestMigrationsAndTaskRequestContext|TestPersistedQueryReceiptIsIdempotentAndImmutable|TestBudgetSerializationFinalizationAndAuditChain' -count=1 -v
```

Result: passed against a disposable PostgreSQL 16 container, then the container
was stopped.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=<temporary local PostgreSQL 16 DSN> -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/control ./internal/gateway -run 'TestListQueriesPageTraversesBeyondFiveHundredRecords|TestStructuredPlanAndDirectSQLKeepRawResultsOwnerOnly' -count=1 -v
```

Result: passed against a disposable PostgreSQL 16 container, then the container
was stopped.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/control ./internal/gateway
```

Result: passed in the normal fixture-skipping container run.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=<temporary local PostgreSQL 16 DSN> -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/control ./internal/gateway -run 'TestPurgeEncryptedResultsBeforeErasesCiphertextOnly|TestArchivedTaskResultsStayReadableUntilRetentionPurge|TestDisabledPrincipalCannotUseStolenBearerToken' -count=1 -v
```

Result: passed against a disposable PostgreSQL 16 container.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=<temporary local PostgreSQL 16 DSN> -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/control ./internal/gateway -count=1
```

Result: passed against the same disposable PostgreSQL 16 container.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=<temporary local PostgreSQL 16 DSN> -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./... -count=1
```

Result: passed against the same disposable PostgreSQL 16 container, then the
container was stopped.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./cmd/gateway ./internal/queryreceipt ./internal/control ./internal/gateway
```

Result: passed after adding atomic receipt persistence and recovery receipt
startup wiring.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=<temporary local PostgreSQL 16 DSN> -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/control ./internal/gateway -run 'TestTerminalSettlementWithReceiptPersistsAtomically|TestStartupRecoveryCanPersistIndeterminateReceipt|TestQueryEncodingFailureSettlesActualUsage|TestQueryFinalizationFailureSettlesActualUsage|TestFailedSettlementMakesServiceUnreadyUntilBackgroundRetrySucceeds|TestConnectorAmbiguityChargesReservationAndUsesStableCode|TestDefinitePreExecutionConnectorFailureReleasesAndNeverRetriesRequestID' -count=1 -v
```

Result: passed against a disposable PostgreSQL 16 container.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=<temporary local PostgreSQL 16 DSN> -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/control ./internal/gateway -count=1
```

Result: passed against the same disposable PostgreSQL 16 container.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=<temporary local PostgreSQL 16 DSN> -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./... -count=1
```

Result: passed against the same disposable PostgreSQL 16 container, then the
container was stopped.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./cmd/gateway ./internal/control
```

Result: passed in the normal fixture-skipping container run after adding
retention TTL/admin/legal-hold code.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=<temporary local PostgreSQL 16 DSN> -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/control ./cmd/gateway -run 'TestPurgeEncryptedResultsBeforeErasesCiphertextOnly|TestRetentionAdminEndpointsRequireAuthAndManageHold|TestRetentionConfigFromEnvParsesDurations' -count=1 -v
```

Result: passed against a disposable PostgreSQL 16 container.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=<temporary local PostgreSQL 16 DSN> -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./cmd/gateway ./internal/control -count=1
```

Result: passed against the same disposable PostgreSQL 16 container.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=<temporary local PostgreSQL 16 DSN> -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./... -count=1
```

Result: passed against the same disposable PostgreSQL 16 container.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/queryreceipt ./cmd/gateway -run 'TestPublicKeyBundleBuildsVerifierForDistributedKeys|TestQueryReceiptPublicKeyBundleFromEnvPublishesActiveAndHistoricalKeys|TestQueryReceiptKeyringHandlerPublishesVerifierBundle' -count=1 -v
```

Result: passed after adding public query-receipt verifier bundle publication.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/queryreceipt ./cmd/gateway
```

Result: passed after adding public query-receipt verifier bundle publication.

```text
sh -n scripts/generate-ed25519-env.sh
```

Result: no output.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/auditchain ./cmd/gateway -run 'TestSignedCheckpointAnchorBindsCheckpointAndKeyWindow|TestAuditAnchorConfigFromEnvParsesSignerAndInterval|TestPostAuditCheckpointAnchorPostsSignedPayload' -count=1 -v
```

Result: passed after adding signed audit-checkpoint anchor export.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/auditchain ./cmd/gateway
```

Result: passed after adding signed audit-checkpoint anchor export.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./...
```

Result: passed after adding signed audit-checkpoint anchor export.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm gofmt -l .
```

Result: no output.

```text
git diff --check
```

Result: no output.

```text
docker ps --filter name=taskgate- --format '{{.Names}}'
```

Result: no output after stopping the disposable PostgreSQL 16 container.

For result key-ID erasure:

```text
docker run -d --rm --name taskgate-stage3-key-erasure -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=taskgate_tests -p 127.0.0.1:25454:5432 postgres:16-bookworm
```

Result: disposable PostgreSQL 16 container started.

```text
docker exec taskgate-stage3-key-erasure pg_isready -U postgres -d taskgate_tests
```

Result: `/var/run/postgresql:5432 - accepting connections`.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=postgres://postgres:postgres@127.0.0.1:25454/taskgate_tests?sslmode=disable -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/control ./cmd/gateway -run 'TestEraseResultEncryptionKeyMakesCiphertextUnreadableButEvidenceRemains|TestMigrationsAndTaskRequestContext|TestRetentionAdminEndpointsRequireAuthAndManageHold|TestAES256GCMValidationAndAuthentication' -count=1 -v
```

Result: passed after adding result key-ID erasure and the token-protected admin
erasure endpoint.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=postgres://postgres:postgres@127.0.0.1:25454/taskgate_tests?sslmode=disable -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./internal/control -count=1
```

Result: passed after adding result key-ID erasure.

```text
docker run --rm --network host -e CONTROL_TEST_POSTGRES_DSN=postgres://postgres:postgres@127.0.0.1:25454/taskgate_tests?sslmode=disable -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./... -count=1
```

Result: passed after adding result key-ID erasure.

```text
docker run --rm -u 1000:1000 -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm gofmt -l .
```

Result: no output.

```text
sh -n scripts/generate-ed25519-env.sh
```

Result: no output.

```text
git diff --check
```

Result: no output.

```text
docker stop taskgate-stage3-key-erasure
```

Result: stopped `taskgate-stage3-key-erasure`.

```text
docker ps --filter name=taskgate- --format '{{.Names}}'
```

Result: no output after stopping the result-key-erasure disposable PostgreSQL
16 container.

Remaining Stage 3 risk:

- Audit inclusion verification now reconstructs hash material and proves the
  returned terminal-to-checkpoint path. Gateway can export signed checkpoint
  anchors to an external sink, but the repository does not itself provide WORM
  retention, trusted timestamps, or transparency-log guarantees for that sink.
- Keyring verification now covers OA grants and V3 query receipts, and Gateway
  publishes a query-receipt verifier bundle, but centralized KMS/HSM-backed key
  custody, actual result-key material destruction, revocation transparency, and
  external verifier transparency logs remain unimplemented.
- Result ciphertext purge, TTL scheduling, administrator purge/hold/key-erasure
  endpoints, legal hold exclusion, result key-ID erasure enforcement, and
  disabled-principal denial are implemented and tested. The remaining
  production gap is binding key-ID erasure to a real KMS/HSM or Secret Manager
  destroy/disable operation and publishing that revocation transparently.
- Audit receipts now expose query-local events plus predecessor/successor/
  checkpoint evidence, and signed checkpoint anchors can be exported. Independent
  retention and timestamp trust for the external anchor sink remain deployment
  responsibilities.

## Stage 4 Evidence

Stage 4 finite-model milestone: the formal suite now uses split finite models
for core task lifecycle, vector budgets, product-aware SQL authorization,
multi-task audit ordering, terminal receipt/audit consistency, and recovery
liveness. The suite remains finite-state design evidence plus an audit
refinement map, not a mechanized proof that the Go/PostgreSQL implementation
refines TLA+.

Modified files:

- `formal/TaskGate.tla`
- `formal/TaskGate.cfg`
- `formal/VectorBudget.tla`
- `formal/VectorBudget.cfg`
- `formal/SQLAuthorization.tla`
- `formal/SQLAuthorization.cfg`
- `formal/MultiTaskAudit.tla`
- `formal/MultiTaskAudit.cfg`
- `formal/ReceiptAudit.tla`
- `formal/ReceiptAudit.cfg`
- `formal/RecoveryLiveness.tla`
- `formal/RecoveryLiveness.cfg`
- `formal/run.sh`
- `formal/README.md`
- `formal/results/.gitignore`
- `formal/results/README.md`
- `formal/results/tlc.json`
- `formal/results/tlc.log`
- `formal/results/vector_budget.json`
- `formal/results/vector_budget.log`
- `formal/results/sql_authorization.json`
- `formal/results/sql_authorization.log`
- `formal/results/multi_task_audit.json`
- `formal/results/multi_task_audit.log`
- `formal/results/receipt_audit.json`
- `formal/results/receipt_audit.log`
- `formal/results/recovery_liveness.json`
- `formal/results/recovery_liveness.log`
- `formal/REFINEMENT.md`
- `evaluation/plots/generate.py`
- `evaluation/generated/paper-results.json`
- `paper/tdsc/generate_tables.py`
- `paper/tdsc/generated/formal_status.tex`
- `paper/tdsc/generated/security_status.tex`
- `paper/tdsc/generated/artifact_status.tex`
- `paper/tdsc/main.tex`
- `paper/tdsc/remediation.md`

Core design:

- `formal/REFINEMENT.md` maps the current finite TLA+ abstractions to the
  implementation at the required granularity:
  `TLA+ action -> Go method -> PostgreSQL transaction -> database invariant ->
  test/fault-injection point`.
- `TaskGate.tla` remains the compatibility core model for one task with
  approval replay, narrowing, reservation, duplicate request handling,
  settlement, definite pre-connector release, conservative indeterminate
  recovery, revocation, expiry, completion, rejection, and catalog drift.
- `VectorBudget.tla` now checks the query, row, and DBMS budget vector,
  bounded settlement, zero-charge release, full-reservation indeterminate
  settlement, per-dimension `used + reserved <= limit`, no reservation after
  archival, and hard-limit archival liveness under weak fairness.
- `SQLAuthorization.tla` is a split finite two-product model for
  product-column provenance, qualified and unqualified column binding,
  constant-expression safe lists, and multi-product function/operator
  intersections.
- `MultiTaskAudit.tla` models finite two-task interleavings, one in-flight
  request per task, a global audit sequence/head, terminal audit events, and
  revoke/expiry races that block new requests without erasing in-flight ones.
- `ReceiptAudit.tla` checks persisted terminal receipt semantics: charge
  bounded by reservation, before/after budget balance, status-specific
  result/error fields, receipt existence for terminal records, and binding to
  the terminal audit sequence/hash.
- `RecoveryLiveness.tla` checks that finite recovering requests eventually
  become terminal under weak fairness for the recovery step.
- `formal/run.sh` now runs all six submitted configs. Generated paper artifacts
  independently verify each result JSON against its model/config/log hashes and
  parse the raw TLC log for no-error completion and final state/depth counts.
- Stale partial security evidence is still rendered as `not_measured` during
  artifact generation instead of being promoted into paper-facing counts.

Validation commands and results:

```text
sh -n formal/run.sh
```

Result: no output.

```text
./formal/run.sh
```

Result: passed under the approved Docker command prefix. TLC 1.7.1 checked:
`TaskGate.tla` at 14,824,257 states generated, 3,255,552 distinct states,
depth 18; `VectorBudget.tla` at 263,229 states generated, 201,134 distinct
states, depth 6; `SQLAuthorization.tla` at 3,073 states generated, 2,561
distinct states, depth 2; `MultiTaskAudit.tla` at 129,103 states generated,
129,103 distinct states, depth 7; `ReceiptAudit.tla` at 3,281 states
generated, 3,281 distinct states, depth 3; and `RecoveryLiveness.tla` at 221
states generated, 135 distinct states, depth 7. Intermediate failed attempts
while developing the new models were not claimed as evidence: one invalid TLA
config function literal, one oversized SQL-authorization request set that
exhausted TLC disk queue, one helper-order semantic error, and one dependent
quantifier-scope error.

```text
python3 evaluation/plots/generate.py --root . --raw-root evaluation/raw --output evaluation/generated --allow-empty
```

Result: passed. The generator selected no completed benchmark run, kept
performance `not_measured`, rejected stale security evidence as `not_measured`,
and wrote `evaluation/generated/paper-results.json` with the five split models
under `formal.additional_results`.

```text
python3 paper/tdsc/generate_tables.py
```

Result: passed with no output. The generated formal table reports all six TLC
models as independently verified from their raw logs. The generated artifact
table reports `make formal` as `passed (6 models)`.

```text
sha256sum formal/TaskGate.tla formal/TaskGate.cfg formal/results/tlc.log formal/results/tlc.json formal/VectorBudget.tla formal/VectorBudget.cfg formal/results/vector_budget.log formal/results/vector_budget.json formal/SQLAuthorization.tla formal/SQLAuthorization.cfg formal/results/sql_authorization.log formal/results/sql_authorization.json formal/MultiTaskAudit.tla formal/MultiTaskAudit.cfg formal/results/multi_task_audit.log formal/results/multi_task_audit.json formal/ReceiptAudit.tla formal/ReceiptAudit.cfg formal/results/receipt_audit.log formal/results/receipt_audit.json formal/RecoveryLiveness.tla formal/RecoveryLiveness.cfg formal/results/recovery_liveness.log formal/results/recovery_liveness.json formal/run.sh formal/REFINEMENT.md formal/README.md formal/results/.gitignore formal/results/README.md evaluation/generated/paper-results.json paper/tdsc/generated/formal_status.tex paper/tdsc/generated/security_status.tex paper/tdsc/generated/artifact_status.tex evaluation/plots/generate.py paper/tdsc/generate_tables.py paper/tdsc/main.tex
```

Result:

```text
bb9f4b3698d55646bcb4e1fa37d194a4947f82a7fc7e1704fc96fc663decf2ea  formal/TaskGate.tla
40e9058a0e786fb462b42daace65de014e605b5c6688d943e1703992eb1b4491  formal/TaskGate.cfg
292bc0bc9c27107823d87977db271c5cff5da46aab87081697326765f27c25d4  formal/results/tlc.log
c67894fc4e88dd32d27d875a4166cd94d5ee24efcc924abcce1c8c4579059d6d  formal/results/tlc.json
884d7499ad0b9fc826add70e35bd14c747cd6dd928c4e0f154f5872bbd49f40b  formal/VectorBudget.tla
875202e13f08e13a5075e613bee9256b835b1bb3f610166dc8b1d7667ac3b575  formal/VectorBudget.cfg
f52f007d83f024115c9ba46b8fb064b16e96ae4c12172b994dd1d635c7c17ace  formal/results/vector_budget.log
2a4921c039e82be5a07c0ecca822ef8dbe45866b65ba17c8fb2c0d0f29f01bf9  formal/results/vector_budget.json
2ecc2f8b11f4db5ec10314a0c188b04f8b324893c3e3c222943791986232fa16  formal/SQLAuthorization.tla
0249f031dca7aa70fd3a5201160d1a7b0c09d67c3e2e62292e9fdcd1468b97a2  formal/SQLAuthorization.cfg
6d7d58e61554216e5f3932bce72d3da15bc8fbbc9b9179b173c88fa37755675f  formal/results/sql_authorization.log
c266b4577a50e21b9b1f5d3b6b360d831e52d3fdb7e4b1a36cbc373e8306756b  formal/results/sql_authorization.json
a6bfc67ad5b45247f5527cf096889c5f32b2a3dd1c86865c8aed5a4e0270c115  formal/MultiTaskAudit.tla
c5178b5241f7478ab8b35a5a0a9d01a6197548de2d86647282793a76ee96f0f6  formal/MultiTaskAudit.cfg
b3a7f1f11f5939b782b2c03d7a2460b477d1976b4746752e7c75f9624270b426  formal/results/multi_task_audit.log
4b4df5455e66b78a295a8310764fd7d8efcf328e3db7dc55d41313c511e51b0d  formal/results/multi_task_audit.json
1cbf10f9304175e235a84b28d8fc318b5ea8e1961411d44665aca154dd329477  formal/ReceiptAudit.tla
b3998ffda8898323842e4c89a360f1f1971dad8e2d3954486841cb0dcca646f7  formal/ReceiptAudit.cfg
d45e26336d0289d5a3b1d1c3003ec771cc4e8ae0230d749fbac8684c2952ad4b  formal/results/receipt_audit.log
d5212d116ee691ee4eef3b52144d4971d9d2f6f078a89d5396ab4adb3f60cd3a  formal/results/receipt_audit.json
399f98b630e77f2ae8e6cb8311d36e36d12a7fec0794b0d592e3b7d1844d3536  formal/RecoveryLiveness.tla
d912f683839f265736540945cf08303e6830f1c780446b43217c4e68e27d754d  formal/RecoveryLiveness.cfg
a237cba7c4c125298fcaeb64ed694d8014cef1b7aaeacf9a2f39c7e9a712c1d0  formal/results/recovery_liveness.log
fe0f1e470fe680327deb1fe51fb475a79b0d14ef312a4a610b546a3c2fab0279  formal/results/recovery_liveness.json
98dfd9de68fcd76549251a893f15441bb4fbb72f676edde0691ac32d04126e21  formal/run.sh
12f6be591a11839c93b559652fcf63c8c59d4a13b1578fe491a4c67a234d59de  formal/REFINEMENT.md
7265a84e3a64818349494f81155d0733328c8f5f52e03ec7b7e0d1d196f55422  formal/README.md
de0c823c7d6b18a9d6748efd33f7db8f163aa055e091299ddacfa32869607eb8  formal/results/.gitignore
75f822876f3c4223218409a0d33b006606754caeb7dec9d8b26e18c446abebaf  formal/results/README.md
3c7c0c0562e2af7374ea26c07f800bafe5b7fdadde3c95d0e804c9c3393a62ce  evaluation/generated/paper-results.json
c54c9b857a9e426e78f1f4ccda37a5d950dc48bc4fb15d970ee13d2fc9cbfa4d  paper/tdsc/generated/formal_status.tex
61a1ead560ee399c1899df292b6422267dfca23cb5de316c268c91f37e455290  paper/tdsc/generated/security_status.tex
4440e32cd4eb5b2320dca73e26373e1a0130b8bf5c2ba684a8c6e8ac090a2c74  paper/tdsc/generated/artifact_status.tex
c91aa62c6e4069e46e0420c419ebeb9d5c0ecf922e3c6574317c3b3dcf9268d0  evaluation/plots/generate.py
12f38aada901e52aed2b2b5fa618d9520717d76baf9c5c45aabd59207918bbad  paper/tdsc/generate_tables.py
47e0944cb4b2263d4d8e209aa07c5831106fa59c94fb115b21878ac50e4cdb44  paper/tdsc/main.tex
```

```text
python3 -c "import ast, pathlib; [ast.parse(pathlib.Path(p).read_text(), filename=p) for p in ('evaluation/plots/generate.py','paper/tdsc/generate_tables.py')]"
```

Result: no output.

```text
git diff --check
```

Result: no output.

```text
docker ps --filter name=taskgate- --format {{.Names}}
```

Result: no output.

Remaining Stage 4 risk:

- The formal suite is still a collection of finite split models. It does not
  compose SQL authorization, vector accounting, receipts, audit inclusion,
  result lifecycle, and recovery into one global state space.
- `SQLAuthorization.tla` abstracts parser output into finite query shapes; it
  does not model arbitrary PostgreSQL AST syntax, parser/deparser bugs, or all
  nested scope constructs.
- `ReceiptAudit.tla` abstracts signatures, JSON canonicalization bytes,
  keyring windows, and public verifier bundle distribution.
- `MultiTaskAudit.tla` abstracts audit hashes as sequence fields and does not
  prove external checkpoint-anchor WORM retention or trusted timestamps.
- `RecoveryLiveness.tla` assumes weak fairness for the recovery action; it is
  not a proof about OS scheduling, process supervisors, or repeated crashes.
- Result lifecycle and external KMS/HSM/WORM/transparency guarantees remain
  outside the formal model suite and are tracked under Stage 3/Stage 6.

## Stage 5 Evidence

Stage 5 framework milestone: evaluation framework provenance, baseline naming,
campaign admission, cache strategy support, and paper-facing statistics were
tightened. No performance, security, fuzz, or fault-injection acceptance
numbers were produced.

Modified files:

- `evaluation/.env.example`
- `evaluation/README.md`
- `evaluation/datasets/README.md`
- `evaluation/environment/reference.json`
- `evaluation/config/smoke.json`
- `evaluation/config/sf1.json`
- `evaluation/config/sf10.json`
- `evaluation/workloads/tpch/manifest.json`
- `evaluation/workloads/tpcds/manifest.json`
- `evaluation/cmd/runner/main.go`
- `evaluation/cmd/runner/main_test.go`
- `evaluation/plots/generate.py`
- `evaluation/plots/test_generate.py`
- `evaluation/generated/paper-results.json`
- `evaluation/generated/summary.csv`
- `evaluation/generated/performance-table.tex`
- `evaluation/generated/latency-p95.svg`
- `evaluation/generated/throughput.svg`
- `paper/tdsc/generate_tables.py`
- `paper/tdsc/generated/performance_status.tex`
- `paper/tdsc/generated/artifact_status.tex`
- `paper/tdsc/main.tex`
- `paper/tdsc/remediation.md`

Core design:

- The misleading `native_view_rls` baseline name was replaced by
  `native_view` in runner code, configs, workload manifests, generators,
  documentation, and paper method text. The framework no longer implies that
  this baseline is PostgreSQL RLS unless a future config implements and records
  a real RLS policy.
- Evaluation configs and workload manifests now record
  `workload_lineage=TPC-derived`. Full-run loading fails closed if the suite or
  workload manifest omits or disagrees with that lineage.
- The runner records `campaign_id`, `baseline_order_seed`,
  `ordering_strategy=seeded_random`, exact `cell_order`,
  `cache_strategy`, `task_concurrency_mode`, workload provenance, dataset
  manifest provenance, metrics-probe provenance, and environment-manifest
  path/SHA-256.
- The runner accepts exactly `cache_strategy=warm` or `cache_strategy=cold`.
  Cold configs must provide `cache_reset_env` entries for every
  experiment/baseline. The runner verifies those commands as executable files
  under `/workspace`, runs the selected command after warmup and immediately
  before measured timing for the cell, and records each command path/SHA-256 in
  `run.json`.
- Full configs require an `environment_manifest_env` variable. The referenced
  manifest must be under `/workspace`, must hash to the recorded SHA-256, and
  must contain schema-versioned `host`, `software`, `database`, and `datasets`
  sections.
- TaskGate task-pool validation supports `distinct_task` and `same_task`
  modes. Current submitted configs use `distinct_task`, preserving per-task
  serialization under concurrency.
- The runner computes canonicalized result hashes for every sample and rejects
  a campaign when semantically equivalent baseline variants produce unstable or
  mismatched results for a query/concurrency cell.
- The artifact generator can admit multiple independently sealed full
  campaigns. Each campaign still has to contain exactly the required SF1/SF10
  suite pair, a matching campaign manifest, a clean current revision, exact
  cell-order permutation, TPC-derived workload provenance, dataset/probe
  hashes, environment-manifest provenance, cold reset-command hashes when
  applicable, and declared query IDs.
- Performance summaries are grouped by
  query/baseline/concurrency/experiment instead of mixing query latencies.
  They add deterministic p50 bootstrap intervals, p50/p95 ratios against the
  direct baseline, per-query throughput, and p99 only when at least 10,000
  observations exist for that row.
- Generated paper tables now include Query, p50 with 95% bootstrap interval,
  p95/p99, and p50/direct fields. When no raw run is selected, the generated
  performance table remains visibly `not_measured` and states
  `no completed raw run selected`.

Validation commands and results:

```text
gofmt -w evaluation/cmd/runner/main.go evaluation/cmd/runner/main_test.go
```

Result: failed locally because `gofmt` was not installed in the host shell; no
formatting evidence was claimed from this failed command.

```text
docker run --rm -u 1000:1000 -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm gofmt -w evaluation/cmd/runner/main.go evaluation/cmd/runner/main_test.go
```

Result: passed. The runner tests include seeded cell ordering, distinct and
same-task task-pool validation, and the cold-cache `cache_reset_env` config
gate.

```text
docker run --rm -v /home/wmm/agent-scope/task_gateway:/src -w /src golang:1.25-bookworm go test ./evaluation/cmd/runner
```

Result: passed.

```text
python3 -m unittest test_generate.py
```

Result: passed from `evaluation/plots` (`Ran 3 tests`, `OK`). A prior
repository-root invocation of `python3 -m unittest evaluation/plots/test_generate.py`
failed because that unittest path does not put `evaluation/plots` on
`sys.path` for `import generate`; it was not a generator failure.

```text
python3 -c "import ast, pathlib; [ast.parse(pathlib.Path(p).read_text(), filename=p) for p in ('evaluation/plots/generate.py','paper/tdsc/generate_tables.py')]"
```

Result: no output.

```text
sh -n evaluation/run.sh
```

Result: no output.

```text
./evaluation/run.sh validate
```

Result: passed. The wrapper built `taskgate-evaluation:local` and validated:
`taskgate-four-baseline-smoke` with one experiment, four baselines, and three
measured runs per worker; `taskgate-sf1-four-baseline` with two experiments,
four baselines, and 30 measured runs per worker; and
`taskgate-sf10-four-baseline` with two experiments, four baselines, and 30
measured runs per worker.

```text
python3 evaluation/plots/generate.py --root . --raw-root evaluation/raw --output evaluation/generated --allow-empty
```

Result: passed. No completed raw run was selected; the generated performance
object remains `not_measured` with note `no completed raw run selected`.

```text
python3 paper/tdsc/generate_tables.py
```

Result: passed with no output.

```text
rg -n "native_view_rls|View/RLS|same query texts|standard TPC-H/TPC-DS result" evaluation paper/tdsc README.md docs --glob '!paper/tdsc/remediation.md'
```

Result: no matches outside this remediation log.

```text
sha256sum evaluation/.env.example evaluation/README.md evaluation/datasets/README.md evaluation/environment/reference.json evaluation/config/smoke.json evaluation/config/sf1.json evaluation/config/sf10.json evaluation/workloads/tpch/manifest.json evaluation/workloads/tpcds/manifest.json evaluation/cmd/runner/main.go evaluation/cmd/runner/main_test.go evaluation/plots/generate.py evaluation/plots/test_generate.py evaluation/generated/paper-results.json evaluation/generated/summary.csv evaluation/generated/performance-table.tex evaluation/generated/latency-p95.svg evaluation/generated/throughput.svg paper/tdsc/generate_tables.py paper/tdsc/generated/performance_status.tex paper/tdsc/generated/artifact_status.tex paper/tdsc/main.tex
```

Result:

```text
b4229c19b3640add754e5119a43664eb7d91cbe6c3772cd207edfc8457af2704  evaluation/.env.example
ee4fe31c7da0081df6985bffdc26139741923bd0aeca3c3e61a134b8ebf106de  evaluation/README.md
2d486447363e5de5ddd4a5d9963a39c1481f448eeeca29a49763c38ac9d1e2a3  evaluation/datasets/README.md
c9493bb127cc76f4e0fc192865a41ef4c66db72c885ffc0972587f389ad1cfe2  evaluation/environment/reference.json
8a4c121ae6639f437e1412d41763482b3a4c6d032f8249455db925f0b866e892  evaluation/config/smoke.json
b9b9b7044c738e911c6a77f88e4847d1f172dce3fdce36c9ca22724a5b4db1e3  evaluation/config/sf1.json
387054e036eac64745a012b77d4314c4163d0b157a3d813d6c5ebcf63a083263  evaluation/config/sf10.json
a48a4748a609ab8eebb44c6fd4aed90a0ed4f94e25aca1481c18c0432efe6307  evaluation/workloads/tpch/manifest.json
85ac0dbcf4143d676275539c1c448b5d13927db532225d2353df993a287cf7f8  evaluation/workloads/tpcds/manifest.json
d25407e71265fa2d310a9eed55083d48667487bfb7af7088da4508e0191193bb  evaluation/cmd/runner/main.go
eea82b699cb57843e848e197e0d424aada27a035c54206932a28447f24d50052  evaluation/cmd/runner/main_test.go
06ac22fe2c887db2ad608b9765caad5c4c445dfff51ae09ff7ab4607344b93d2  evaluation/plots/generate.py
165f0b31164411e924e7cbf01daefaac0cfa2e2124fb6c3be64ca33c0d618bf3  evaluation/plots/test_generate.py
38d5e176ecb9913fdbb6f8e80c82a1b67148f88063676c4687c72721bdac8826  evaluation/generated/paper-results.json
844e760c81bff4a36452c2e43fc567a2eea17892622f43ea087281b2aaf145a9  evaluation/generated/summary.csv
c31fddce49b19685765cc2c0229516454c200975b80336329a92df61d071662d  evaluation/generated/performance-table.tex
a7cec0a2a987504d46d35255b65499d9a33c3a3b259d4ce6c30a6748764535aa  evaluation/generated/latency-p95.svg
cab8a6e658c0e1add6c93e2970bb24a875f29cfc0dfa78c0610d495ead895cb2  evaluation/generated/throughput.svg
1a058a1cbdb83d798fb27594efc7aa0180480b8fa3492a1807df6d7bfe54b3b4  paper/tdsc/generate_tables.py
644082803014cae7534e4f11969d5ca50607a2d3a37f3437e506a6d383250dc2  paper/tdsc/generated/performance_status.tex
eb49c2272d607be33598bd21dc3d286b1b3b61d3edf83bc9731b63027b3ab479  paper/tdsc/generated/artifact_status.tex
af86c65732b97d50c11c68cb5f12d0999ef9598abb325f44a8f2e3b54b9feb20  paper/tdsc/main.tex
```

Carry-forward risk for Stage 6:

- No full smoke/performance/security/fault campaign was run, and no acceptance
  latency, throughput, connector-crossing, budget-fault, or fuzz result is
  claimed.
- Cold-cache support is a cell-level reset hook and has not been exercised
  against real deployments. Any claim that needs per-observation cold cache
  still needs a separately defined method and raw evidence.
- The environment-manifest validator checks path, schema version, digest, and
  required top-level sections; it does not prove the semantic correctness of
  every host, software, database, or dataset field.
- PostgreSQL RLS is still not implemented as a baseline. The framework now
  reports `native_view`; a future RLS baseline must add real database policies
  and separate provenance before any RLS claim is made.
- Campaign aggregation has unit-fixture coverage and strict manifest checks,
  but has not yet been exercised against real full raw campaigns.

## Phase Status

| Phase | Status | Gate before next claim |
|---|---|---|
| 0 - baseline | Complete | Tracker exists, references current HEAD, preserves original worktree state, and does not claim reproduced experiments. |
| 1 - SQL authorization semantics | Complete | P0-001 closed with product-indexed SQL policy, corpus additions, gateway connector-boundary test, and updated SQL-policy wording. |
| 2 - Catalog/datasource binding | Complete | P0-002 closed with datasource attestation, schema-digest binding, receipt/control propagation, and current Go test evidence. |
| 3 - receipt/audit/lifecycle | In progress | Atomic terminal receipt persistence, terminal query-record immutability, receipt budget/status semantic checks, V3 signed-at receipts, keyring verification, public query-receipt verifier bundle publication, cursor-based receipt listing, independently verified audit inclusion paths, signed external audit-checkpoint anchor export, revoked/expired result lifecycle, ciphertext purge, result key-ID erasure enforcement, TTL scheduling, admin retention/key-erasure endpoints, legal hold workflow, and disabled-principal denial are implemented and tested. P0-003 remains open until centralized KMS/HSM-backed key custody, actual result-key material destruction, revocation transparency, external verifier transparency logs, and external anchor sink WORM/timestamp/retention guarantees are complete. |
| 4 - formal model expansion | Complete | Six finite split models and fresh TLC logs/hashes cover core task lifecycle, product-column authorization, vector hard-limit archival, multi-task audit order and revoke/expiry races, terminal receipt/audit consistency, and recovery liveness. `formal/REFINEMENT.md` maps current TLA+ actions to Go methods, PostgreSQL transactions/invariants, and tests. The remaining limitation is finite audit-map evidence, not a mechanized Go/PostgreSQL refinement proof or single composed full-system proof. |
| 5 - evaluation framework | Complete | Baseline naming, TPC-derived lineage, seeded cell order, campaign ID, warm/cold cache strategy support, distinct/same task-mode validation, environment/dataset/probe provenance, result-equivalence hashing, multi-campaign admission, per-query statistics, bootstrap p50 CI, direct ratios, and p99 withholding are implemented and focused tests/config validation passed. Real full campaigns and any desired PostgreSQL RLS baseline move to Stage 6 evidence production. |
| 6 - evidence production | Not started | Requires smoke gates after Phases 1-5. Do not estimate missing results. |
| 7 - paper rewrite | Not started | Rewrite only from completed implementation and real evidence. |
| 8 - final submission gate | Not started | Independent static review after all gates above pass. |

## Stage 0 Acceptance Checklist

- [x] Current HEAD recorded.
- [x] Initial worktree status recorded as clean.
- [x] Existing generated results recorded as archived evidence, not current HEAD reproduction.
- [x] P0 issues have owner files and concrete close conditions.
- [x] Claim groups classified as evidence-backed, mechanism-only, unmeasured, or paper-implementation inconsistent.
- [x] No formal experiment, full benchmark, long fuzz, or fault-injection campaign was run.
- [x] No commit or push was performed; the remediation plan says not to commit or push unless requested.
