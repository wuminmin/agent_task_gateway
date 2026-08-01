# TaskGate Refinement Map

This file maps the current finite TLA+ abstraction to implementation surfaces
that exist in this repository. It is evidence for Stage 4 traceability, not a
claim that the Go implementation has been mechanically proved to refine the
model.

Current formal status:

- Core model: `formal/TaskGate.tla`
- Core config: `formal/TaskGate.cfg`
- Vector-budget model: `formal/VectorBudget.tla`
- Vector-budget config: `formal/VectorBudget.cfg`
- SQL authorization model: `formal/SQLAuthorization.tla`
- SQL authorization config: `formal/SQLAuthorization.cfg`
- Multi-task audit model: `formal/MultiTaskAudit.tla`
- Multi-task audit config: `formal/MultiTaskAudit.cfg`
- Receipt/audit model: `formal/ReceiptAudit.tla`
- Receipt/audit config: `formal/ReceiptAudit.cfg`
- Recovery liveness model: `formal/RecoveryLiveness.tla`
- Recovery liveness config: `formal/RecoveryLiveness.cfg`
- Root-family exposure model: `formal/ExposureLedger.tla`
- Root-family exposure config: `formal/ExposureLedger.cfg`
- Artifact publication model: `formal/ArtifactPublication.tla`
- Artifact publication config: `formal/ArtifactPublication.cfg`
- V4 ordinal/bitmap refinement model: `formal/ExposureBitmapRefinement.tla`
- V4 ordinal/bitmap refinement config: `formal/ExposureBitmapRefinement.cfg`
- Abstract V5 Outcome-set settlement model: `formal/OutcomeHashSetRefinement.tla`
- Abstract V5 Outcome-set settlement config: `formal/OutcomeHashSetRefinement.cfg`
- Primary result: `formal/results/tlc.json`
- Vector-budget result: `formal/results/vector_budget.json`
- SQL authorization result: `formal/results/sql_authorization.json`
- Multi-task audit result: `formal/results/multi_task_audit.json`
- Receipt/audit result: `formal/results/receipt_audit.json`
- Recovery liveness result: `formal/results/recovery_liveness.json`
- Root-family exposure result: `formal/results/exposure_ledger.json`
- Artifact publication result: `formal/results/artifact_publication.json`
- V4 ordinal/bitmap result: `formal/results/exposure_bitmap_refinement.json`
- Abstract V5 Outcome-set settlement result: `formal/results/outcome_hash_set_refinement.json`
- Archived tool: TLC 1.7.1
- Latest core TLC result: passed at 2026-07-25T03:38:31Z with 14,824,257
  states generated, 3,255,552 distinct states, and depth 18.
- Latest vector-budget TLC result: passed at 2026-07-25T03:38:39Z with
  263,229 states generated, 201,134 distinct states, and depth 6.
- Latest SQL authorization TLC result: passed at 2026-07-25T03:38:40Z with
  3,073 states generated, 2,561 distinct states, and depth 2.
- Latest multi-task audit TLC result: passed at 2026-07-25T03:38:42Z with
  129,103 states generated, 129,103 distinct states, and depth 7.
- Latest receipt/audit TLC result: passed at 2026-07-25T03:38:43Z with
  3,281 states generated, 3,281 distinct states, and depth 3.
- Latest recovery-liveness TLC result: passed at 2026-07-25T03:38:44Z with
  221 states generated, 135 distinct states, and depth 7.
- Latest root-family exposure result: passed at 2026-08-01T06:27:34Z with
  410,766 states generated, 148,706 distinct states, and depth 12.
- Latest artifact-publication result: passed at 2026-08-01T08:01:51Z with
  1,497 states generated, 484 distinct states, and depth 15.
- Latest V4 ordinal/bitmap refinement result: passed at
  2026-08-01T06:27:37Z with 122,976 states generated, 60,680 distinct states,
  and depth 9.
- Latest abstract V5 Outcome-set settlement result: passed at
  2026-08-01T08:01:54Z with 20 states generated, 10 distinct states, and
  depth 4.
- Scope: one core task, finite request/receipt sets, abstract
  relation/column/scope sets, explicit crash transitions, a separate finite
  vector-budget ledger over query, row, and DBMS dimensions, a finite
  two-product SQL authorization abstraction, finite two-task audit
  interleavings, finite terminal receipt/audit consistency, and finite
  recovery liveness under weak fairness. The exposure split adds two requests,
  one root and one child task, finite release/influence/outcome fact universes, and
  bounded terminal replay. The V4 refinement model separately supplies a finite
  FactID--ordinal bijection, segmented exact bitmaps, one root epoch, CAS refresh,
  and committed-observation replay. The abstract V5 Outcome model checks exact
  hash-set difference/union, no double charge, budget/replay safety, and
  fail-closed collision/corruption states; it does not represent physical
  radix partitions or object reuse. The artifact model separates accounted
  settlement from later verified availability, exercises failure/mismatch/retry,
  and permits recovery without re-execution.

The model intentionally abstracts away SQL parser byte-level syntax, full AST
coverage, cryptographic signatures, PostgreSQL row contents, network I/O,
result ciphertext, external timestamp/WORM storage, and wall-clock scheduling.
The implementation evidence below therefore supports a refinement audit only
at the named abstraction boundaries.

## State Mapping

| TLA+ state | Go/domain state | Control PostgreSQL state |
|---|---|---|
| `taskState = "PENDING"` | `TaskAwaitingSubmission` or `TaskAwaitingApproval` before durable grant activation | `tasks.state IN ('AWAITING_SUBMISSION','AWAITING_APPROVAL')`, no active `budget_ledger` requirement |
| `taskState = "ACTIVE"` | `TaskActive` after approval callback applies a narrowing grant | `tasks.state='ACTIVE'`, `task_grants` row exists, `budget_ledger` row exists |
| `taskState IN {"REVOKED","EXPIRED","COMPLETED","FAILED"}` | `TaskArchived` with `terminal_reason` `revoked`, `expired`, `completed`, `rejected`, `budget_exhausted`, or `failed` | `tasks.state='ARCHIVED'`, terminal reason stored in `tasks.terminal_reason` |
| `approvalValid` | Accepted OA callback with valid HMAC, state/context checks, and signed approval receipt | Completed `callback_idempotency` row, `approval_events` row, immutable `task_grants` row |
| `catalogMatches` | Catalog version and live datasource/schema evidence still match the request | `query_records.catalog_version`, `catalog_digest`, `datasource_id`, `schema_digest`, plus connector readiness attestation |
| `grantHistory` | Domain narrowing of requested manifest and issued grant | `task_grants.approved_products_json`, `approved_columns_json`, `mandatory_scope_json`, budget columns |
| `processedReceipts` | Callback event or receipt idempotency | `callback_idempotency.event_id`, `approval_events.event_id` |
| `used` and `reserved` | One budget dimension in `BudgetUsage` | One of the triplets `used_*` and `reserved_*` in `budget_ledger` |
| `VectorBudget.used` and `VectorBudget.reserved` | Full `BudgetUsage` vector | `used_queries`, `used_rows`, `used_db_ms`, `reserved_queries`, `reserved_rows`, `reserved_db_ms` |
| `requestState[request] = "NEW"` | No durable query record for `(task_id, request_id)` | No row in `query_records` for the unique `(task_id, request_id)` |
| `requestState[request] = "RESERVED"` | Budget reservation exists and connector may not yet have produced a terminal outcome | `query_records.status='RESERVED'`, corresponding `reserved_*` ledger values are nonzero |
| `requestState[request] = "SETTLED"` | Successful completed query | `query_records.status='COMPLETED'`, `query_receipts` may be persisted in the same transaction on Gateway paths |
| `requestState[request] = "RELEASED"` | Definite pre-execution failure or policy failure after reservation release | `query_records.status='RELEASED'`, reservation removed and no budget charged |
| `requestState[request] = "INDETERMINATE"` | Ambiguous connector/gateway outcome charged conservatively | `query_records.status='INDETERMINATE'`, full reservation charged during runtime handling or startup recovery |
| `executionCount[request]` | Connector invocation count for an idempotency key | Enforced indirectly by `(task_id, request_id)` and terminal replay behavior; not stored as a counter |
| `startedRequests` | Request IDs that have acquired a reservation | Rows in `query_records` for a task |
| `SQLAuthorization.requestSourceProducts` | Product provenance attached to accepted SQL expressions and derived columns | Product/column allowlists from Catalog-derived grant and SQL-policy scope binding |
| `MultiTaskAudit.auditLog` and `auditHead` | Control audit event stream and current checkpoint | `audit_events.sequence`, `audit_events.previous_hash`, `audit_events.current_hash`, and `audit_chain_head` |
| `MultiTaskAudit.terminalSnapshot` | Set of request IDs already started when a task is revoked or expires | Existing `query_records` for the task remain terminal-settleable; `tasks.state='ARCHIVED'` blocks new reservations |
| `ReceiptAudit.receiptPersisted` | Immutable terminal query receipt exists | `query_receipts` row keyed by `query_id` with canonical receipt bytes and signature |
| `ReceiptAudit.receiptAuditSeq` and `receiptAuditHash` | Receipt fields naming the terminal audit event | `query_receipts.terminal_audit_sequence` and `terminal_audit_hash` |
| `RecoveryLiveness.requestState = "RECOVERING"` | Durable or in-memory settlement retry remains pending | Startup recovery over durable `RESERVED` records or Gateway background retry after finalization failure |
| `ExposureLedger.knownRelease`, `knownInfluence`, and `knownOutcome` | Root-family accounted facts and usage | Immutable `exposure_facts` rows partitioned by `ledger_kind`, plus the three `exposure_ledgers.used_*_facts` counters |
| `ExposureLedger.requestTask` | Root or delegated task that issued a request | `query_exposure_reservations.task_id` and `root_task_id`; every family member resolves to the same root ledger |
| `ExposureLedger.requestState = "RESERVED"` | Exposure evidence is required for the query | `query_exposure_reservations.status='RESERVED'` created with the ordinary resource reservation |
| `ExposureLedger.requestState IN {"BUFFERED","DERIVED"}` | Business result and companion provenance have executed but remain internal | Gateway memory between `QueryPair` and `FinalizeQuery`; these transient states are intentionally not durable database states |
| `ExposureLedger.requestState = "SETTLED"` | Novel facts were atomically accepted and accounted | Exposure reservation and root ledger updated with terminal query evidence, V7 settlement receipt, and PENDING artifact intent |
| `ExposureLedger.requestState = "REJECTED"` | Actual novel exposure exceeded a root limit | Transaction rolls back fact/head and PENDING metadata; any private staging object is unavailable and deleted/reconciled; Gateway returns `exposure_budget_exhausted` |
| `ExposureLedger.accounted` | Ledger settlement has committed | `query_records.status='COMPLETED'` and exposure reservation is `SETTLED`; this does not imply artifact availability |
| `ArtifactPublication.artifactState = "STAGED"` | Encrypted Parquet exists only at a private staging key | No result-artifact row is AVAILABLE and staging is not a read path |
| `ArtifactPublication.artifactState = "PENDING"` | Accounting committed with recoverable publication intent | `result_artifacts.status='PENDING'`, V7 receipt exists, canonical object is not yet consumable |
| `ArtifactPublication.artifactState = "AVAILABLE"` | Logical release boundary after verified promotion | Canonical object hash matches committed metadata; `status='AVAILABLE'`, `consumed_at` and `QUERY_RESULT_CONSUMED` commit together |
| `ExposureBitmapRefinement.FactOf` | Immutable publication dictionary (`internal/ordinal`) | `ordinal_dictionary_segments/chunks` plus publication digest binding |
| `ExposureBitmapRefinement.head` and `rootEpoch` | `control.OrdinalRootHead` | Three set-manifest digests and one epoch in the ordinal root-head row |
| `ExposureBitmapRefinement.bitmapEffect` | `ordinal.BitmapSet` / `control.OrdinalHybridSet` | Immutable content-addressed containers plus sparse dynamic facts |
| `ExposureBitmapRefinement.knownFacts` | Decoded abstract root-family exposure state | Exact decode of the three current set manifests; not a second writable ledger |

## Action Mapping

| TLA+ action | Go boundary | PostgreSQL transaction or invariant | Test or fault-injection point | Refinement notes |
|---|---|---|---|---|
| `NarrowGrant` | `domain.CheckTaskGrantCoreNarrowing`, `gateway.Service.validateApprovedGrant`, `Store.PutGrant` | `task_grants` immutable trigger prevents later ordinary rewrites | `TestTaskGrantCoreV1CheckNarrowing`, `TestTaskGrantCoreV1RejectsEveryExpansionDimension`, `TestOACallbackNarrowingIsEnforcedBeforeGrantPersistence` | Model treats relations, columns, scopes, and one scalar limit as independent sets; implementation also binds products, datasource, schema, and vector budgets. |
| `ApproveFresh` | `Store.ApplyApprovalCallback`, `gateway.Service.OACallbackHandler` | `callback_idempotency` claim/complete transaction, `approval_events`, `task_grants`, `budget_ledger`, task transition to `ACTIVE` | `TestApprovalCallbackReplayAndConflict`, `TestOACallbackHMACSubmissionApprovalReplayAndBadSignature` | Model starts at `PENDING`; implementation separates draft submission, awaiting approval, and active task states. |
| `ReplayApproval` | `Store.LookupCallback`, `Store.ApplyApprovalCallback` replay branch | `callback_idempotency.event_id` primary key and payload digest binding | `TestApprovalCallbackReplayAndConflict`, `TestLookupCallbackIsReadOnlyAcrossStatuses` | Model tracks receipt effects; implementation binds event ID and raw payload digest. |
| `RejectInvalidApproval` | `gateway.Service.OACallbackHandler`, approval receipt verifier, grant validator | Callback stays uncompleted or returns stable error; no grant/budget rows for invalid approval | `TestOACallbackHMACSubmissionApprovalReplayAndBadSignature`, `TestOACallbackRejectRequiresValidSignedReceipt`, `TestApprovalReceiptKeyringHonorsOverlapAndRetirement` | Model abstracts invalid approvals as bounded attempts, not specific signature/HMAC failure classes. |
| `ReserveQuery` | `gateway.Service.executeSQL`, `Store.ReserveBudget` | Transaction locks task and budget rows, checks active state/expiry/catalog version, enforces `reserved_queries=0`, inserts `query_records`, appends `QUERY_BUDGET_RESERVED` | `TestBudgetSerializationFinalizationAndAuditChain`, `TestRequestIDIsRequiredAndRetriesNeverExecuteTwice`, `TestNarrowedSignedGrantControlsReservationAndStatementTimeout` | Model requires relation/column/scope membership; implementation also enforces SQL policy and live datasource/schema evidence before reservation. |
| `DuplicateRequest` | `Store.ReserveBudget` replay path, `gateway.Service.queryReplayResponse` | Unique index on `(task_id, request_id)` and digest/actor comparison | `TestRequestIDReplayIsAtomicAndDigestBound`, `TestRequestIDIsRequiredAndRetriesNeverExecuteTwice` | Model permits only same-request replay; implementation rejects same request ID with different digest or actor. |
| `BeginConnector` | `gateway.Service.executeSQL` after successful reservation and before connector `Query` | No additional durable phase marker beyond `query_records.status='RESERVED'` | `TestDefinitePreExecutionConnectorFailureReleasesAndNeverRetriesRequestID`, `TestConnectorAmbiguityChargesReservationAndUsesStableCode` | The model has an explicit `EXECUTING` state. The implementation intentionally does not persist that boundary, so startup recovery treats durable `RESERVED` conservatively. |
| `ConnectorResult` | `dataconnector.Connector.Query` returns rows and DB time | Result is not terminal until budget/result/receipt transaction commits | `TestStructuredPlanAndDirectSQLKeepRawResultsOwnerOnly`, `TestQueryEncodingFailureSettlesActualUsage` | Model abstracts returned rows as `RESULT_KNOWN`; implementation encrypts result bytes before settlement. |
| `BeginSettle` | `Store.settleWithReceipt` or `Store.FinalizeQueryMeasuredWithReceipt` begins transaction | `beginTx`, `query_records FOR UPDATE`, `budget_ledger FOR UPDATE` | `TestTerminalSettlementWithReceiptPersistsAtomically` | Model separates begin-settle; implementation combines locking, budget update, result persistence, audit append, and optional receipt persistence in one transaction. |
| `SettleKnown` | `Store.FinalizeQueryMeasuredWithReceipt`, `Store.SettleBudgetWithReceipt`, retry settlement path | Charges bounded observed rows/DBMS, updates `query_records`, inserts `encrypted_query_results`, appends terminal audit, persists receipt on Gateway paths | `TestBudgetSerializationFinalizationAndAuditChain`, `TestTerminalSettlementWithReceiptPersistsAtomically`, `TestQueryFinalizationFailureSettlesActualUsage` | Model charges one scalar unit; implementation charges queries, rows, and DBMS with independent hard checks. |
| `DefinitePreConnectorFailure` | `gateway.Service.releaseQueryBudget` for known pre-execution failures after reservation | `ReleaseBudgetWithReceipt` removes reservation, charges zero, terminal status `RELEASED` | `TestDefinitePreExecutionConnectorFailureReleasesAndNeverRetriesRequestID`, `TestPolicyDenialFailsBeforeConnectorAndReservation` | Policy denials before reservation do not appear in the model because no request state changes. |
| `CrashFromDurableReservation` | `Store.recover` at startup for durable `RESERVED` query records | Recovery locks all `RESERVED` rows, charges full reservation, sets `INDETERMINATE`, appends audit, optionally persists receipt | `TestRestartRecoveryAndCallbackRetry`, `TestStartupRecoveryCanPersistIndeterminateReceipt`, `TestRecoveryChargesReservationAndArchivesAtHardLimit` | This is the implementation counterpart to the model's conservative durable-crash rule. |
| `CrashUnknownOutcome` | Runtime connector ambiguity handler `gateway.Service.markQueryIndeterminate` | `MarkIndeterminateWithReceipt` charges full reservation and makes request ID terminal | `TestConnectorAmbiguityChargesReservationAndUsesStableCode` | Model increments execution count; implementation records terminal status and stable error code. |
| `CrashWithKnownResult` | Finalization failure after connector result or after local result processing | Gateway background retry uses saved settlement intent; readiness fails while retry is pending | `TestFailedSettlementMakesServiceUnreadyUntilBackgroundRetrySucceeds`, `TestQueryEncodingFailureSettlesActualUsage`, `TestQueryFinalizationFailureSettlesActualUsage` | Model's `RECOVERING` is abstract; implementation has in-memory retry state and startup recovery for durable reservations. |
| `Revoke` | `gateway.Service.revokeTask`, `Store.TransitionTask` | Task row transition to `ARCHIVED` with `terminal_reason='revoked'`; existing results remain lifecycle-managed | `TestRevocationBlocksNewQueriesWithoutCancellingInFlightQuery`, `TestArchivedTaskResultsStayReadableUntilRetentionPurge` | Model only blocks new starts after terminal snapshot; implementation also preserves old result/receipt/audit access until purge or key erasure. |
| `Expire` | `Store.ReserveBudget` expiry check, `Store.recover`, `sweepExpired` | `archiveTaskTx` records `ARCHIVED` and `TASK_ARCHIVED` audit event | `TestGrantExpiryBoundsStatementAndQueryTimeoutsAndRejectsExpiredGrant`, `TestRecoveryChargesReservationAndArchivesAtHardLimit` | Model treats expiry as nondeterministic; implementation uses wall-clock checks and a periodic sweep. |
| `RejectTask` | OA rejection callback through `Store.ApplyApprovalCallback` and `Store.TransitionTask` | Task is archived with rejection/failed terminal reason; no active grant/budget for execution | `TestOACallbackRejectRequiresValidSignedReceipt`, domain task state-machine tests | Model names this `FAILED`; implementation distinguishes rejected, failed, and budget-exhausted terminal reasons. |
| `CompleteTask` | `gateway.Service.completeTask`, `Store.TransitionTask` | Transition only when task is active and no in-flight query remains at service level | `TestTaskStateMachineHappyPath`, Gateway task tests | Model requires `NoInFlight`; implementation relies on task/budget/query checks and service flow rather than a dedicated DB trigger for completion-with-reservation. |
| `CatalogDrift` | Connector readiness and per-query datasource/schema checks | Catalog version/digest and datasource/schema evidence are stored in grants, query records, audit payloads, and receipts | `TestSchemaDriftFailsQueryClosedBeforeReservation`, `TestDatasourceMismatchFailsQueryClosedBeforeReservation`, `TestLiveSchemaDigestDetectsViewDefinitionDrift` | Model collapses all drift into `catalogMatches=FALSE`; implementation has source ID, database, user, major version, and schema digest dimensions. |
| `VectorBudget.Reserve` | `Store.ReserveBudget` | Vector reservation increments `reserved_queries`, `reserved_rows`, and `reserved_db_ms` only if all dimensions remain within limits | `TestBudgetSerializationFinalizationAndAuditChain`, `TestMigrationsAndTaskRequestContext` | Split model removes task/SQL state and focuses on vector arithmetic. |
| `VectorBudget.Complete` | `Store.FinalizeQueryMeasuredWithReceipt`, `Store.SettleBudgetWithReceipt` | Charges a vector bounded by the reservation and clears the full reservation | `TestBudgetSerializationFinalizationAndAuditChain`, `TestTerminalSettlementWithReceiptPersistsAtomically` | Implementation always charges one query for completed execution and independently bounds row/DBMS charges. |
| `VectorBudget.FailPostExecution` | `Store.FailBudgetWithReceipt` | Charges bounded observed use and clears the reservation | `TestQueryEncodingFailureSettlesActualUsage`, `TestQueryFinalizationFailureSettlesActualUsage` | Failure semantics are post-execution; definite pre-execution errors use release. |
| `VectorBudget.Release` | `Store.ReleaseBudgetWithReceipt` | Clears the full reservation and charges zero | `TestDefinitePreExecutionConnectorFailureReleasesAndNeverRetriesRequestID` | Matches definite pre-connector failure and non-execution release paths. |
| `VectorBudget.MarkIndeterminate` | `Store.MarkIndeterminateWithReceipt`, `Store.recover` | Charges the full reservation vector and clears reservation | `TestConnectorAmbiguityChargesReservationAndUsesStableCode`, `TestStartupRecoveryCanPersistIndeterminateReceipt` | Covers runtime ambiguity and durable startup recovery accounting. |
| `VectorBudget.ArchiveHardLimit` | `Store.SettleBudgetWithReceipt`, `Store.MarkIndeterminateWithReceipt`, `Store.recover`, `archiveTaskTx` | When a settlement reaches a hard vector limit and no reservation remains, task archive state is recorded with terminal audit evidence | `TestRecoveryChargesReservationAndArchivesAtHardLimit`, budget hard-limit tests in `internal/control` | Split model checks eventual archival after hard limit under weak fairness; implementation performs archival inside settlement/recovery flows. |
| `SQLAuthorization.Accept` | `sqlpolicy.Authorize`, `sqlpolicy.walkSelect`, expression/function/operator checks | No Control PG write occurs before SQL authorization succeeds; accepted SQL later carries product/datasource evidence into `ReserveBudget` | `TestProductSpecificColumnAuthorization`, `TestProductSpecificFunctionAndOperatorAuthorization`, `TestProductIntersectionForCrossProductExpressions`, `TestDerivedColumnProvenance` | Split model abstracts parser output into constant, qualified, and unqualified query shapes. |
| `SQLAuthorization.Reject` | `sqlpolicy.Authorize`, Gateway pre-reservation policy denial | Query is rejected before connector invocation and before budget reservation when authorization fails | `TestAmbiguousColumnRejected`, `TestCTEShadowingDoesNotInheritOuterPermission`, SQL attack corpus tests | Model checks that invalid query shapes cannot enter the accepted state. |
| `MultiTaskAudit.Reserve` | `Store.ReserveBudget` | Task and budget rows are locked; `reserved_queries=0` is required per task; audit append locks global head | `TestBudgetSerializationFinalizationAndAuditChain`, `TestRequestIDReplayIsAtomicAndDigestBound` | Split model allows interleavings across tasks while preserving one in-flight request per task. |
| `MultiTaskAudit.Complete`, `Release`, `MarkIndeterminate` | Terminal settlement methods with receipt builders | Terminal query status and terminal audit event are written in one transaction | `TestTerminalSettlementWithReceiptPersistsAtomically`, `TestConnectorAmbiguityChargesReservationAndUsesStableCode`, `TestDefinitePreExecutionConnectorFailureReleasesAndNeverRetriesRequestID` | Model abstracts terminal event hash material but checks every terminal request has a terminal event in the global sequence. |
| `MultiTaskAudit.Revoke` and `Expire` | `Store.TransitionTask`, expiry checks, `sweepExpired` | Task archive transition appends audit evidence while existing query records remain inspectable/settleable | `TestRevocationBlocksNewQueriesWithoutCancellingInFlightQuery`, `TestArchivedTaskResultsStayReadableUntilRetentionPurge` | Split model explicitly allows the revoke/expiry race with in-flight requests and blocks only new starts. |
| `ReceiptAudit.Complete` | `Store.FinalizeQueryMeasuredWithReceipt` | Completed query record, result hash, terminal audit event, and query receipt persist atomically | `TestTerminalSettlementWithReceiptPersistsAtomically`, `TestQueryReceiptRejectsInvalidSemanticVectors` | Model checks result hash is present, error is absent, charge is bounded, and budget before/after balances. |
| `ReceiptAudit.Release` | `Store.ReleaseBudgetWithReceipt` | Reservation is cleared, zero charge is persisted, terminal audit event is bound to receipt | `TestDefinitePreExecutionConnectorFailureReleasesAndNeverRetriesRequestID`, receipt semantic verifier tests | Model checks zero charge, no result hash, and nonempty error evidence for release. |
| `ReceiptAudit.Fail` | `Store.FailBudgetWithReceipt` | Post-execution failure charges bounded observed use and persists terminal evidence | `TestQueryEncodingFailureSettlesActualUsage`, `TestQueryFinalizationFailureSettlesActualUsage` | Model checks failure has no result hash, has an error, and charges no more than reservation. |
| `ReceiptAudit.Indeterminate` | `Store.MarkIndeterminateWithReceipt`, `Store.recover` | Ambiguous outcome charges full reservation and binds receipt to terminal audit event | `TestConnectorAmbiguityChargesReservationAndUsesStableCode`, `TestStartupRecoveryCanPersistIndeterminateReceipt` | Model checks indeterminate charge equals reservation. |
| `RecoveryLiveness.RecoverStep` | `Store.recover`, Gateway settlement retry loop | Durable `RESERVED` rows are eventually made `INDETERMINATE`; known-result retry eventually completes when retry remains enabled | `TestStartupRecoveryCanPersistIndeterminateReceipt`, `TestFailedSettlementMakesServiceUnreadyUntilBackgroundRetrySucceeds` | Liveness is finite and assumes weak fairness for recovery execution; it is not a scheduler proof for arbitrary process crashes. |
| `ExposureLedger.Reserve` | `gateway.Service.executeSQL`, `Store.ReserveBudget` with `ExposureReservationRequest` | Inserts `query_records` and `query_exposure_reservations` in the resource-budget reservation transaction after resolving `tasks.root_task_id` | `TestExposurePlanHidesMeteringKeysAndDeduplicatesReplay`, `TestExposureTaskRejectsDirectSQLWithoutProvenance` | Estimated exposure is admission metadata; only actual novel FactIDs affect the root ledger. |
| `ExposureLedger.ExecuteAndBuffer` | `dataconnector.Connector.QueryPair` | Visible and provenance companion queries run in one read-only `REPEATABLE READ` business-database transaction; no Control PG result is yet committed | `TestExposurePlanHidesMeteringKeysAndDeduplicatesReplay`, live Compose acceptance | The model represents both result sets as fact sets and tracks physical execution separately from exposure charge. |
| `ExposureLedger.DeriveProvenance` | `planExposureContext.deriveObservationV2`, `AttachOutcomeV3`, `ValidateRelationV2`, `ObserveV2` | Hidden typed entity keys and source rows become V2 release/dependency sets; V3 binds the normalized proposition and released result into one outcome fact | V2 algebra/oracle tests; `TestExposurePlanHidesMeteringKeysAndDeduplicatesReplay`; `TestExposureV3ChargesDistinctZeroResultPredicates` | TLC assumes derivation exact; typed base semantics are in `docs/exposure-algebra-v2.md`, and the V3 wrapper is in `docs/exposure-accounting.md`. |
| `ExposureLedger.Settle` | V5 settlement inside `FinalizeOrdinalQueryArtifactMeasuredWithReceipt` | Locks the root head, checks all three limits, advances the three set digests, and commits terminal query/audit, V7 receipt, and PENDING artifact metadata atomically | `TestV5SettlementAndSemanticReplayPostgres`, `TestConcurrentTaskFamilySettlementCannotOverspend`, artifact Control tests | A row lock/CAS serializes concurrent family settlement; availability is a later transition. |
| `ExposureLedger.RejectOverBudget` | failed V5 settlement / finalization rollback | Conditional root-ledger update fails with `ErrExposureBudgetExhausted`; the surrounding transaction rolls back novel heads and PENDING metadata | `TestExposureOverBudgetDoesNotStoreOrChargeResult`, `TestExposureBudgetRejectsBufferedResultBeforeRelease` | Physical DB work and private staging can exist transiently, but no canonical object or AVAILABLE state is created. |
| `ExposureLedger.ReleaseBeforeExecution` | `releaseExposureReservationTx` inside resource reservation release | Marks the exposure reservation `RELEASED` and appends audit evidence without adding facts | definite pre-execution failure tests | This action excludes failures after visible data has been produced. |
| `ExposureLedger.Replay` | `queryReplayResponse` and immutable query result/receipt lookup | Unique `(task_id, request_id)` returns the terminal observation; no connector or settlement transaction is rerun | `TestRequestIDIsRequiredAndRetriesNeverExecuteTwice`, `TestExposurePlanHidesMeteringKeysAndDeduplicatesReplay` | Replay count is bounded only to keep the TLC state space finite. |
| `ExposureLedger.RevokeRoot` | `revokeTask`, ancestor checks in execution | Root archival blocks new descendant requests; already terminal request IDs remain observationally replayable | `TestDelegatedTaskSharesRootExposureAndStopsWithParent` | The split model tracks root activity but abstracts the full signed delegation chain. |
| `ArtifactPublication.Stage` | `gateway.Service.stageResultArtifact` | Encode/encrypt and upload to a randomized private staging key before Control settlement | result-artifact manager tests | STAGED bytes confer no logical availability. |
| `ArtifactPublication.Settle` | `FinalizeOrdinalQueryArtifactMeasuredWithReceipt` | One Control transaction commits ledgers, terminal query/audit, V7 receipt, expected object hash, and PENDING row | `TestArtifactPromotionFailurePreservesSettlementAndRecoversWithoutReexecution` | V7 is a settlement receipt, not an availability receipt. |
| `ArtifactPublication.Promote` | `gateway.Service.promoteResultArtifact`, `Store.MarkResultArtifactAvailable` | Copy to canonical key, verify digest, then atomically set AVAILABLE/`consumed_at` and append `QUERY_RESULT_CONSUMED` | `TestResultDeliveryCapabilityExpiresAndDownloadsDoNotMutateState`, Control artifact tests | The audit event proves logical availability; later downloads do not mutate state. |
| `ArtifactPublication.PromotionFail` / `PromotionHashMismatch` | failed copy or digest verification in `gateway.Service.promoteResultArtifact` | PENDING intent and settlement remain durable; no AVAILABLE transition occurs | `TestArtifactPromotionFailurePreservesSettlementAndRecoversWithoutReexecution`, artifact manager digest tests | Failure is explored as state, not erased by assigning the expected hash. |
| `ArtifactPublication.RetryPending` | promotion retry / reconciliation scheduling | Clears only transient failed-attempt state; the artifact remains PENDING | artifact recovery tests | No fairness is assumed, so the model makes no eventual-availability claim. |
| `ArtifactPublication.Recover` | `Service.ReconcilePendingArtifacts` and same-request replay | Reuses committed PENDING intent; no budget/exposure settlement and no connector invocation | `TestArtifactPromotionFailurePreservesSettlementAndRecoversWithoutReexecution`, `TestArtifactRecoveryGatesReadinessUntilFullPassCompletes` | Readiness remains closed until a full recovery pass succeeds. |

## V4 Ordinal Refinement Mapping

| Refinement action/invariant | Implementation boundary | Persistence/atomicity boundary | Evidence |
|---|---|---|---|
| Dictionary bijection / exact decode | `internal/ordinal` compiler, `Dictionary.Expand`, HOT/COLD parsers | Immutable publication, segment bounds, full hash/payload collision checks | `internal/ordinal` dictionary, artifact, persistence, and bitmap tests |
| Bitmap OR / ANDNOT / popcount | `ordinal.BitmapSet` | Canonical portable container bytes; non-canonical encodings rejected | `internal/ordinal/bitmap_test.go` |
| Streamed exact effect | `dataconnector.QueryPairStream`, Gateway ordinal derivation sink | No streamed prefix is publishable unless the Business transaction succeeds | Connector stream live tests and Gateway ordinal derivation tests |
| Prepare candidate at epoch | `settleOrdinalExposureMeasuredTx` | Read root head and build immutable candidate set manifests in one Control transaction | `TestOrdinalSettlementAndReferenceReplayAvoidsBitmapWork` |
| CAS all three dimensions | ordinal root-head conditional update | One epoch controls Release/Influence/Outcome; conflict recomputes, failure rolls back | `TestOrdinalRootSettlementSerializesConcurrentFamilyUpdates` |
| Exact committed-observation replay | ordinal observation reference settlement | Reference must be committed for the same root and dictionary set | `TestOrdinalReferenceMustAlreadyBeCommittedForSameRoot` |
| Fail-closed corruption/boundary | ordinal normalization and dictionary-set validation | No head/result/audit/receipt partial commit | out-of-bounds, cross-Catalog, collision, and over-budget ordinal tests |

## Database Invariants Used By The Mapping

| Invariant | Database mechanism |
|---|---|
| Budget cannot exceed limits | `budget_ledger` check constraints for each vector dimension plus settlement checks before update |
| At most one in-flight query per task | `ReserveBudget` locks the task and budget row, requires `reserved_queries=0`, and updates with `WHERE reserved_queries=0` |
| Request ID at-most-once | Unique index on `(task_id, request_id)` from migration `002_query_request_id.sql` plus digest/actor replay check |
| Terminal query evidence immutability | `query_records_terminal_no_update` and `query_records_no_delete` from migration `005_terminal_query_immutability.sql` |
| Grant immutability | `task_grants_no_update` and `task_grants_no_delete` triggers |
| Receipt immutability | `query_receipts_no_update` and `query_receipts_no_delete` triggers |
| Audit total order | Single `audit_chain_head` row locked `FOR UPDATE`, unique audit sequence/current hash |
| Result key erasure is durable evidence | `result_encryption_keys` `ACTIVE`/`ERASED` check constraints and no-reactivation trigger |
| Root-family exposure cannot exceed either limit | `exposure_ledgers` checks plus a `FOR UPDATE` root-ledger lock and conditional settlement update |
| A fact is charged at most once per root and ledger kind | `exposure_facts` primary key on `(root_task_id, ledger_kind, fact_sha256)` |
| Exposure facts are immutable | `exposure_facts_no_update` and `exposure_facts_no_delete` triggers |
| Delegated tasks share the root budget subject | signed `root_task_id`/`parent_task_id`, task foreign keys, delegation narrowing checks, and root-ledger lookup |
| Exposure settlement and publication intent commit together | V5 artifact finalization commits exposure heads, terminal query/audit, V7 receipt, expected object digest, and PENDING metadata in one Control PG transaction |
| Availability is subsequent and fail closed | `MarkResultArtifactAvailable` commits AVAILABLE, `consumed_at`, and `QUERY_RESULT_CONSUMED` only after canonical promotion and digest verification |

## Current Non-Refinement Gaps

These gaps are intentionally not hidden by the mapping:

1. `SQLAuthorization.tla` models a finite two-product authorization abstraction
   with constant, qualified, and unqualified query shapes. It does not model
   arbitrary PostgreSQL AST syntax, every lexical-scope construct, correlated
   subquery semantics, parser bugs, or deparser behavior.
2. `VectorBudget.tla` checks vector reserve/settle/release/indeterminate
   arithmetic and hard-limit archival under weak fairness, but it does not
   compose vector accounting with SQL, receipt bytes, result lifecycle, or
   external recovery scheduling.
3. `MultiTaskAudit.tla` checks finite two-task interleavings, per-task
   one-in-flight admission, revoke/expiry races, and global audit order. It
   abstracts audit hashes as ordered sequence fields rather than cryptographic
   hash bytes or external inclusion proofs.
4. `ReceiptAudit.tla` checks terminal receipt field semantics and terminal
   audit binding, but it does not model Ed25519 signatures, keyring
   valid-from/retirement windows, public verifier bundle distribution, or
   JSON canonicalization bytes.
5. `RecoveryLiveness.tla` checks finite recovery convergence under weak
   fairness for the recovery action. It is not a proof that an operating system,
   process supervisor, or background scheduler will always run.
6. Result lifecycle controls such as retention purge, legal hold, disabled
   principals, result key-ID erasure, and external audit anchor publication are
   implemented and tested outside the formal model suite.
7. The repository still does not provide centralized KMS/HSM key custody,
   actual result-key material destruction, external verifier transparency logs,
   or WORM/trusted-timestamp guarantees for audit anchors.
8. `ExposureLedger.tla` treats release, source-influence, and outcome observations as
   finite sets supplied by an exact derivation step. It does not model SQL
   parsing, row encoding, cryptographic FactID collisions, hash preimages, or
   malformed connector values. Those boundaries are specified in
   `docs/exposure-algebra-v2.md` and tested by the V2 corpus, but not
   mechanized as a Go refinement proof.
9. The exposure model has one root, one child, two requests, two facts per
   ledger, and one terminal replay in the checked configuration. It establishes
   the invariants only for that finite scope; larger delegation DAGs and
   workloads are implementation/evaluation evidence.
10. The online compiler supports one-product structured plans plus connected
    inner-equi-join graphs over 2--16 distinct Catalog-stable roles, with one or
    more column-to-column equalities per edge, and same-product union-distinct;
    either multi-input form may be grouped with `COUNT`/`SUM`/`MIN`/`MAX`. SQL aliases
    resolve to stable roles, and sorted graph nodes, edges, and predicates
    deterministically fold into the existing binary Join algebra. Within the
    16-source operational complexity/DoS ceiling the graph shape is otherwise
    unrestricted, including ten-source chains; the 1 MiB MCP request-body
    bound, AST validation, resource budgets, statement timeout, and row limits
    also apply. Arbitrary SQL, disconnected graphs, outer/cross/self/non-equality
    joins, union-all, and multi-input pagination remain unsupported and fail
    closed.

## Stage 4 Acceptance Impact

This file satisfies the traceability deliverable:

```text
TLA+ action -> Go method -> PostgreSQL transaction -> database invariant -> test/fault-injection point
```

Together with the submitted TLC JSON/log pairs, this provides finite-model
evidence for split product-column authorization, vector resource budgeting,
root-family three-dimensional exposure accounting, multi-task audit ordering, terminal
receipt/audit consistency, and recovery liveness. It remains finite-state
design evidence plus an audit map, not a mechanized Go/PostgreSQL refinement
proof.
